// 房间面板"文档"段（RFC-0014 §2.4 房间文档附着，MemoryTab 先例平移）：
// 附着清单（标题/版本/附着者与时间——附着不复制不锁定，同一文档可附着多房间）
// + 附着操作（工作区文档挑选器 / 新建文档并附着）+ 解除附着。
// 标题数据源 = state/docs 元信息缓存（快照订阅——附着段不需要卡片级 SSE）。
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type AttachedDoc, type ParticipantView } from "../../api/client";
import { displayNameOf, relativeTime, truncate } from "../../lib/ui";
import { refreshDocs, useDocMeta, useDocs } from "../../state/docs";

export function DocsTab({
  attachedDocs,
  participants,
  busy,
  onAttach,
  onDetach,
}: {
  /** 快照 docs 投影（附着事实；标题经元信息缓存按 doc_id 惰性解析）。 */
  attachedDocs: AttachedDoc[];
  participants: ParticipantView[];
  /** 进行中的附着/解除目标（doc_id；命令链版本校准+409 重试在 room.ts 内）。 */
  busy: string | null;
  onAttach: (docID: string) => void;
  onDetach: (docID: string) => void;
}) {
  const [picking, setPicking] = useState(false);

  return (
    <div className="py-1">
      <div className="flex items-center justify-between px-3 pb-1 pt-1">
        <h3 className="text-xs font-medium text-dim">附着文档（{attachedDocs.length}）</h3>
        <button
          type="button"
          onClick={() => setPicking((v) => !v)}
          aria-expanded={picking}
          className="rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          + 附着
        </button>
      </div>
      <p className="px-3 pb-1.5 text-[11px] leading-4 text-faint">
        附着的文档会进入本房间的讨论语境（agent 评估/生成时可见摘录）；附着不复制、不锁定。
      </p>
      {picking && (
        <AttachPicker
          attachedIDs={attachedDocs.map((d) => d.doc_id)}
          busy={busy}
          onAttach={onAttach}
          onCreated={(docID) => onAttach(docID)}
        />
      )}
      {attachedDocs.length === 0 ? (
        <p className="px-3 py-3 text-xs text-faint">暂无附着文档——点"+ 附着"把工作区文档带入本房间。</p>
      ) : (
        <ul className="py-1">
          {attachedDocs.map((d) => (
            <AttachedRow
              key={d.doc_id}
              item={d}
              participants={participants}
              busy={busy === d.doc_id}
              onDetach={onDetach}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

/** 附着行：标题/状态徽标（元信息缓存）+ 附着者/时间（快照投影）+ 打开/解除。 */
function AttachedRow({
  item,
  participants,
  busy,
  onDetach,
}: {
  item: AttachedDoc;
  participants: ParticipantView[];
  busy: boolean;
  onDetach: (docID: string) => void;
}) {
  const navigate = useNavigate();
  const { doc, missing } = useDocMeta(item.doc_id);
  const attacher = item.attached_by === "par_owner" ? "我" : displayNameOf(participants, item.attached_by);
  return (
    <li className="flex items-center gap-2 px-3 py-2 text-xs">
      <span className="shrink-0 text-dim">
        <IconDoc />
      </span>
      <div className="min-w-0 flex-1">
        <button
          type="button"
          onClick={() => navigate(`/docs/${encodeURIComponent(item.doc_id)}`)}
          title="打开文档"
          className="block max-w-full truncate text-left text-text hover:text-accent hover:underline"
        >
          {missing ? "（文档已删除）" : (doc?.title ?? "加载中…")}
        </button>
        <span className="text-[11px] text-faint">
          {doc ? `v${doc.version} · ` : ""}
          {attacher} 附着于 {relativeTime(item.attached_at)}
          {doc?.status === "archived" ? " · 已归档" : ""}
        </span>
      </div>
      <button
        type="button"
        disabled={busy}
        onClick={() => onDetach(item.doc_id)}
        title="解除附着（文档本身不受影响）"
        className="shrink-0 rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
      >
        {busy ? "解除中…" : "解除"}
      </button>
    </li>
  );
}

/** 附着挑选器：工作区文档过滤列表（客户端过滤）+ 新建文档并附着。 */
function AttachPicker({
  attachedIDs,
  busy,
  onAttach,
  onCreated,
}: {
  attachedIDs: string[];
  busy: string | null;
  onAttach: (docID: string) => void;
  onCreated: (docID: string) => void;
}) {
  const navigate = useNavigate();
  const { docs, error } = useDocs();
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  useEffect(() => {
    void refreshDocs();
  }, []);

  const attached = new Set(attachedIDs);
  const q = query.trim().toLowerCase();
  const candidates = (docs ?? []).filter(
    (d) => !attached.has(d.doc_id) && (q === "" || d.title.toLowerCase().includes(q) || d.doc_id.includes(q)),
  );

  const createAndAttach = async () => {
    const title = query.trim();
    if (!title || creating) return;
    setCreating(true);
    setCreateError(null);
    try {
      const created = await api.createDoc(title);
      onCreated(created.doc_id);
      navigate(`/docs/${encodeURIComponent(created.doc_id)}`);
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : String(e));
      setCreating(false);
    }
  };

  return (
    <div className="mx-2 mb-2 rounded-lg border border-border bg-surface-2 p-2">
      <input
        autoFocus
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="搜索文档标题，或输入新文档名…"
        aria-label="搜索或新建文档"
        className="w-full rounded-lg border border-border bg-surface px-2 py-1 text-xs outline-none placeholder:text-faint focus:border-accent"
      />
      {error && <p className="mt-1 px-1 text-[11px] text-danger">{error}</p>}
      {createError && <p className="mt-1 px-1 text-[11px] text-danger">{createError}</p>}
      <ul className="mt-1 max-h-48 overflow-y-auto">
        {docs === null ? (
          <li className="px-1 py-1.5 text-[11px] text-faint">文档列表加载中…</li>
        ) : candidates.length === 0 && q === "" ? (
          <li className="px-1 py-1.5 text-[11px] text-faint">没有可附着的文档。</li>
        ) : (
          candidates.slice(0, 20).map((d) => (
            <li key={d.doc_id}>
              <button
                type="button"
                disabled={busy === d.doc_id}
                onClick={() => onAttach(d.doc_id)}
                className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-xs transition-colors hover:bg-surface-3 disabled:opacity-40"
              >
                <span className="min-w-0 flex-1 truncate text-text">{d.title}</span>
                <span className="shrink-0 text-[10px] text-faint">
                  v{d.version} · {relativeTime(d.updated_at)}
                </span>
              </button>
            </li>
          ))
        )}
      </ul>
      {q !== "" && (
        <button
          type="button"
          disabled={creating}
          onClick={() => void createAndAttach()}
          className="mt-1 w-full rounded-lg bg-accent-soft px-2 py-1.5 text-left text-xs text-accent transition-opacity hover:opacity-85 disabled:opacity-40"
        >
          {creating ? "创建中…" : `新建文档「${truncate(query.trim(), 24)}」并附着`}
        </button>
      )}
    </div>
  );
}

function IconDoc() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <path d="M14 2v6h6M16 13H8M16 17H8M10 9H8" />
    </svg>
  );
}
