// 文档主页（RFC-0014 §2.8 首版）：工作区文档列表（updated_at 倒序）+ 创建者
// 筛选（我/各 agent——创建者集合由列表数据推导，名称经席位表解析）+ 全文检索
// （q≥3 字触发，FTS5 trigram；<3 字不请求——CJK 子串语义下限）+ 新建文档。
// 数据源 state/docs 列表 store（筛选态自持）；页面不轮询。
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { api, type DocSearchHit, type DocSummary } from "../api/client";
import { relativeTime, shortId, truncate } from "../lib/ui";
import { refreshDocs, setDocsCreatorFilter, useDocs } from "../state/docs";
import { refreshAgentSeats, useRooms } from "../state/rooms";

/** 本地 owner（ADR-0009 单 owner 形态；创建者筛选"我"的 participant_id）。 */
const OWNER_PID = "par_owner";
/** 检索触发下限（runes；服务端 trigram CJK ≥3 字子串语义）。 */
const SEARCH_MIN_RUNES = 3;

export function DocsHomePage() {
  const navigate = useNavigate();
  const location = useLocation();
  const { docs, error, createdBy } = useDocs();
  const { seats } = useRooms();
  // 编辑器删除/他端删除回跳提示（一次性——读后即清 location.state 防刷新重现）。
  const [notice, setNotice] = useState<string | null>(
    (location.state as { notice?: string } | null)?.notice ?? null,
  );
  useEffect(() => {
    if (notice) window.history.replaceState({}, "");
  }, [notice]);
  const [creating, setCreating] = useState(false);
  const [createTitle, setCreateTitle] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<DocSearchHit[] | null>(null);
  const [searchError, setSearchError] = useState<string | null>(null);
  const searchSeq = useRef(0);

  useEffect(() => {
    void refreshDocs();
    void refreshAgentSeats();
  }, []);

  // 全文检索：防抖 300ms；q<3 字回列表态；序号兜底丢弃迟到响应。
  useEffect(() => {
    const q = query.trim();
    if ([...q].length < SEARCH_MIN_RUNES) {
      setHits(null);
      setSearchError(null);
      return;
    }
    const t = setTimeout(() => {
      const seq = ++searchSeq.current;
      api
        .searchDocs(q)
        .then((d) => {
          if (searchSeq.current !== seq) return;
          setHits(d.hits);
          setSearchError(null);
        })
        .catch((e) => {
          if (searchSeq.current !== seq) return;
          setHits(null);
          setSearchError(e instanceof Error ? e.message : String(e));
        });
    }, 300);
    return () => clearTimeout(t);
  }, [query]);

  /** 创建者显示名（par_owner → 我；agent → 席位显示名；其余 → 短 id）。 */
  const creatorName = (pid: string): string => {
    if (pid === OWNER_PID) return "我";
    const seat = (seats ?? []).find((s) => s.participant_id === pid);
    return seat?.display_name ?? shortId(pid);
  };

  // 创建者筛选芯片：全部 / 我 / 列表中出现的 agent 创建者（按显示名序）。
  const creators = useMemo(() => {
    const set = new Set<string>();
    for (const d of docs ?? []) set.add(d.created_by);
    return [...set].filter((p) => p !== OWNER_PID).sort((a, b) => creatorName(a).localeCompare(creatorName(b)));
    // eslint-disable-next-line react-hooks/exhaustive-deps -- seats 到位后名称变化须重排
  }, [docs, seats]);

  const rows = useMemo(
    () => [...(docs ?? [])].sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1)),
    [docs],
  );

  const submitCreate = async () => {
    const title = createTitle.trim();
    if (!title || createBusy) return;
    setCreateBusy(true);
    setCreateError(null);
    try {
      const created = await api.createDoc(title);
      void refreshDocs();
      navigate(`/docs/${encodeURIComponent(created.doc_id)}`);
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : String(e));
      setCreateBusy(false);
    }
  };

  const searching = hits !== null || searchError !== null;

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center gap-3 border-b border-border px-5 py-3">
        <h1 className="text-sm font-medium tracking-tight">文档</h1>
        <div className="flex-1" />
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="搜索文档（≥3 字）…"
          aria-label="搜索文档"
          className="w-64 rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none placeholder:text-faint focus:border-accent"
        />
        <button
          type="button"
          onClick={() => {
            setCreateTitle("");
            setCreateError(null);
            setCreating(true);
          }}
          className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-accent-contrast transition-opacity hover:opacity-90"
        >
          新建文档
        </button>
      </header>

      <div className="flex items-center gap-1.5 border-b border-border px-5 py-2 text-xs">
        <span className="text-faint">创建者</span>
        <FilterChip active={createdBy === null} onClick={() => void setDocsCreatorFilter(null)}>
          全部
        </FilterChip>
        <FilterChip active={createdBy === OWNER_PID} onClick={() => void setDocsCreatorFilter(OWNER_PID)}>
          我
        </FilterChip>
        {creators.map((pid) => (
          <FilterChip key={pid} active={createdBy === pid} onClick={() => void setDocsCreatorFilter(pid)}>
            {creatorName(pid)}
          </FilterChip>
        ))}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-5 py-3">
        {notice && (
          <div className="mx-auto mb-2 flex max-w-3xl items-center gap-2 rounded-lg border border-border bg-surface-2 px-3 py-2 text-xs text-dim">
            <span className="min-w-0 flex-1">{notice}</span>
            <button
              type="button"
              onClick={() => setNotice(null)}
              aria-label="关闭提示"
              className="shrink-0 rounded px-1 text-faint hover:text-text"
            >
              ×
            </button>
          </div>
        )}
        {searching ? (
          <SearchResults hits={hits} error={searchError} query={query.trim()} docs={docs} />
        ) : docs === null ? (
          <p className="py-16 text-center text-sm text-faint">加载中…</p>
        ) : error ? (
          <p className="py-16 text-center text-sm text-danger">{error}</p>
        ) : rows.length === 0 ? (
          <div className="py-16 text-center text-sm text-faint">
            <p>{createdBy ? "该创建者暂无文档。" : "还没有文档。"}</p>
            <p className="mt-1 text-xs">点右上"新建文档"，或在房间里让 agent 把结论写成文档。</p>
          </div>
        ) : (
          <ul className="mx-auto flex max-w-3xl flex-col gap-1.5">
            {rows.map((d) => (
              <DocRow key={d.doc_id} doc={d} nameOf={creatorName} />
            ))}
          </ul>
        )}
      </div>

      {creating && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          role="dialog"
          aria-modal="true"
          aria-label="新建文档"
          onClick={() => !createBusy && setCreating(false)}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !createBusy) setCreating(false);
          }}
        >
          <div
            className="mx-4 w-full max-w-md rounded-2xl border border-border bg-surface p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-sm font-semibold text-text">新建文档</h2>
            <input
              autoFocus
              value={createTitle}
              onChange={(e) => setCreateTitle(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void submitCreate();
              }}
              maxLength={200}
              disabled={createBusy}
              placeholder="文档标题"
              aria-label="文档标题"
              className="mt-3 w-full rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none placeholder:text-faint focus:border-accent"
            />
            {createError && <p className="mt-2 text-xs text-danger">{createError}</p>}
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                disabled={createBusy}
                onClick={() => setCreating(false)}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
              >
                取消
              </button>
              <button
                type="button"
                disabled={createBusy || !createTitle.trim()}
                onClick={() => void submitCreate()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {createBusy ? "创建中…" : "创建"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function FilterChip({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-full px-2.5 py-1 transition-colors ${
        active ? "bg-accent-soft text-accent" : "text-dim hover:bg-surface-2 hover:text-text"
      }`}
    >
      {children}
    </button>
  );
}

function DocRow({ doc, nameOf }: { doc: DocSummary; nameOf: (pid: string) => string }) {
  return (
    <li>
      <Link
        to={`/docs/${encodeURIComponent(doc.doc_id)}`}
        className="flex items-center gap-3 rounded-xl border border-border bg-surface px-3.5 py-2.5 transition-colors hover:bg-surface-2"
      >
        <span className="shrink-0 text-dim">
          <IconDoc />
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-2">
            <span className="truncate text-sm font-medium text-text">{doc.title}</span>
            {doc.status === "archived" && (
              <span className="shrink-0 rounded bg-surface-3 px-1.5 text-[10px] leading-4 text-dim">已归档</span>
            )}
          </span>
          <span className="mt-0.5 block text-[11px] text-faint">
            {nameOf(doc.created_by)} 创建 · {nameOf(doc.updated_by)} 更新于 {relativeTime(doc.updated_at)}
          </span>
        </span>
        <span className="shrink-0 rounded bg-surface-3 px-1.5 py-px text-[10px] leading-4 text-dim">v{doc.version}</span>
      </Link>
    </li>
  );
}

function SearchResults({
  hits,
  error,
  query,
  docs,
}: {
  hits: DocSearchHit[] | null;
  error: string | null;
  query: string;
  docs: DocSummary[] | null;
}) {
  if (error) return <p className="py-16 text-center text-sm text-danger">{error}</p>;
  if (hits === null) return <p className="py-16 text-center text-sm text-faint">检索中…</p>;
  if (hits.length === 0) {
    return <p className="py-16 text-center text-sm text-faint">没有匹配「{truncate(query, 24)}」的文档。</p>;
  }
  // 命中行只带被命中字段（标题命中 → title；正文命中 → body、title 为空）：
  // 空标题回退到列表 store 里的现标题，再退 doc_id。
  const titleOf = (h: DocSearchHit): string =>
    h.title || (docs ?? []).find((d) => d.doc_id === h.doc_id)?.title || h.doc_id;
  return (
    <ul className="mx-auto flex max-w-3xl flex-col gap-1.5">
      {hits.map((h) => (
        <li key={h.doc_id}>
          <Link
            to={`/docs/${encodeURIComponent(h.doc_id)}`}
            className="block rounded-xl border border-border bg-surface px-3.5 py-2.5 transition-colors hover:bg-surface-2"
          >
            <span className="flex items-center gap-2">
              <span className="min-w-0 flex-1 truncate text-sm font-medium text-text">{titleOf(h)}</span>
              <span className="shrink-0 rounded bg-surface-3 px-1.5 py-px text-[10px] leading-4 text-dim">v{h.version}</span>
            </span>
            <span className="mt-0.5 block truncate text-[11px] text-dim">{truncate(h.body.replace(/\s+/g, " ").trim(), 90)}</span>
            <span className="mt-0.5 block text-[10px] text-faint">{relativeTime(h.occurred_at)}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

function IconDoc() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <path d="M14 2v6h6M16 13H8M16 17H8M10 9H8" />
    </svg>
  );
}
