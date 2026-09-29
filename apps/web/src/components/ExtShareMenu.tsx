// 外部云文档共享控件（RFC-0016 多 provider）：共享菜单（已配置 provider 列表）
// + per-provider 已共享徽标（同文档可两家并存）。DocsTab 行内与文档编辑页
// 顶栏共用。
import { useEffect, useRef, useState } from "react";
import type { ExtDocProviderID } from "../api/client";
import { refreshExtShares, shareDocToExt, useExtShares } from "../state/extdocs";
import { relativeTime } from "../lib/ui";

/** provider 展示信息（与 ExtDocProviderView.display_name 对齐的本地副本）。 */
const PROVIDER_LABEL: Record<string, string> = {
  tencent_docs: "腾讯文档",
  feishu_docs: "飞书云文档",
};

/** 已共享徽标（per-provider 外链；无映射返回 null）。 */
export function ExtShareBadges({ docID }: { docID: string }) {
  const ext = useExtShares();
  if (ext.unavailable) return null;
  const shares = ext.byDoc?.get(docID);
  if (!shares || shares.size === 0) return null;
  return (
    <>
      {[...shares.entries()].map(([provider, share]) => (
        <a
          key={provider}
          href={share.external_url}
          target="_blank"
          rel="noreferrer"
          title={`已共享到${PROVIDER_LABEL[provider] ?? provider}（${relativeTime(share.shared_at)}）——点击打开`}
          className="shrink-0 rounded-lg bg-surface-2 px-2 py-1.5 text-xs text-dim transition-colors hover:text-text"
        >
          {PROVIDER_LABEL[provider] ?? provider} ↗
        </a>
      ))}
    </>
  );
}

/** 共享下拉菜单（仅列已配置 provider；无可用 provider 不渲染）。 */
export function ExtShareMenu({ docID, compact = false }: { docID: string; compact?: boolean }) {
  const ext = useExtShares();
  const [open, setOpen] = useState(false);
  const [busyProvider, setBusyProvider] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    void refreshExtShares();
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  if (ext.unavailable) return null;
  const configured = [...ext.configuredProviders] as ExtDocProviderID[];
  if (configured.length === 0) return null;
  const shared = ext.byDoc?.get(docID);
  const candidates = configured.filter((p) => !shared?.has(p));
  if (candidates.length === 0) return null;

  const share = async (provider: ExtDocProviderID) => {
    setOpen(false);
    setBusyProvider(provider);
    setMsg(null);
    try {
      await shareDocToExt(docID, provider);
      setMsg(`已共享到${PROVIDER_LABEL[provider] ?? provider}`);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusyProvider(null);
    }
  };

  return (
    <div ref={ref} className="relative shrink-0">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        disabled={busyProvider !== null}
        aria-expanded={open}
        aria-haspopup="menu"
        title="导出 markdown 快照并创建外部云文档（文档级映射，不回同步）"
        className={
          compact
            ? "rounded-lg px-2.5 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
            : "shrink-0 rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
        }
      >
        {busyProvider ? "共享中…" : "共享到云文档 ▾"}
      </button>
      {msg && <span className="ml-1 text-[10px] text-faint">{msg}</span>}
      {open && (
        <div
          role="menu"
          className="animate-fade-in absolute right-0 top-full z-40 mt-1 w-32 overflow-hidden rounded-lg border border-border bg-surface py-0.5 shadow-xl"
        >
          {candidates.map((p) => (
            <button
              key={p}
              type="button"
              role="menuitem"
              onClick={() => void share(p)}
              className="block w-full px-2.5 py-1 text-left text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
            >
              {PROVIDER_LABEL[p] ?? p}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
