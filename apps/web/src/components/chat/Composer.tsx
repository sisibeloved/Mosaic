// 输入框：自动增高 textarea（Enter 发送 / Shift+Enter 换行）；
// 输入 @ 弹成员补全（snapshot.participants 的 agent 项，键盘可选），选中成 chip
// （可点 × 移除；上限 3——契约 PostMessagePayload.addressed_to maxItems=3）。
import { useEffect, useRef, useState } from "react";
import type { ParticipantView } from "../../api/client";
import { adapterLabel } from "../../lib/copy";
import { Avatar } from "./Avatar";

const MAX_TARGETS = 3;

interface Mention {
  query: string;
  start: number; // "@" 在文本中的下标
  end: number; // 光标位（query 末）
}

/** 光标前最后一个 @token（空白或行首起、不含空白/@）。 */
function detectMention(value: string, caret: number): Mention | null {
  const m = /(?:^|\s)@([^\s@]*)$/.exec(value.slice(0, caret));
  if (!m) return null;
  return { query: m[1], start: caret - m[1].length - 1, end: caret };
}

export function Composer({
  disabled,
  paused,
  agents,
  onSend,
}: {
  disabled: boolean;
  paused: boolean;
  agents: ParticipantView[];
  onSend: (body: string, addressedTo: string[]) => void;
}) {
  const [body, setBody] = useState("");
  const [chips, setChips] = useState<ParticipantView[]>([]);
  const [mention, setMention] = useState<Mention | null>(null);
  const [highlight, setHighlight] = useState(0);
  const areaRef = useRef<HTMLTextAreaElement | null>(null);

  const blocked = disabled || paused;

  // 自动增高（上限 180px）
  useEffect(() => {
    const el = areaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 180)}px`;
  }, [body]);

  const candidates =
    mention === null || chips.length >= MAX_TARGETS
      ? []
      : agents
          .filter(
            (a) =>
              !chips.some((c) => c.participant_id === a.participant_id) &&
              (mention.query === "" ||
                a.display_name.toLowerCase().includes(mention.query.toLowerCase()) ||
                a.participant_id.toLowerCase().includes(mention.query.toLowerCase())),
          )
          .slice(0, 6);

  const pick = (target: ParticipantView) => {
    if (!mention || chips.length >= MAX_TARGETS) return;
    const next = `${body.slice(0, mention.start)}${body.slice(mention.end)}`;
    setBody(next);
    setChips((prev) => (prev.some((c) => c.participant_id === target.participant_id) ? prev : [...prev, target]));
    setMention(null);
    areaRef.current?.focus();
  };

  const submit = () => {
    const text = body.trim();
    if (!text || blocked) return;
    onSend(text, chips.map((c) => c.participant_id));
    setBody("");
    setChips([]);
    setMention(null);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (candidates.length > 0) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        setHighlight((h) => (h + (e.key === "ArrowDown" ? 1 : candidates.length - 1)) % candidates.length);
        return;
      }
      if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey)) {
        e.preventDefault();
        pick(candidates[Math.min(highlight, candidates.length - 1)]);
        return;
      }
      if (e.key === "Escape") {
        setMention(null);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="border-t border-border px-4 py-3">
      <div className="mx-auto max-w-3xl">
        {chips.length > 0 && (
          <div className="mb-1.5 flex flex-wrap gap-1.5">
            {chips.map((c) => (
              <span
                key={c.participant_id}
                className="flex items-center gap-1 rounded-full bg-accent-soft py-0.5 pl-1 pr-1.5 text-xs text-text"
              >
                <Avatar participantID={c.participant_id} displayName={c.display_name} size={16} />
                @{c.display_name}
                <button
                  type="button"
                  aria-label={`移除 ${c.display_name}`}
                  className="ml-0.5 rounded-full px-1 text-dim hover:text-text"
                  onClick={() => setChips((prev) => prev.filter((x) => x.participant_id !== c.participant_id))}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
        <div className="relative">
          {candidates.length > 0 && (
            <ul className="animate-fade-in absolute bottom-full left-0 mb-1.5 w-64 overflow-hidden rounded-xl border border-border bg-surface-2 py-1 shadow-lg">
              {candidates.map((a, i) => (
                <li key={a.participant_id}>
                  <button
                    type="button"
                    onMouseEnter={() => setHighlight(i)}
                    onClick={() => pick(a)}
                    className={`flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm ${
                      i === highlight ? "bg-surface-3" : ""
                    }`}
                  >
                    <Avatar participantID={a.participant_id} displayName={a.display_name} size={20} />
                    <span className="truncate">{a.display_name}</span>
                    {a.adapter && <span className="ml-auto text-[10px] text-faint">{adapterLabel(a.adapter)}</span>}
                  </button>
                </li>
              ))}
            </ul>
          )}
          <div className="flex items-end gap-2 rounded-2xl border border-border bg-surface-2 px-3 py-2 transition-colors focus-within:border-accent">
            <textarea
              ref={areaRef}
              rows={1}
              value={body}
              disabled={blocked}
              autoComplete="off"
              placeholder={
                paused
                  ? "房间已暂停——恢复后可继续发言"
                  : chips.length > 0
                    ? "输入消息…（将点名所选对象）"
                    : "输入消息，@ 点名 agent（最多 3 个）"
              }
              className="max-h-[180px] flex-1 resize-none bg-transparent text-sm outline-none placeholder:text-faint"
              onChange={(e) => {
                setBody(e.target.value);
                setMention(detectMention(e.target.value, e.target.selectionStart ?? e.target.value.length));
                setHighlight(0);
              }}
              onKeyDown={onKeyDown}
            />
            <button
              type="button"
              onClick={submit}
              disabled={blocked || !body.trim()}
              className="shrink-0 rounded-lg bg-accent px-3.5 py-1.5 text-sm font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              发送
            </button>
          </div>
        </div>
        <p className="mt-1 text-[11px] text-faint">Enter 发送 · Shift+Enter 换行 · 消息对所有参与者可见</p>
      </div>
    </div>
  );
}
