import { useEffect, useRef } from "react";
import type { TimelineEntry } from "../api/room";

export function Timeline({ entries }: { entries: TimelineEntry[] }) {
  const bottomRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [entries.length]);

  return (
    <div className="timeline" role="log" aria-label="讨论时间线">
      {entries.length === 0 && (
        <div className="hint">发送第一条消息——agent 会自动参与讨论。</div>
      )}
      {entries.map((e) => {
        if (e.kind === "message") {
          return (
            <div key={e.key} className={`msg ${e.actorKind}`}>
              <div className="meta">
                {e.actorID} · {e.occurredAt}
              </div>
              <div className="body">{e.body}</div>
            </div>
          );
        }
        const label =
          e.kind === "round-open"
            ? "— 轮开启 —"
            : e.kind === "round-close"
              ? `— 轮结束（${e.detail}）—`
              : e.kind === "pause"
                ? "— 房间已暂停 —"
                : "— 房间已恢复 —";
        return (
          <div key={e.key} className="round-marker">
            {label}
          </div>
        );
      })}
      <div ref={bottomRef} />
    </div>
  );
}
