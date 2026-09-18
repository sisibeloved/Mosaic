// 文档编辑器（RFC-0014 §2.5 自绘块编辑器 + §2.3 修订批 CAS，不引重框架）：
//  工作台模型：blocks（React state，用户所见）+ committedRef（最近一次与服务端
//    一致的块集与版本）——提交 = diffBlocks(committed, working) 的协议 op 批；
//  保存节律：任何工作区变更 → 1.5s 防抖 → commit_doc_revision（base_version =
//    committed.version；>64 ops 分批顺序提交）；提交期新编辑由 dirtyAgain 接力；
//  409 重定基：响应携带当前态 → 在新态上重放全部未提交 ops（锚点丢失回退文末
//    ——用户文本不丢）→ 重试一次；再撞 → 提示 + 本地编辑保留（下次编辑自愈，
//    横幅可手动重试）；
//  直播（§2.6 doc:{id} SSE 经 useDoc 快照重取）：无未提交变更 → 静默采用；
//    编辑中 → 只亮"有新版本"，不打断输入（合并走提交时的 409 路径）；
//  新块 ID 客户端生成（blk_w…，服务端保留非空 ID）——新块跨提交稳定可锚定；
//  归档 = 只读横幅；他端删除 → 回列表并提示。标题改名独立命令（rename_doc）。
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, type DocBlock, type DocCommandKind, type DocCommandResponse } from "../api/client";
import { useDoc } from "../api/doc";
import { MarkdownBody } from "../components/chat/MarkdownBody";
import { applyOpsLocal, diffBlocks, listBlockMarkdown, newBlockID } from "../lib/docops";
import { relativeTime } from "../lib/ui";
import { refreshDocMeta, refreshDocs } from "../state/docs";

const COMMIT_DEBOUNCE_MS = 1500;
/** 服务端 MaxOpsPerBatch——超量变更分批顺序提交（op 序自包含，跨批锚定安全）。 */
const OPS_BATCH = 64;

type SaveState = "idle" | "dirty" | "saving" | "saved" | "conflict" | "error";

const BLOCK_TYPES: { id: DocBlock["type"]; label: string }[] = [
  { id: "heading", label: "标题" },
  { id: "paragraph", label: "段落" },
  { id: "list", label: "列表" },
  { id: "code", label: "代码" },
  { id: "quote", label: "引用" },
  { id: "hr", label: "分隔线" },
];

export function DocEditorPage() {
  const { docId = null } = useParams();
  const navigate = useNavigate();
  const { doc, notFound, deleted, error: loadError } = useDoc(docId);

  const [blocks, setBlocks] = useState<DocBlock[] | null>(null);
  const blocksRef = useRef<DocBlock[] | null>(null);
  const committedRef = useRef<{ blocks: DocBlock[]; version: number } | null>(null);
  const [saveState, setSaveState] = useState<SaveState>("idle");
  const [saveError, setSaveError] = useState<string | null>(null);
  const [hasUpdate, setHasUpdate] = useState(false);
  const [savedVersion, setSavedVersion] = useState(0);
  const [editingID, setEditingID] = useState<string | null>(null);
  const committingRef = useRef(false);
  const dirtyAgainRef = useRef(false);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const [titleDraft, setTitleDraft] = useState("");
  const [titleEditing, setTitleEditing] = useState(false);
  const [actionBusy, setActionBusy] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteReason, setDeleteReason] = useState("");

  const setWorking = useCallback((next: DocBlock[]) => {
    blocksRef.current = next;
    setBlocks(next);
  }, []);

  // 服务器态 → 工作台：无未提交变更时静默采用；有则只亮"有新版本"（合并走提交
  // 时的 409 重定基——不打断在途输入）。自己提交的回流（version 不超前）忽略。
  useEffect(() => {
    if (!doc) return;
    const committed = committedRef.current;
    if (committed && doc.version <= committed.version) return;
    const working = blocksRef.current ?? [];
    const pending = committed ? diffBlocks(committed.blocks, working).length > 0 : false;
    if (!committed || !pending) {
      committedRef.current = { blocks: doc.blocks, version: doc.version };
      setWorking(doc.blocks);
      setHasUpdate(false);
    } else {
      setHasUpdate(true);
    }
  }, [doc, setWorking]);

  // 标题草稿：非编辑期跟随服务器态（SSE 重取同步改名结果）。
  useEffect(() => {
    if (doc && !titleEditing) setTitleDraft(doc.title);
  }, [doc, titleEditing]);

  // 他端删除（doc.deleted / 快照 404）→ 回列表带提示。
  useEffect(() => {
    if (deleted) navigate("/docs", { state: { notice: "文档已被删除（可能在他端操作）" } });
  }, [deleted, navigate]);

  const commitNow = useCallback(async (): Promise<void> => {
    const id = docId;
    const committed = committedRef.current;
    if (!id || !committed) return;
    if (committingRef.current) {
      dirtyAgainRef.current = true;
      return;
    }
    const working = blocksRef.current ?? [];
    const ops = diffBlocks(committed.blocks, working);
    if (ops.length === 0) {
      setSaveState("idle");
      return;
    }
    committingRef.current = true;
    setSaveState("saving");
    setSaveError(null);
    const snapshot = working; // 本次提交的基线快照
    let version = committed.version;
    try {
      for (let i = 0; i < ops.length; i += OPS_BATCH) {
        const resp = await api.commitDocRevision(id, version, ops.slice(i, i + OPS_BATCH));
        version = resp.doc_version;
      }
      committedRef.current = { blocks: snapshot, version };
      setSavedVersion(version);
      setSaveState("saved");
      setHasUpdate(false);
      void refreshDocMeta(id);
    } catch (e) {
      const conflict = e instanceof ApiError && e.status === 409 ? e.docConflict : undefined;
      if (conflict?.state) {
        // 409 重定基：在新态上重放全部未提交 ops（锚点丢失回退文末），重试一次
        const base2 = { blocks: conflict.state.blocks, version: conflict.state.version };
        const liveWorking = blocksRef.current ?? [];
        const rebased = applyOpsLocal(base2.blocks, diffBlocks(committed.blocks, liveWorking));
        committedRef.current = base2;
        setWorking(rebased);
        setHasUpdate(false);
        try {
          const ops2 = diffBlocks(base2.blocks, rebased);
          let v2 = base2.version;
          for (let i = 0; i < ops2.length; i += OPS_BATCH) {
            const resp = await api.commitDocRevision(id, v2, ops2.slice(i, i + OPS_BATCH));
            v2 = resp.doc_version;
          }
          committedRef.current = { blocks: rebased, version: v2 };
          setSavedVersion(v2);
          setSaveState("saved");
          void refreshDocMeta(id);
        } catch {
          setSaveState("conflict");
          setSaveError("文档已被更新，本地编辑已保留——请重试保存");
        }
      } else {
        setSaveState("error");
        setSaveError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      committingRef.current = false;
      if (dirtyAgainRef.current) {
        dirtyAgainRef.current = false;
        void commitNow();
      }
    }
  }, [docId, setWorking]);

  /** 工作区变更入口：1.5s 防抖聚合 → 修订批。 */
  const markDirty = useCallback(() => {
    setSaveState((s) => (s === "saving" ? s : "dirty"));
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      debounceRef.current = null;
      void commitNow();
    }, COMMIT_DEBOUNCE_MS);
  }, [commitNow]);

  useEffect(
    () => () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    },
    [],
  );

  const mutate = useCallback(
    (fn: (ws: DocBlock[]) => DocBlock[]) => {
      setWorking(fn(blocksRef.current ?? []));
      markDirty();
    },
    [setWorking, markDirty],
  );

  const updateText = (id: string, text: string) =>
    mutate((ws) => ws.map((b) => (b.block_id === id ? { ...b, text } : b)));
  const changeType = (id: string, type: DocBlock["type"]) =>
    mutate((ws) => ws.map((b) => (b.block_id === id ? { ...b, type, text: type === "hr" ? "" : b.text } : b)));
  const insertAfter = (afterID: string | null) => {
    const nb: DocBlock = { block_id: newBlockID(), type: "paragraph", text: "" };
    mutate((ws) => {
      if (afterID === null) return [...ws, nb];
      const idx = ws.findIndex((b) => b.block_id === afterID);
      const next = [...ws];
      next.splice(idx >= 0 ? idx + 1 : next.length, 0, nb);
      return next;
    });
    setEditingID(nb.block_id);
  };
  const removeBlock = (id: string) => {
    mutate((ws) => ws.filter((b) => b.block_id !== id));
    setEditingID((cur) => (cur === id ? null : cur));
  };
  /** 退出编辑：新建空块未落内容 → 移除不留痕（从未进已提交态，diff 自然为空）。 */
  const finishEdit = (id: string) => {
    setEditingID((cur) => (cur === id ? null : cur));
    const ws = blocksRef.current ?? [];
    const b = ws.find((x) => x.block_id === id);
    const committed = committedRef.current;
    if (
      b &&
      b.type !== "hr" &&
      b.text.trim() === "" &&
      committed &&
      !committed.blocks.some((x) => x.block_id === id)
    ) {
      setWorking(ws.filter((x) => x.block_id !== id));
    }
  };

  /** 改名（rename_doc 独立命令）：409 拉新态重试一次。 */
  const submitTitle = async () => {
    setTitleEditing(false);
    const title = titleDraft.trim();
    if (!docId || !doc || !title || title === doc.title) {
      if (doc) setTitleDraft(doc.title);
      return;
    }
    setActionError(null);
    try {
      await api.docCommand(docId, "rename_doc", doc.version, { title });
      void refreshDocMeta(docId);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try {
          const fresh = await api.getDoc(docId);
          await api.docCommand(docId, "rename_doc", fresh.version, { title });
          void refreshDocMeta(docId);
          return;
        } catch (e2) {
          setActionError(e2 instanceof Error ? e2.message : String(e2));
          return;
        }
      }
      setActionError(e instanceof Error ? e.message : String(e));
      setTitleDraft(doc.title);
    }
  };

  /** 管理动作（归档/恢复/副本/删除）：版本校准 + 409 重试一次（withVersion 先例）。 */
  const runAction = async (
    kind: DocCommandKind,
    payload: unknown,
    onOK?: (resp: DocCommandResponse) => void,
  ) => {
    if (!docId || actionBusy) return;
    setActionBusy(kind);
    setActionError(null);
    try {
      const cur = doc ?? (await api.getDoc(docId));
      let resp: DocCommandResponse;
      try {
        resp = await api.docCommand(docId, kind, cur.version, payload);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          const fresh = await api.getDoc(docId);
          resp = await api.docCommand(docId, kind, fresh.version, payload);
        } else {
          throw e;
        }
      }
      void refreshDocs();
      if (docId) void refreshDocMeta(docId);
      onOK?.(resp);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
      throw e;
    } finally {
      setActionBusy(null);
    }
  };

  // ---- 渲染 ----

  if (notFound) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 text-sm">
        <p className="text-faint">文档不存在或已删除。</p>
        <Link to="/docs" className="text-accent hover:underline">
          返回文档列表
        </Link>
      </div>
    );
  }
  if (!doc || blocks === null) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-faint">
        {loadError ?? "加载中…"}
      </div>
    );
  }

  const readOnly = doc.status === "archived";
  const saveBadge =
    saveState === "dirty"
      ? "待保存…"
      : saveState === "saving"
        ? "保存中…"
        : saveState === "saved"
          ? `已保存 v${savedVersion}`
          : null;

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center gap-2 border-b border-border px-4 py-2.5">
        <Link
          to="/docs"
          title="返回文档列表"
          aria-label="返回文档列表"
          className="shrink-0 rounded-lg p-1.5 text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          <IconBack />
        </Link>
        {titleEditing && !readOnly ? (
          <input
            autoFocus
            value={titleDraft}
            onChange={(e) => setTitleDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submitTitle();
              if (e.key === "Escape") {
                setTitleDraft(doc.title);
                setTitleEditing(false);
              }
            }}
            onBlur={() => void submitTitle()}
            maxLength={200}
            aria-label="文档标题"
            className="w-72 rounded-lg border border-border bg-surface-2 px-2 py-1 text-sm outline-none focus:border-accent"
          />
        ) : (
          <h1
            className={`truncate text-sm font-medium tracking-tight ${readOnly ? "" : "cursor-text"}`}
            title={readOnly ? doc.title : "点击改名"}
            onClick={() => !readOnly && setTitleEditing(true)}
          >
            {doc.title}
          </h1>
        )}
        {readOnly && (
          <span className="shrink-0 rounded bg-surface-3 px-1.5 py-px text-[10px] leading-4 text-dim">已归档</span>
        )}
        {hasUpdate && !readOnly && (
          <span
            className="shrink-0 rounded bg-accent-soft px-1.5 py-px text-[10px] leading-4 text-accent"
            title="他端有新版本；你的下次提交会自动在新版本上合并"
          >
            有新版本
          </span>
        )}
        {saveBadge && !readOnly && <span className="shrink-0 text-[11px] text-faint">{saveBadge}</span>}
        <div className="flex-1" />
        <span className="shrink-0 text-[11px] text-faint" title={`v${doc.version} · ${doc.updated_by} 更新`}>
          v{doc.version} · {relativeTime(doc.updated_at)}
        </span>
        <a
          href={api.exportDocUrl(doc.doc_id)}
          download={`${doc.title}.md`}
          title="导出 markdown"
          className="shrink-0 rounded-lg px-2.5 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          导出
        </a>
        <DocMenu
          archived={readOnly}
          busy={actionBusy}
          onDuplicate={() =>
            void runAction("duplicate_doc", {}, (resp) => navigate(`/docs/${encodeURIComponent(resp.doc_id)}`)).catch(
              () => {},
            )
          }
          onArchiveToggle={() => void runAction(readOnly ? "restore_doc" : "archive_doc", {}).catch(() => {})}
          onDelete={() => {
            setDeleteReason("");
            setDeleting(true);
          }}
        />
      </header>
      {(actionError || saveError) && (
        <p className="border-b border-border bg-[color-mix(in_srgb,var(--danger)_8%,transparent)] px-4 py-1.5 text-xs text-danger">
          {saveError ?? actionError}
          {saveState === "conflict" && (
            <button
              type="button"
              onClick={() => void commitNow()}
              className="ml-2 rounded px-1.5 py-0.5 text-[11px] underline hover:opacity-80"
            >
              重试保存
            </button>
          )}
        </p>
      )}
      {readOnly && (
        <p className="border-b border-border bg-surface-2 px-4 py-1.5 text-xs text-faint">
          文档已归档——只读。如需继续编辑，从右上"…"菜单恢复。
        </p>
      )}
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex max-w-3xl flex-col gap-1 px-6 py-6">
          {blocks.length === 0 && (
            <p className="py-8 text-center text-sm text-faint">
              {readOnly ? "空文档。" : "空文档——点击下方按钮添加第一个块。"}
            </p>
          )}
          {blocks.map((b) => (
            <BlockView
              key={b.block_id}
              block={b}
              editing={editingID === b.block_id && !readOnly}
              readOnly={readOnly}
              onStartEdit={() => !readOnly && b.type !== "hr" && setEditingID(b.block_id)}
              onChangeText={(text) => updateText(b.block_id, text)}
              onFinishEdit={() => finishEdit(b.block_id)}
              onInsertAfter={() => insertAfter(b.block_id)}
              onRemove={() => removeBlock(b.block_id)}
              onChangeType={(t) => changeType(b.block_id, t)}
            />
          ))}
          {!readOnly && (
            <button
              type="button"
              onClick={() => insertAfter(null)}
              className="mt-1 self-start rounded-lg px-2.5 py-1 text-xs text-faint transition-colors hover:bg-surface-2 hover:text-text"
            >
              + 添加块
            </button>
          )}
        </div>
      </div>

      {deleting && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          role="dialog"
          aria-modal="true"
          aria-label="删除文档确认"
          onClick={() => !actionBusy && setDeleting(false)}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !actionBusy) setDeleting(false);
          }}
        >
          <div
            className="mx-4 w-full max-w-md rounded-2xl border border-border bg-surface p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-sm font-semibold text-text">删除文档「{doc.title}」</h2>
            <p className="mt-2 text-xs leading-5 text-dim">
              删除不可逆：文档内容与修订历史将被清除（仅保留墓碑与删除理由）；房间里的引用卡片会降级为"已删除"。
            </p>
            <textarea
              autoFocus
              value={deleteReason}
              onChange={(e) => setDeleteReason(e.target.value)}
              maxLength={280}
              rows={2}
              disabled={actionBusy === "delete_doc"}
              placeholder="删除理由（必填，1–280 字，留痕审计）"
              aria-label="删除理由"
              className="mt-3 w-full resize-none rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none placeholder:text-faint focus:border-danger"
            />
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                disabled={actionBusy === "delete_doc"}
                onClick={() => setDeleting(false)}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
              >
                取消
              </button>
              <button
                type="button"
                disabled={actionBusy === "delete_doc" || !deleteReason.trim()}
                onClick={() =>
                  void runAction("delete_doc", { reason: deleteReason.trim() }, () =>
                    navigate("/docs", { state: { notice: `文档「${doc.title}」已删除` } }),
                  ).catch(() => {})
                }
                className="rounded-lg bg-danger px-3 py-1.5 text-xs font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {actionBusy === "delete_doc" ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

/** 单块视图：读模式按类型渲染（hover 出控件）；点击进编辑态（自增高 textarea，
 *  markdown 源文本）；Enter 在文末 → 下方插新块；Esc/blur 退出编辑。 */
function BlockView({
  block,
  editing,
  readOnly,
  onStartEdit,
  onChangeText,
  onFinishEdit,
  onInsertAfter,
  onRemove,
  onChangeType,
}: {
  block: DocBlock;
  editing: boolean;
  readOnly: boolean;
  onStartEdit: () => void;
  onChangeText: (text: string) => void;
  onFinishEdit: () => void;
  onInsertAfter: () => void;
  onRemove: () => void;
  onChangeType: (type: DocBlock["type"]) => void;
}) {
  const areaRef = useRef<HTMLTextAreaElement | null>(null);
  const [confirming, setConfirming] = useState(false);

  // 删除两步确认（仅非空块）：× → 3s 内"确认？"，超时回退
  useEffect(() => {
    if (!confirming) return;
    const t = setTimeout(() => setConfirming(false), 3000);
    return () => clearTimeout(t);
  }, [confirming]);

  // 自增高（上限 400px）+ 进编辑态聚焦
  useEffect(() => {
    const el = areaRef.current;
    if (!el || !editing) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 400)}px`;
  }, [block.text, editing]);
  useEffect(() => {
    if (editing) areaRef.current?.focus();
  }, [editing]);

  const controls = !readOnly && (
    <div
      // mousedown 拦默认：控件交互不夺 textarea 焦点（否则 blur → 退编辑 →
      // 控件随块卸载，下拉/按钮闪现即死——2026-09-18 狗粮实证）；click 止冒泡：
      // 读模式点控件不进入编辑。
      onMouseDown={(e) => e.preventDefault()}
      onClick={(e) => e.stopPropagation()}
      className={`absolute right-1.5 top-1.5 flex items-center gap-0.5 rounded-lg bg-surface px-1 py-0.5 shadow-sm transition-opacity ${
        editing ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-within:opacity-100"
      }`}
    >
      <TypeMenu value={block.type} onChange={onChangeType} />
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          onFinishEdit();
          onInsertAfter();
        }}
        title="在下方插入块"
        aria-label="在下方插入块"
        className="rounded px-1 text-[11px] text-faint transition-colors hover:bg-surface-3 hover:text-text"
      >
        +
      </button>
      {block.type !== "hr" && block.text.trim() !== "" && !confirming ? (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            setConfirming(true);
          }}
          title="删除块"
          aria-label="删除块"
          className="rounded px-1 text-[11px] text-faint transition-colors hover:bg-surface-3 hover:text-danger"
        >
          ×
        </button>
      ) : (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            setConfirming(false);
            onRemove();
          }}
          title={confirming ? "确认删除该块" : "删除块"}
          aria-label={confirming ? "确认删除该块" : "删除块"}
          className={`rounded px-1 text-[11px] transition-colors ${
            confirming ? "bg-danger text-white" : "text-faint hover:bg-surface-3 hover:text-danger"
          }`}
        >
          {confirming ? "确认？" : "×"}
        </button>
      )}
    </div>
  );

  if (editing && block.type !== "hr") {
    return (
      <div className="group relative rounded-lg border border-accent/50 bg-surface px-2.5 py-1.5">
        {controls}
        <textarea
          ref={areaRef}
          value={block.text}
          onChange={(e) => onChangeText(e.target.value)}
          onBlur={onFinishEdit}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing) return;
            const el = e.currentTarget;
            if (e.key === "Escape") {
              e.preventDefault();
              onFinishEdit();
              return;
            }
            if (e.key !== "Enter") return;
            // Ctrl/Cmd+Enter：收块并在下方新起一块（显式出口）。
            if (e.ctrlKey || e.metaKey) {
              e.preventDefault();
              onFinishEdit();
              onInsertAfter();
              return;
            }
            // 空行再 Enter（光标在尾且文本以换行结尾）：去尾换行 → 收块新起——
            // 多行块（列表/表格/分段）的自然出口；其余 Enter = 块内换行（默认
            // 行为——此前 Enter 直接切新块，表格/列表逐行被拆散、永不渲染）。
            if (
              !e.shiftKey &&
              el.selectionStart === el.value.length &&
              el.selectionEnd === el.value.length &&
              el.value.endsWith("\n")
            ) {
              e.preventDefault();
              onChangeText(el.value.replace(/\n+$/, ""));
              onFinishEdit();
              onInsertAfter();
            }
          }}
          rows={1}
          aria-label="块内容（markdown）"
          className="w-full resize-none bg-transparent pr-16 font-mono text-[13px] leading-relaxed outline-none placeholder:text-faint"
          placeholder={
            block.type === "code"
              ? "代码…（Ctrl+Enter 收块）"
              : "markdown 文本…（Enter 换行；空行再 Enter 或 Ctrl+Enter 新起一块）"
          }
        />
      </div>
    );
  }

  return (
    <div
      className={`group relative rounded-lg px-2.5 py-1.5 transition-colors ${readOnly ? "" : "cursor-text hover:bg-surface-2"}`}
      onClick={(e) => {
        // 链接可点（新开页签），不进入编辑——markdown 超链接是阅读面的一部分。
        if ((e.target as HTMLElement).closest("a")) return;
        onStartEdit();
      }}
    >
      {controls}
      <BlockContent block={block} />
    </div>
  );
}

/** 块类型下拉（自绘——原生 select 在此控件容器里必被秒杀：读模式点击冒泡进
 *  编辑态抢焦、编辑态点击触发 textarea blur 退编辑，下拉框闪现即消失；
 *  2026-09-18 狗粮实证。容器已拦 mousedown 默认 + click 冒泡，此处只管开关）。 */
function TypeMenu({ value, onChange }: { value: DocBlock["type"]; onChange: (t: DocBlock["type"]) => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const current = BLOCK_TYPES.find((t) => t.id === value)?.label ?? value;
  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-haspopup="menu"
        title="块类型"
        aria-label="块类型"
        className="rounded px-1 text-[11px] text-dim transition-colors hover:bg-surface-3 hover:text-text"
      >
        {current} ▾
      </button>
      {open && (
        <div
          role="menu"
          className="animate-fade-in absolute right-0 top-full z-40 mt-1 w-24 overflow-hidden rounded-lg border border-border bg-surface py-0.5 shadow-xl"
        >
          {BLOCK_TYPES.map((t) => (
            <button
              key={t.id}
              type="button"
              role="menuitem"
              onClick={() => {
                setOpen(false);
                onChange(t.id);
              }}
              className={`block w-full px-2.5 py-1 text-left text-[11px] transition-colors hover:bg-surface-2 ${
                t.id === value ? "font-medium text-text" : "text-dim hover:text-text"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/** 读模式按块类型渲染（标题/段落走 MarkdownBody；列表补标记后走 MarkdownBody；
 *  代码原文 <pre>；hr 横线）。 */
function BlockContent({ block }: { block: DocBlock }) {
  if (block.type !== "hr" && block.text.trim() === "") {
    return <p className="text-sm text-faint">空块——点击编辑</p>;
  }
  switch (block.type) {
    case "heading":
      return (
        <div className="text-[15px] font-semibold text-text">
          <MarkdownBody text={block.text} />
        </div>
      );
    case "code":
      return (
        <pre className="overflow-x-auto rounded-lg bg-surface-3 p-2.5 font-mono text-xs leading-relaxed">
          {block.text}
        </pre>
      );
    case "quote":
      return (
        <div className="border-l-2 border-border pl-2.5 text-dim">
          <MarkdownBody text={block.text} />
        </div>
      );
    case "list":
      // 块类型即语义：纯行文本渲染前补列表标记（listBlockMarkdown 已含标记则
      // 原样）——列表块不再与段落渲染无别；GFM 表格/超链接等 markdown 语法由
      // MarkdownBody 全量支持（remark-gfm）。
      return <MarkdownBody text={listBlockMarkdown(block.text)} />;
    case "hr":
      return <hr className="my-1 border-border" />;
    default:
      return <MarkdownBody text={block.text} />;
  }
}

/** "…" 菜单：副本 / 归档|恢复 / 删除（导出在顶栏常驻——高频独立操作）。 */
function DocMenu({
  archived,
  busy,
  onDuplicate,
  onArchiveToggle,
  onDelete,
}: {
  archived: boolean;
  busy: string | null;
  onDuplicate: () => void;
  onArchiveToggle: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  return (
    <div ref={ref} className="relative shrink-0">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-haspopup="menu"
        title="更多操作"
        aria-label="更多操作"
        className={`rounded-lg p-2 transition-colors ${
          open ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
        }`}
      >
        <IconMore />
      </button>
      {open && (
        <div
          role="menu"
          className="animate-fade-in absolute right-0 top-full z-40 mt-1 w-44 overflow-hidden rounded-xl border border-border bg-surface py-1 shadow-xl"
        >
          <button
            type="button"
            role="menuitem"
            disabled={busy === "duplicate_doc"}
            onClick={() => {
              setOpen(false);
              onDuplicate();
            }}
            className="block w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
          >
            {busy === "duplicate_doc" ? "创建副本中…" : "创建副本"}
          </button>
          <button
            type="button"
            role="menuitem"
            disabled={busy === "archive_doc" || busy === "restore_doc"}
            onClick={() => {
              setOpen(false);
              onArchiveToggle();
            }}
            className="block w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
          >
            {archived ? "恢复文档" : "归档文档"}
          </button>
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              onDelete();
            }}
            className="block w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)] hover:text-danger"
          >
            删除文档…
          </button>
        </div>
      )}
    </div>
  );
}

function IconBack() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M19 12H5M12 19l-7-7 7-7" />
    </svg>
  );
}

function IconMore() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor">
      <circle cx="5" cy="12" r="1.6" />
      <circle cx="12" cy="12" r="1.6" />
      <circle cx="19" cy="12" r="1.6" />
    </svg>
  );
}
