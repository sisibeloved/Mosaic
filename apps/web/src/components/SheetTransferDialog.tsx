// 表格手动交换对话框（RFC-0017 M2，副本语义）：导入模式（粘云端表格链接 →
// 预览摘要+损失清单 → 执行建本地新表）/ 导出模式（选 provider → 预览 →
// 执行建云端副本）。共享入口：DocsHomePage 新建对话框、DocsTab 附着、
// SheetEditorPage 顶栏「导出到云」。
import { useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  uuidv7,
  type ExtDocProviderID,
  type SheetTransferIssue,
  type SheetTransferPreview,
  type SheetTransferRecord,
  type SheetTransferUpdatePreview,
} from "../api/client";
import { refreshExtShares, useExtShares } from "../state/extdocs";
import { relativeTime } from "../lib/ui";

/** provider 展示名（与 ExtShareMenu 对齐）。 */
const PROVIDER_LABEL: Record<string, string> = {
  tencent_docs: "腾讯文档",
  feishu_docs: "飞书云文档",
};

/** 损失项 severity 徽标样式。 */
const SEVERITY_STYLE: Record<string, string> = {
  info: "bg-surface-3 text-dim",
  warning: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  blocked: "bg-danger/15 text-danger",
};
const SEVERITY_LABEL: Record<string, string> = { info: "注记", warning: "有损", blocked: "阻止" };
const ACTION_LABEL: Record<string, string> = {
  kept: "保留",
  converted: "转值",
  dropped: "不迁移",
  truncated: "截断",
};
const STATUS_LABEL: Record<string, string> = {
  running: "执行中",
  succeeded: "成功",
  succeeded_with_warnings: "成功（有损）",
  failed: "失败",
  unknown: "结果未知",
};

/** 转换报告列表（预览与执行结果共用）。 */
function IssueList({ issues }: { issues: SheetTransferIssue[] }) {
  if (issues.length === 0) {
    return <p className="text-xs text-faint">无转换损失——内容按原语义迁移。</p>;
  }
  return (
    <ul className="max-h-40 space-y-1 overflow-y-auto">
      {issues.map((is, i) => (
        <li key={i} className="flex items-start gap-1.5 text-[11px] leading-relaxed">
          <span className={`shrink-0 rounded px-1 py-0.5 ${SEVERITY_STYLE[is.severity] ?? SEVERITY_STYLE.info}`}>
            {SEVERITY_LABEL[is.severity] ?? is.severity}
          </span>
          <span className="min-w-0 flex-1 text-dim">
            {is.sheet && <span className="text-text">{is.sheet}：</span>}
            {is.reason}
            {is.action && <span className="text-faint">（{ACTION_LABEL[is.action] ?? is.action}）</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** 交换对话框。mode=import 时 onImported 携新 doc_id；export 需 docID。 */
export function SheetTransferDialog({
  mode,
  docID,
  onClose,
  onImported,
}: {
  mode: "import" | "export";
  docID?: string;
  onClose: () => void;
  onImported?: (docID: string) => void;
}) {
  const ext = useExtShares();
  const [url, setUrl] = useState("");
  const [provider, setProvider] = useState<string>("");
  const [preview, setPreview] = useState<SheetTransferPreview | null>(null);
  const [record, setRecord] = useState<SheetTransferRecord | null>(null);
  const [busy, setBusy] = useState<"idle" | "preview" | "execute">("idle");
  const [error, setError] = useState<string | null>(null);
  const [errorIssues, setErrorIssues] = useState<SheetTransferIssue[] | null>(null);
  // M3 回写：历史导出目标 → 差异预览 → 异步执行（轮询/取消/续跑）
  const [targets, setTargets] = useState<SheetTransferRecord[] | null>(null);
  const [updTarget, setUpdTarget] = useState<{ provider: ExtDocProviderID; url: string } | null>(null);
  const [updPreview, setUpdPreview] = useState<SheetTransferUpdatePreview | null>(null);
  const [live, setLive] = useState<SheetTransferRecord | null>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    void refreshExtShares();
  }, []);

  useEffect(() => {
    if (mode === "export" && !provider && ext.configuredProviders.size > 0) {
      setProvider([...ext.configuredProviders][0]);
    }
  }, [mode, provider, ext.configuredProviders]);

  // 历史导出目标（去重取最近；成功/有损成功的 external_url）
  useEffect(() => {
    if (mode !== "export" || !docID) return;
    api
      .listSheetTransfers({ doc_id: docID, direction: "export", limit: 50 })
      .then((r) => {
        const seen = new Set<string>();
        const out: SheetTransferRecord[] = [];
        for (const rec of r.transfers) {
          if (!rec.external_url || seen.has(rec.external_url)) continue;
          if (rec.status !== "succeeded" && rec.status !== "succeeded_with_warnings") continue;
          seen.add(rec.external_url);
          out.push(rec);
        }
        setTargets(out);
      })
      .catch(() => setTargets(null));
  }, [mode, docID]);

  // 回写进度轮询（running 期间 1s；终态/卸载即停）
  useEffect(() => {
    if (!live || live.status !== "running") return;
    const id = setInterval(() => {
      api
        .getSheetTransfer(live.transfer_id)
        .then((rec) => setLive(rec))
        .catch(() => undefined);
    }, 1000);
    pollRef.current = id;
    return () => clearInterval(id);
  }, [live]);
  useEffect(() => () => {
    if (pollRef.current) clearInterval(pollRef.current);
  }, []);

  const doPreview = async () => {
    setBusy("preview");
    setError(null);
    setErrorIssues(null);
    setPreview(null);
    try {
      const pv = mode === "import"
        ? await api.previewSheetTransfer({ direction: "import", url: url.trim() })
        : await api.previewSheetTransfer({ direction: "export", doc_id: docID, provider: provider as "tencent_docs" | "feishu_docs" });
      setPreview(pv);
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const doExecute = async () => {
    if (!preview) return;
    setBusy("execute");
    setError(null);
    setErrorIssues(null);
    try {
      const rec = mode === "import"
        ? await api.executeSheetTransfer({
            direction: "import",
            url: url.trim(),
            snapshot_hash: preview.snapshot_hash ?? undefined,
            idempotency_key: uuidv7(),
          })
        : await api.executeSheetTransfer({
            direction: "export",
            doc_id: docID,
            provider: provider as "tencent_docs" | "feishu_docs",
            source_version: preview.source_version ?? undefined,
            idempotency_key: uuidv7(),
          });
      setRecord(rec);
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const doUpdatePreview = async (t: { provider: ExtDocProviderID; url: string }) => {
    setUpdTarget(t);
    setUpdPreview(null);
    setLive(null);
    setBusy("preview");
    setError(null);
    setErrorIssues(null);
    try {
      const pv = await api.previewSheetTransferUpdate({
        doc_id: docID ?? "",
        provider: t.provider,
        external_url: t.url,
      });
      setUpdPreview(pv);
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const doUpdateExecute = async () => {
    if (!updPreview || !updTarget) return;
    setBusy("execute");
    setError(null);
    try {
      const rec = await api.executeSheetTransferUpdate({
        doc_id: updPreview.doc_id,
        provider: updTarget.provider,
        external_url: updTarget.url,
        target_hash: updPreview.target_hash,
        source_version: updPreview.source_version,
        idempotency_key: uuidv7(),
      });
      setLive(rec);
      if (rec.status !== "running") {
        setRecord(rec);
      }
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const doCancel = async () => {
    if (!live) return;
    setBusy("execute");
    try {
      setLive(await api.cancelSheetTransfer(live.transfer_id));
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const doResume = async () => {
    if (!live) return;
    setBusy("execute");
    setError(null);
    try {
      setLive(await api.resumeSheetTransfer(live.transfer_id));
    } catch (e) {
      applyError(e);
    } finally {
      setBusy("idle");
    }
  };

  const applyError = (e: unknown) => {
    if (e instanceof ApiError) {
      if (e.code === "sheet_transfer_blocked") {
        const payload = e.payload as { report?: SheetTransferIssue[] } | undefined;
        setErrorIssues(payload?.report ?? []);
        setError("预检存在阻止项（见下方清单）——请缩小范围或处理后重试。");
        return;
      }
      if (e.code === "sheet_transfer_stale_preview") {
        setError("源或目标已变化（预览过期）——请重新预览。");
        setPreview(null);
        setUpdPreview(null);
        return;
      }
      if (e.code === "sheet_transfer_target_changed") {
        setError("目标未写区域已被修改（可能有协作者在编辑）——请重新预览。");
        return;
      }
      if (e.code === "sheet_transfer_not_resumable") {
        setError(e.message);
        return;
      }
      if (e.code === "extdocs_reauth_required") {
        setError("平台授权已失效——请到设置页重新授权后再试。");
        return;
      }
      if (e.code === "extdocs_token_missing") {
        setError("平台未配置——请到设置页完成配置。");
        return;
      }
      setError(e.message);
      return;
    }
    setError(e instanceof Error ? e.message : String(e));
  };

  const configured = [...ext.configuredProviders];

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
      role="dialog"
      aria-modal="true"
      aria-label={mode === "import" ? "从云文档导入表格" : "导出表格到云"}
      onClick={() => busy === "idle" && onClose()}
      onKeyDown={(e) => {
        if (e.key === "Escape" && busy === "idle") onClose();
      }}
    >
      <div
        className="mx-4 max-h-[85vh] w-full max-w-lg overflow-y-auto rounded-2xl border border-border bg-surface p-5 shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold text-text">
            {mode === "import" ? "从云文档导入表格" : "导出表格副本到云"}
          </h2>
          <span className="text-[10px] text-faint">副本交换 · 不自动同步</span>
        </div>

        {ext.unavailable && <p className="mt-3 text-xs text-danger">外部文档面未装配。</p>}

        {!record && mode === "import" && (
          <>
            <input
              autoFocus
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && url.trim()) void doPreview();
              }}
              disabled={busy !== "idle"}
              placeholder="粘贴腾讯文档表格 / 飞书电子表格或多维表格链接（https://…）"
              aria-label="云端表格链接"
              className="mt-3 w-full rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none placeholder:text-faint focus:border-accent"
            />
            <p className="mt-1.5 text-[11px] text-faint">
              按链接自动归属平台；导入为本地新表格副本，公式保留原文（超出本地能力的内容见预览清单）。飞书多维表格按「字段=列头、记录=行」展平导入，仅只读。
            </p>
          </>
        )}

        {!record && mode === "export" && !updTarget && !live && (
          <div className="mt-3 flex gap-1.5">
            {configured.length === 0 && <p className="text-xs text-faint">没有已配置的平台——请先到设置页配置。</p>}
            {configured.map((pid) => (
              <button
                key={pid}
                type="button"
                disabled={busy !== "idle"}
                onClick={() => setProvider(pid)}
                className={`rounded-lg border px-3 py-1.5 text-xs transition-colors ${
                  provider === pid ? "border-accent bg-accent-soft text-accent" : "border-border text-dim hover:bg-surface-2 hover:text-text"
                }`}
              >
                {PROVIDER_LABEL[pid] ?? pid}
              </button>
            ))}
          </div>
        )}

        {/* M3 更新已有目标：历史导出目标列表 */}
        {!record && mode === "export" && !updTarget && !live && targets && targets.length > 0 && (
          <div className="mt-3 border-t border-border pt-2">
            <p className="text-[10px] text-faint">或更新已有目标（只覆写差异格，不动结构、不清除远端多出格）</p>
            <ul className="mt-1 space-y-1">
              {targets.map((tg) => (
                <li key={tg.transfer_id}>
                  <button
                    type="button"
                    disabled={busy !== "idle"}
                    onClick={() => void doUpdatePreview({ provider: tg.provider as ExtDocProviderID, url: tg.external_url ?? "" })}
                    className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-xs transition-colors hover:bg-surface-2 disabled:opacity-40"
                  >
                    <span className="shrink-0 rounded bg-surface-3 px-1.5 py-0.5 text-[10px] text-dim">
                      {PROVIDER_LABEL[tg.provider] ?? tg.provider}
                    </span>
                    <span className="min-w-0 flex-1 truncate text-dim">{tg.external_url}</span>
                    <span className="shrink-0 text-[10px] text-faint">{relativeTime(tg.created_at)}</span>
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* M3 回写差异预览 */}
        {!record && mode === "export" && updTarget && !live && (
          <div className="mt-3 rounded-xl border border-border bg-surface-2 p-3">
            <div className="flex items-center justify-between">
              <p className="text-xs font-medium text-text">更新目标</p>
              <button
                type="button"
                disabled={busy !== "idle"}
                onClick={() => {
                  setUpdTarget(null);
                  setUpdPreview(null);
                }}
                className="rounded px-2 py-0.5 text-[11px] text-dim hover:bg-surface-3 hover:text-text"
              >
                ← 换目标
              </button>
            </div>
            <p className="mt-1 truncate text-[11px] text-faint">{updTarget.url}</p>
            {updPreview ? (
              <>
                <ul className="mt-1.5 space-y-0.5 text-[11px] text-dim">
                  {updPreview.diffs.map((d, i) => (
                    <li key={i}>
                      {d.name} · 覆写 {d.to_write} 格{d.formula_diffs > 0 && `（含公式 ${d.formula_diffs}）`}
                      {d.remote_extra > 0 && ` · 远端多出 ${d.remote_extra}（不清除）`}
                    </li>
                  ))}
                </ul>
                <div className="mt-2 border-t border-border pt-2">
                  <IssueList issues={updPreview.issues} />
                </div>
              </>
            ) : (
              <p className="mt-2 text-xs text-faint">{busy === "preview" ? "比对差异中…" : "预览失败，请重试或换目标。"}</p>
            )}
          </div>
        )}

        {/* M3 回写执行态：进度/取消/续跑 */}
        {live && !record && (
          <div className="mt-3 rounded-xl border border-border bg-surface-2 p-3">
            <p className="text-xs font-medium text-text">
              {live.status === "running" && "覆写中…"}
              {live.status === "partial" && "已写部分后中止（已写保留）"}
              {live.status === "canceled" && "已取消（已写保留）"}
              {live.status === "unknown" && "结果未知（传输中断）——请核对目标后续跑"}
              {(live.status === "succeeded" || live.status === "succeeded_with_warnings") && (live.status === "succeeded" ? "更新完成。" : "完成（有损——见下方清单）。")}
              {live.status === "failed" && `失败：${live.error ?? ""}`}
            </p>
            {live.cells_total !== undefined && live.cells_total > 0 && (
              <div className="mt-2">
                <div className="h-1.5 overflow-hidden rounded-full bg-surface-3">
                  <div
                    className="h-full rounded-full bg-accent transition-[width]"
                    style={{ width: `${Math.min(100, Math.round(((live.cells_written ?? 0) / live.cells_total) * 100))}%` }}
                  />
                </div>
                <p className="mt-1 text-[10px] text-faint">
                  {live.cells_written ?? 0} / {live.cells_total} 格
                </p>
              </div>
            )}
            {live.report.length > 0 && (
              <div className="mt-2 border-t border-border pt-2">
                <IssueList issues={live.report} />
              </div>
            )}
            <div className="mt-3 flex justify-end gap-2">
              {live.status === "running" && (
                <button
                  type="button"
                  disabled={busy !== "idle"}
                  onClick={() => void doCancel()}
                  className="rounded-lg border border-border px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-3 hover:text-text disabled:opacity-40"
                >
                  {busy === "execute" ? "请求中…" : "取消"}
                </button>
              )}
              {(live.status === "partial" || live.status === "canceled" || live.status === "unknown") && (
                <button
                  type="button"
                  disabled={busy !== "idle"}
                  onClick={() => void doResume()}
                  className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                >
                  {busy === "execute" ? "校验中…" : "续跑"}
                </button>
              )}
              {(live.status === "succeeded" || live.status === "succeeded_with_warnings" || live.status === "failed") && (
                <button
                  type="button"
                  onClick={onClose}
                  className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
                >
                  完成
                </button>
              )}
            </div>
          </div>
        )}

        {preview && !record && (
          <div className="mt-3 rounded-xl border border-border bg-surface-2 p-3">
            <p className="text-xs font-medium text-text">
              {preview.title || "（未命名）"}
              <span className="ml-2 text-[10px] font-normal text-faint">
                {PROVIDER_LABEL[preview.provider] ?? preview.provider} · {preview.sheets.length} 张工作表
              </span>
            </p>
            <ul className="mt-1.5 space-y-0.5 text-[11px] text-dim">
              {preview.sheets.map((sh, i) => (
                <li key={i}>
                  {sh.name} · 已填 {sh.filled} 格{sh.formulas > 0 && ` · 公式 ${sh.formulas}`}
                  {sh.merge_count !== undefined && sh.merge_count > 0 && ` · 合并 ${sh.merge_count} 区（不迁移）`}
                </li>
              ))}
            </ul>
            <div className="mt-2 border-t border-border pt-2">
              <IssueList issues={preview.issues} />
            </div>
          </div>
        )}

        {error && (
          <div className="mt-2 rounded-lg bg-danger/10 px-2.5 py-1.5 text-xs text-danger">
            {error}
          </div>
        )}
        {errorIssues && (
          <div className="mt-2 rounded-xl border border-border bg-surface-2 p-2">
            <IssueList issues={errorIssues} />
          </div>
        )}

        {record && (
          <div className="mt-3 rounded-xl border border-border bg-surface-2 p-3">
            <p className="text-xs font-medium text-text">
              {record.status === "succeeded" && "交换完成。"}
              {record.status === "succeeded_with_warnings" && "完成（有损——见下方清单）。"}
              {(record.status === "failed" || record.status === "unknown") && `${STATUS_LABEL[record.status]}：${record.error ?? ""}`}
            </p>
            {record.external_url && (
              <a
                href={record.external_url}
                target="_blank"
                rel="noreferrer"
                className="mt-1 block truncate text-[11px] text-accent hover:underline"
              >
                {record.external_url} ↗
              </a>
            )}
            {record.doc_id && (
              <p className="mt-1 text-[11px] text-faint">本地表格：{record.doc_id}</p>
            )}
            <div className="mt-2 border-t border-border pt-2">
              <IssueList issues={record.report} />
            </div>
            <div className="mt-3 flex justify-end gap-2">
              {record.doc_id && (
                <button
                  type="button"
                  onClick={() => onImported?.(record.doc_id!)}
                  className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90"
                >
                  打开表格
                </button>
              )}
              <button
                type="button"
                onClick={onClose}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
              >
                完成
              </button>
            </div>
          </div>
        )}

        {!record && !live && (
          <div className="mt-4 flex justify-end gap-2">
            <button
              type="button"
              disabled={busy !== "idle"}
              onClick={() => {
                setUpdTarget(null);
                setUpdPreview(null);
                onClose();
              }}
              className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
            >
              取消
            </button>
            {mode === "export" && updTarget ? (
              <button
                type="button"
                disabled={busy !== "idle" || !updPreview || updPreview.cells_total === 0}
                onClick={() => void doUpdateExecute()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {busy === "execute" ? "受理中…" : updPreview?.cells_total === 0 ? "无差异" : `覆写 ${updPreview?.cells_total ?? 0} 格`}
              </button>
            ) : !preview ? (
              <button
                type="button"
                disabled={busy !== "idle" || (mode === "import" ? !url.trim() : !provider)}
                onClick={() => void doPreview()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {busy === "preview" ? "读取中…" : "预览"}
              </button>
            ) : (
              <button
                type="button"
                disabled={busy !== "idle"}
                onClick={() => void doExecute()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {busy === "execute" ? "执行中…" : mode === "import" ? "导入副本" : "导出副本"}
              </button>
            )}
          </div>
        )}

        <RecentTransfers mode={mode} docID={docID} />
      </div>
    </div>
  );
}

/** 最近交换记录（折叠态展示，失败/未知醒目）。 */
function RecentTransfers({ mode, docID }: { mode: "import" | "export"; docID?: string }) {
  const [items, setItems] = useState<SheetTransferRecord[] | null>(null);
  useEffect(() => {
    api
      .listSheetTransfers({ direction: mode, ...(mode === "export" && docID ? { doc_id: docID } : {}), limit: 5 })
      .then((r) => setItems(r.transfers))
      .catch(() => setItems(null));
  }, [mode, docID]);
  if (!items || items.length === 0) return null;
  return (
    <div className="mt-4 border-t border-border pt-2">
      <p className="text-[10px] text-faint">最近{mode === "import" ? "导入" : "导出"}</p>
      <ul className="mt-1 space-y-0.5">
        {items.map((r) => (
          <li key={r.transfer_id} className="flex items-center gap-2 text-[11px]">
            <span
              className={
                r.status === "failed" || r.status === "unknown"
                  ? "text-danger"
                  : r.status === "succeeded_with_warnings"
                    ? "text-amber-600 dark:text-amber-400"
                    : "text-dim"
              }
            >
              {STATUS_LABEL[r.status] ?? r.status}
            </span>
            <span className="min-w-0 flex-1 truncate text-dim">{r.title || r.doc_id || r.external_url}</span>
            <span className="shrink-0 text-faint">{relativeTime(r.created_at)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
