// 输入框：自动增高 textarea（Enter 发送 / Shift+Enter 换行）；
// 输入 @ 弹成员补全（snapshot.participants 的 agent 项，键盘可选），选中成 chip
// （可点 × 移除；上限 3——契约 PostMessagePayload.addressed_to maxItems=3）。
// M4-0 引用回复：quoted 非空时输入区顶部渲染引用卡片（作者 + 摘要，× 取消），
// 发送携带 reply_to（message.posted 既有载荷字段）。
import { useEffect, useRef, useState } from "react";
import type { ParticipantView } from "../../api/client";
import { adapterLabel } from "../../lib/copy";
import { Avatar } from "./Avatar";

const MAX_TARGETS = 3;

/**
 * 剪贴板粘贴图片（RFC-0013 附件面）：paste 事件的 clipboardData.items 里
 * 截图/复制的图像以 kind=file、image/* 出现（Win+Shift+S、网页复制图均此形态，
 * WebView2/Chromium 系全支持）。命名带时间戳（剪贴板 Blob 本无名或恒 image.png）。
 */
function imagesFromClipboard(dt: DataTransfer | null): File[] {
  if (!dt) return [];
  const out: File[] = [];
  for (const item of dt.items) {
    if (item.kind !== "file" || !item.type.startsWith("image/")) continue;
    const f = item.getAsFile();
    if (!f) continue;
    const ext = f.type === "image/jpeg" ? "jpg" : (f.type.split("/")[1] || "png");
    const ts = new Date();
    const pad = (n: number) => String(n).padStart(2, "0");
    const name = `截图-${ts.getFullYear()}${pad(ts.getMonth() + 1)}${pad(ts.getDate())}-${pad(ts.getHours())}${pad(ts.getMinutes())}${pad(ts.getSeconds())}.${ext}`;
    out.push(new File([f], name, { type: f.type }));
  }
  return out;
}

/** 引用回复的目标消息（RoomPage 自时间线条目构造）。 */
export interface QuotedMessage {
  eventID: string;
  author: string;
  excerpt: string;
}

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

/** 外部点名注入（v1.73 右键"引用并 @ 作者"/成员行 @）：nonce 递增保证同目标
 * 重复注入也能触发 effect；消费后由 onMentionConsumed 清零。 */
export interface MentionRequest {
  pid: string;
  nonce: number;
}

export function Composer({
  disabled,
  paused,
  agents,
  quoted,
  onCancelQuote,
  attachments,
  onAddAttachment,
  onRemoveAttachment,
  mentionRequest,
  onMentionConsumed,
  onSend,
}: {
  disabled: boolean;
  paused: boolean;
  agents: ParticipantView[];
  /** 引用回复目标（null = 普通发言）；发送后由调用方清除。 */
  quoted: QuotedMessage | null;
  onCancelQuote: () => void;
  /** RFC-0013 附件：已上传待发送（token + 展示元数据）；上传由 RoomPage 经 api 发起。 */
  attachments: { token: string; name: string; sizeBytes: number }[];
  onAddAttachment: (file: File) => void;
  onRemoveAttachment: (token: string) => void;
  mentionRequest: MentionRequest | null;
  onMentionConsumed: () => void;
  onSend: (body: string, addressedTo: string[], replyTo: string | null, attachments: string[]) => void;
}) {
  const [body, setBody] = useState("");
  const [chips, setChips] = useState<ParticipantView[]>([]);
  const [mention, setMention] = useState<Mention | null>(null);
  const [highlight, setHighlight] = useState(0);
  const areaRef = useRef<HTMLTextAreaElement | null>(null);

  const blocked = disabled || paused;

  // 外部点名注入：右键/成员行 → chips（去重 + 上限 3，与 @ 补全同门）。
  useEffect(() => {
    if (!mentionRequest) return;
    const target = agents.find((a) => a.participant_id === mentionRequest.pid);
    if (target) {
      setChips((prev) =>
        prev.some((c) => c.participant_id === target.participant_id) || prev.length >= MAX_TARGETS
          ? prev
          : [...prev, target],
      );
      areaRef.current?.focus();
    }
    onMentionConsumed();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- 注入按 nonce 触发一次性副作用
  }, [mentionRequest]);

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
    onSend(text, chips.map((c) => c.participant_id), quoted?.eventID ?? null, attachments.map((a) => a.token));
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
    <div className="border-t border-border py-3 pl-4 pr-6">
      <div className="mx-auto max-w-3xl">
        {quoted && (
          <div className="mb-1.5 flex items-start gap-2 rounded-lg border-l-2 border-accent/60 bg-surface-2 px-2.5 py-1.5 text-xs text-dim">
            <div className="min-w-0 flex-1">
              <span className="text-faint">回复 {quoted.author}：</span>
              <span className="block truncate">{quoted.excerpt || "（空消息）"}</span>
            </div>
            <button
              type="button"
              aria-label="取消引用"
              title="取消引用"
              onClick={onCancelQuote}
              className="shrink-0 rounded-full px-1.5 leading-4 text-dim transition-colors hover:bg-surface-3 hover:text-text"
            >
              ×
            </button>
          </div>
        )}
        {attachments.length > 0 && (
          <div className="mb-1.5 flex flex-wrap gap-1.5">
            {attachments.map((a) => (
              <span key={a.token} className="flex items-center gap-1 rounded-full bg-surface-3 py-0.5 pl-2 pr-1.5 text-xs text-dim">
                📎 {a.name}（{(a.sizeBytes / 1024).toFixed(0)} KiB）
                <button
                  type="button"
                  aria-label={`移除附件 ${a.name}`}
                  onClick={() => onRemoveAttachment(a.token)}
                  className="rounded-full px-1 text-dim hover:text-text"
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
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
          <div className="flex items-center gap-2 rounded-2xl border border-border bg-surface-2 px-3 py-2 transition-colors focus-within:border-accent">
            <label
              title="上传附件（≤8MiB，随消息发送）"
              className="shrink-0 cursor-pointer text-faint transition-colors hover:text-text"
            >
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
                <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
              </svg>
              <input
                type="file"
                multiple
                className="hidden"
                disabled={blocked}
                onChange={(e) => {
                  for (const f of e.target.files ?? []) onAddAttachment(f);
                  e.target.value = "";
                }}
              />
            </label>
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
              onPaste={(e) => {
                const imgs = imagesFromClipboard(e.clipboardData);
                if (imgs.length === 0) return;
                const hasText = Array.from(e.clipboardData.items).some((it) => it.kind === "string");
                if (!hasText) e.preventDefault(); // 纯图剪贴板：不向输入框贴空文本
                for (const f of imgs) onAddAttachment(f);
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
