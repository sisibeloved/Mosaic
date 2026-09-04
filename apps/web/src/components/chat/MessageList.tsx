// 消息流（参考 Grok Bot / KimiClaw 群聊）：人类右侧气泡带"我"头像（accent 色、省名字行）；
// agent 左侧带头像/显示名/类别徽标；system 事件居中细灰条。用户上翻时暂停自动滚底。
import { useEffect, useRef } from "react";
import type { TimelineEntry } from "../../api/room";
import type { ParticipantView } from "../../api/client";
import { adapterLabel, channelLabel, kindLabel } from "../../lib/copy";
import { absoluteTime, displayNameOf, participantOf, relativeTime } from "../../lib/ui";
import { Avatar } from "./Avatar";
import { MarkdownBody } from "./MarkdownBody";

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

export function MessageList({
  entries,
  participants,
}: {
  entries: TimelineEntry[];
  participants: ParticipantView[];
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
            <div key={e.key} data-event-id={e.key} className="rounded-xl">
              <HumanBubble entry={e} participants={participants} />
            </div>
          ) : (
            <div key={e.key} data-event-id={e.key} className="rounded-xl">
              <AgentBubble entry={e} participants={participants} />
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
}: {
  entry: TimelineEntry;
  participants: ParticipantView[];
}) {
  // 头像在最右（外侧），气泡在头像左侧（内侧）；名字行省略——自己知道自己。
  return (
    <div className="animate-rise flex justify-end gap-2.5">
      <div className="flex max-w-[80%] flex-col items-end">
        <div className="rounded-2xl rounded-br-md bg-accent-soft px-3.5 py-2 text-sm text-text">
          <MarkdownBody text={entry.body ?? ""} />
        </div>
        <div className="mt-0.5 text-[11px] text-faint">
          <AddressedLine entry={entry} participants={participants} />
          {relativeTime(entry.occurredAt)}
        </div>
      </div>
      <Avatar participantID={entry.actorID} displayName="我" color="var(--accent)" />
    </div>
  );
}

function AgentBubble({
  entry,
  participants,
}: {
  entry: TimelineEntry;
  participants: ParticipantView[];
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
        </div>
        <div className="w-fit max-w-full rounded-2xl rounded-tl-md bg-surface-2 px-3.5 py-2 text-sm">
          <AddressedLine entry={entry} participants={participants} />
          <MarkdownBody text={clean} />
          {todos && <TodoChip items={todos} />}
        </div>
      </div>
    </div>
  );
}
