// 房内检索浮层（v1.71 从记忆 Tab 上移——检索是房间级功能，不隶属记忆）：
// 顶栏搜索按钮展开，回车检索（FTS5 trigram），命中点击跳转时间线并关闭；
// ESC 关闭。空态/错误如实呈现。
import { useEffect, useRef, useState } from "react";
import { api, type SearchHit } from "../../api/client";
import { truncate } from "../../lib/ui";

export function RoomSearch({
  roomID,
  onJumpToEvent,
  onClose,
}: {
  roomID: string;
  onJumpToEvent: (eventID: string) => void;
  onClose: () => void;
}) {
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<SearchHit[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const run = () => {
    const query = q.trim();
    if (!query || busy) return;
    setBusy(true);
    setError(null);
    api
      .searchMessages(roomID, query)
      .then((d) => setHits(d.hits ?? []))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false));
  };

  return (
    <div
      className="animate-fade-in absolute left-0 right-0 top-full z-30 border-b border-border bg-surface px-4 py-2 shadow-lg"
      role="search"
      onKeyDown={(e) => {
        if (e.key === "Escape") onClose();
      }}
    >
      <div className="mx-auto flex max-w-3xl gap-2">
        <div className="flex min-w-0 flex-1 items-center gap-2 rounded-xl border border-border bg-surface-2 px-3 py-1.5 focus-within:border-accent">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className="shrink-0 text-faint">
            <circle cx="11" cy="11" r="7" />
            <path d="M21 21l-4.35-4.35" />
          </svg>
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") run();
            }}
            placeholder="搜索房内消息（关键词，如：预算 超限）"
            aria-label="搜索房内消息"
            className="min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-faint"
          />
        </div>
        <button
          type="button"
          disabled={busy || !q.trim()}
          onClick={run}
          className="shrink-0 rounded-xl bg-accent px-3.5 py-1.5 text-sm font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {busy ? "搜索中…" : "搜索"}
        </button>
        <button
          type="button"
          onClick={onClose}
          aria-label="关闭搜索"
          className="shrink-0 rounded-xl px-2 py-1.5 text-sm text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          ESC
        </button>
      </div>
      {(error || hits !== null) && (
        <div className="mx-auto mt-2 max-w-3xl">
          {error && <p className="text-xs text-danger">{error}</p>}
          {hits !== null && (
            <ul className="max-h-64 overflow-y-auto rounded-xl border border-border bg-surface-2 py-1">
              {hits.length === 0 && <li className="px-3 py-2 text-xs text-faint">无命中。</li>}
              {hits.map((h) => (
                <li key={h.event_id}>
                  <button
                    type="button"
                    onClick={() => {
                      onJumpToEvent(h.event_id);
                      onClose();
                    }}
                    className="w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-surface-3 hover:text-text"
                  >
                    <span className="font-medium text-text">{h.actor}</span>：{truncate(h.body, 72)}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
