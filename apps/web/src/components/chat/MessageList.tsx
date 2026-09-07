// 消息流（参考飞书 / Grok Bot 群聊）：人类右侧气泡带"我"头像（accent 色、省名字行）；
// agent 左侧带头像/显示名/类别徽标；system 事件居中细灰条。用户上翻时暂停自动滚底。
// M4-0 聊天交互补齐：气泡 hover 动作条（复制原文 / 引用回复）；带 reply_to 的消息
// 气泡顶部渲染引用条（作者 + 摘要，点击跳原消息）。
import { useEffect, useMemo, useRef, useState } from "react";
import type { TimelineEntry } from "../../api/room";
import type { ParticipantView } from "../../api/client";
import { adapterLabel, channelLabel, kindLabel } from "../../lib/copy";
import { absoluteTime, displayNameOf, participantOf, relativeTime, truncate } from "../../lib/ui";
import { copyText } from "../../lib/clipboard";
import { Avatar } from "./Avatar";
import { MarkdownBody } from "./MarkdownBody";

/** 引用条摘要长度（字符数，与面板 describeEvent 的 24 字摘要区分——条内更宽松）。 */
const QUOTE_EXCERPT = 64;

interface TodoLine {
  text: string;
  done: boolean;
}

/**
 * 剥离 body 里的 mosaic-todo 申报块（v1.49 协议）：围栏块是机器协议载荷而非
 * 聊天内容，气泡里渲染为独立的申报芯片；正文其余部分照常 Markdown 渲染。
 * 与服务端 tasklist.go 同语义：最后一个块生效、未闭合容忍、零有效行不构成申报。
 */
function splitTodoBlock(body: string): { clean: string; todos: TodoLine[] | null } {
  const lines = body.split("\n");
  const clean: string[] = [];
  let todos: TodoLine[] | null = null;
  let current: TodoLine[] | null = null;
  for (const ln of lines) {
    const t = ln.trim();
    if (current === null) {
      if (t.startsWith("```mosaic-todo")) {
        current = [];
        continue;
      }
      clean.push(ln);
      continue;
    }
    if (t.startsWith("```")) {
      if (current.length > 0) todos = current; // 块闭合：零有效行不构成申报
      current = null;
      continue;
    }
    const m = /^[-*+]\s*\[([ xX])\]\s*(.+)$/.exec(t);
    if (m) current.push({ done: m[1].toLowerCase() === "x", text: m[2].trim() });
  }
  if (current !== null && current.length > 0) todos = current; // 未闭合兜底
  return { clean: clean.join("\n").trimEnd(), todos };
}

/** 申报芯片：agent 回复里的任务申报渲染为紧凑清单（协议可视化，非正文）。 */
function TodoChip({ items }: { items: TodoLine[] }) {
  return (
    <div className="mt-1.5 rounded-lg border border-border bg-surface-3/60 px-2.5 py-1.5 text-[11px] leading-5 text-dim">
      <span className="text-faint">任务申报</span>
      <ul className="mt-0.5">
        {items.map((it, i) => (
          <li key={i}>
            {it.done ? "✓" : "○"} {it.text}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** 引用条：被引消息在气泡顶部的可点击摘要（作者 + 摘要；点击滚到原消息）。 */
function ReplyQuoteBar({
  target,
  fallbackID,
  participants,
  onJump,
}: {
  target: TimelineEntry | null;
  fallbackID: string;
  participants: ParticipantView[];
  onJump: (eventID: string) => void;
}) {
  if (!target || target.kind !== "message") {
    // 目标不在已载条目内（理论上快照全量载入不发生；防御性回退显示事件 id 尾段）
    return (
      <div className="mb-1.5 rounded-lg border-l-2 border-border bg-surface-3/50 px-2 py-1 text-[11px] text-faint">
        引用的消息不在当前视图
      </div>
    );
  }
  const author = displayNameOf(participants, target.actorID);
  const summary = truncate((target.body ?? "").replace(/\s+/g, " ").trim(), QUOTE_EXCERPT) || "（空消息）";
  return (
    <button
      type="button"
      onClick={() => onJump(fallbackID)}
      title="跳到被引消息"
      className="mb-1.5 block w-full truncate rounded-lg border-l-2 border-accent/60 bg-surface-3/50 px-2 py-1 text-left text-[11px] leading-5 text-dim transition-colors hover:bg-surface-3"
    >
      <span className="text-faint">回复 {author}：</span>
      {summary}
    </button>
  );
}

/** 气泡 hover 动作条：复制原文（含协议块原文）+ 引用回复。飞书式——常驻占位透明，hover 显形。 */
function BubbleActions({
  entry,
  onQuote,
}: {
  entry: TimelineEntry;
  onQuote: (entry: TimelineEntry) => void;
}) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);
  useEffect(() => () => {
    if (timerRef.current) window.clearTimeout(timerRef.current);
  }, []);
  const doCopy = async () => {
    // 复制的是原文（entry.body 全量，含 mosaic-todo 块）——所见即所发。
    const ok = await copyText(entry.body ?? "");
    if (ok) {
      setCopied(true);
      if (timerRef.current) window.clearTimeout(timerRef.current);
      timerRef.current = window.setTimeout(() => setCopied(false), 1400);
    }
  };
  return (
    <div className="flex items-center gap-0.5 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
      <button
        type="button"
        onClick={() => void doCopy()}
        aria-label="复制消息原文"
        title="复制原文"
        className="rounded-md px-1.5 py-0.5 text-[11px] text-faint transition-colors hover:bg-surface-3 hover:text-text"
      >
        {copied ? "已复制" : "复制"}
      </button>
      <button
        type="button"
        onClick={() => onQuote(entry)}
        aria-label="引用该消息回复"
        title="引用回复"
        className="rounded-md px-1.5 py-0.5 text-[11px] text-faint transition-colors hover:bg-surface-3 hover:text-text"
      >
        引用
      </button>
    </div>
  );
}

export function MessageList({
  entries,
  participants,
  onQuote,
  onJumpToEvent,
}: {
  entries: TimelineEntry[];
  participants: ParticipantView[];
  /** 点"引用"：把该消息带入输入框上方的引用卡片（RoomPage 持状态）。 */
  onQuote: (entry: TimelineEntry) => void;
  /** 引用条 / 跳转：滚到对应事件（RoomPage 的 onJumpToEvent——data-event-id 锚定）。 */
  onJumpToEvent: (eventID: string) => void;
}) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const pinnedRef = useRef(true); // 贴底时才跟随新消息滚动

  useEffect(() => {
    const el = boxRef.current;
    if (el && pinnedRef.current) el.scrollTop = el.scrollHeight;
  }, [entries]);

  const onScroll = () => {
    const el = boxRef.current;
    if (!el) return;
    pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  // 引用解析索引（event_id → 条目）：快照全量载入下被引消息总在；缺则渲染降级文案。
  const entryIndex = useMemo(() => {
    const m = new Map<string, TimelineEntry>();
    for (const e of entries) m.set(e.key, e);
    return m;
  }, [entries]);

  return (
    <div
      ref={boxRef}
      onScroll={onScroll}
      className="flex-1 overflow-y-auto px-4 py-4 [scrollbar-gutter:stable]"
      role="log"
      aria-label="讨论时间线"
    >
      <div className="mx-auto flex max-w-3xl flex-col gap-3">
        {entries.length === 0 && (
          <p className="py-16 text-center text-sm text-faint">
            发送第一条消息——agent 会自行评估是否参与讨论。
          </p>
        )}
        {entries.map((e) =>
          e.kind === "system" ? (
            <SystemBar key={e.key} text={e.detail ?? ""} time={absoluteTime(e.occurredAt)} />
          ) : e.actorKind === "human" ? (
            <div key={e.key} data-event-id={e.key} className="group rounded-xl">
              <HumanBubble
                entry={e}
                participants={participants}
                quoteTarget={e.replyTo ? (entryIndex.get(e.replyTo) ?? null) : null}
                onJump={onJumpToEvent}
                onQuote={onQuote}
              />
            </div>
          ) : (
            <div key={e.key} data-event-id={e.key} className="group rounded-xl">
              <AgentBubble
                entry={e}
                participants={participants}
                quoteTarget={e.replyTo ? (entryIndex.get(e.replyTo) ?? null) : null}
                onJump={onJumpToEvent}
                onQuote={onQuote}
              />
            </div>
          ),
        )}
      </div>
    </div>
  );
}

// 系统条带绝对时间（v1.49 长静默排障：[dev] 条目只有序没有钟，分不清何时发生）。
function SystemBar({ text, time }: { text: string; time: string }) {
  return (
    <div className="my-1 flex items-center gap-3 text-[11px] text-faint">
      <span className="h-px flex-1 bg-border" />
      <span className="shrink-0">
        {time && <span className="mr-1.5 tabular-nums">{time}</span>}
        {text}
      </span>
      <span className="h-px flex-1 bg-border" />
    </div>
  );
}

function AddressedLine({
  entry,
  participants,
}: {
  entry: TimelineEntry;
  participants: ParticipantView[];
}) {
  if (!entry.addressedTo || entry.addressedTo.length === 0) return null;
  const names = entry.addressedTo.map((id) => `@${displayNameOf(participants, id)}`).join(" ");
  return <span className="mr-2 text-accent">{names}</span>;
}

function HumanBubble({
  entry,
  participants,
  quoteTarget,
  onJump,
  onQuote,
}: {
  entry: TimelineEntry;
  participants: ParticipantView[];
  quoteTarget: TimelineEntry | null;
  onJump: (eventID: string) => void;
  onQuote: (entry: TimelineEntry) => void;
}) {
  // 头像在最右（外侧），气泡在头像左侧（内侧）；名字行省略——自己知道自己。
  return (
    <div className="animate-rise flex justify-end gap-2.5">
      <div className="flex max-w-[80%] flex-col items-end">
        <div className="rounded-2xl rounded-br-md bg-accent-soft px-3.5 py-2 text-sm text-text">
          {entry.replyTo && (
            <ReplyQuoteBar target={quoteTarget} fallbackID={entry.replyTo} participants={participants} onJump={onJump} />
          )}
          <MarkdownBody text={entry.body ?? ""} />
        </div>
        <div className="mt-0.5 flex items-center gap-1.5 text-[11px] text-faint">
          <AddressedLine entry={entry} participants={participants} />
          {relativeTime(entry.occurredAt)}
          <BubbleActions entry={entry} onQuote={onQuote} />
        </div>
      </div>
      <Avatar participantID={entry.actorID} displayName="我" color="var(--accent)" />
    </div>
  );
}

function AgentBubble({
  entry,
  participants,
  quoteTarget,
  onJump,
  onQuote,
}: {
  entry: TimelineEntry;
  participants: ParticipantView[];
  quoteTarget: TimelineEntry | null;
  onJump: (eventID: string) => void;
  onQuote: (entry: TimelineEntry) => void;
}) {
  const p = participantOf(participants, entry.actorID);
  const name = p?.display_name ?? displayNameOf(participants, entry.actorID);
  const { clean, todos } = splitTodoBlock(entry.body ?? ""); // 协议块不入正文渲染
  return (
    <div className="animate-rise flex gap-2.5">
      <Avatar participantID={entry.actorID} displayName={name} />
      <div className="min-w-0 max-w-[80%]">
        <div className="mb-0.5 flex flex-wrap items-baseline gap-x-2 text-xs">
          <span className="font-medium text-text">{name}</span>
          <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] leading-4 text-dim">
            {kindLabel(entry.actorKind)}
          </span>
          {p?.adapter && (
            <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] leading-4 text-dim">
              {adapterLabel(p.adapter)}
              {p.channel ? ` · ${channelLabel(p.channel)}` : ""}
            </span>
          )}
          <span className="text-faint">{relativeTime(entry.occurredAt)}</span>
          <BubbleActions entry={entry} onQuote={onQuote} />
        </div>
        <div className="w-fit max-w-full rounded-2xl rounded-tl-md bg-surface-2 px-3.5 py-2 text-sm">
          {entry.replyTo && (
            <ReplyQuoteBar target={quoteTarget} fallbackID={entry.replyTo} participants={participants} onJump={onJump} />
          )}
          <AddressedLine entry={entry} participants={participants} />
          <MarkdownBody text={clean} />
          {todos && <TodoChip items={todos} />}
        </div>
      </div>
    </div>
  );
}
