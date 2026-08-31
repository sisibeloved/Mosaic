// 右侧抽屉（成员 / 记分卡 / 谱系）：数据源为房间快照投影（随 SSE 事件由 room.ts
// 防抖重取），无手动刷新。保送沿用 endorse_intent 命令链（room.endorse 内部做版本校准+409 重试）。
import { useEffect, useState } from "react";
import type { GraphEdge, ScorecardItem, ThreadItem } from "../../api/room";
import type { ParticipantView } from "../../api/client";
import { displayNameOf, shortId, truncate } from "../../lib/ui";
import { Avatar } from "./Avatar";

type Tab = "members" | "scorecard" | "graph";

const TABS: { id: Tab; label: string }[] = [
  { id: "members", label: "成员" },
  { id: "scorecard", label: "记分卡" },
  { id: "graph", label: "谱系" },
];

export function MemberPanel({
  participants,
  scorecard,
  threads,
  edges,
  endorseBusy,
  onEndorse,
  onTabActive,
  onClose,
}: {
  participants: ParticipantView[];
  scorecard: ScorecardItem[];
  threads: ThreadItem[];
  edges: GraphEdge[];
  endorseBusy: string | null;
  onEndorse: (intentID: string) => void;
  /** Tab 打开/切换时回调（触发投影刷新）。 */
  onTabActive: (tab: Tab) => void;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("members");

  useEffect(() => {
    onTabActive(tab);
  }, [tab, onTabActive]);

  return (
    <aside className="animate-fade-in flex w-80 shrink-0 flex-col border-l border-border bg-surface">
      <div className="flex items-center border-b border-border px-2 py-1.5">
        <div className="flex flex-1 gap-1">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setTab(t.id)}
              className={`rounded-lg px-2.5 py-1 text-xs transition-colors ${
                tab === t.id ? "bg-surface-3 text-text" : "text-dim hover:text-text"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="收起面板"
          className="rounded-lg px-2 py-1 text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          ✕
        </button>
      </div>
      <div className="flex-1 overflow-y-auto">
        {tab === "members" && <MembersTab participants={participants} />}
        {tab === "scorecard" && (
          <ScorecardTab
            scorecard={scorecard}
            participants={participants}
            busy={endorseBusy}
            onEndorse={onEndorse}
          />
        )}
        {tab === "graph" && <GraphTab threads={threads} edges={edges} />}
      </div>
    </aside>
  );
}

function MembersTab({ participants }: { participants: ParticipantView[] }) {
  if (participants.length === 0) {
    return <p className="px-3 py-4 text-xs text-faint">暂无参与者信息。</p>;
  }
  return (
    <ul className="py-1">
      {participants.map((p) => (
        <li key={p.participant_id} className="flex items-center gap-2.5 px-3 py-2">
          <Avatar participantID={p.participant_id} displayName={p.display_name} size={28} />
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-1.5 text-sm">
              <span className="truncate">{p.display_name}</span>
              {p.kind === "human" && (
                <span className="rounded bg-accent-soft px-1.5 text-[10px] leading-4 text-accent">我</span>
              )}
            </div>
            <div className="mt-0.5 flex flex-wrap gap-1">
              <Badge>{p.kind}</Badge>
              {p.adapter && <Badge>{p.adapter}</Badge>}
              {p.channel && <Badge>{p.channel}</Badge>}
            </div>
          </div>
          <Badge tone={p.seat_status === "seated" ? "ok" : "dim"}>{p.seat_status}</Badge>
        </li>
      ))}
    </ul>
  );
}

function ScorecardTab({
  scorecard,
  participants,
  busy,
  onEndorse,
}: {
  scorecard: ScorecardItem[];
  participants: ParticipantView[];
  busy: string | null;
  onEndorse: (intentID: string) => void;
}) {
  const items = [...scorecard].reverse(); // 最新在前
  if (items.length === 0) {
    return (
      <p className="px-3 py-4 text-xs text-faint">
        尚无意向记录——发起一轮讨论后此处可查（band 公开、精确分不公开，反 Goodhart）。
      </p>
    );
  }
  return (
    <ul className="divide-y divide-border">
      {items.map((it) => {
        const canEndorse = !it.selected && !it.endorsed && it.action !== "silent";
        return (
          <li key={it.intent_id} className="px-3 py-2 text-xs">
            <div className="flex items-center gap-2">
              <span className="font-medium text-text">
                {displayNameOf(participants, it.participant_id)}
              </span>
              <Badge tone={it.selected ? "ok" : it.score_band === "unranked" ? "warn" : "dim"}>
                {it.score_band}
              </Badge>
              <span className="ml-auto text-dim">
                {it.selected ? "✓ 获选" : it.endorsed ? "已保送" : "未选"}
              </span>
            </div>
            <div className="mt-0.5 text-dim">
              意向：{it.action === "silent" ? "弃权" : it.type || it.action || "—"}
            </div>
            {it.unselected_reason && <div className="text-faint">未选理由：{it.unselected_reason}</div>}
            {it.public_rationale && (
              <div className="truncate text-faint" title={it.public_rationale}>
                {truncate(it.public_rationale, 60)}
              </div>
            )}
            {canEndorse && (
              <button
                type="button"
                disabled={busy === it.intent_id}
                onClick={() => onEndorse(it.intent_id)}
                className="mt-1 rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
              >
                {busy === it.intent_id ? "保送中…" : "保送（授予发言权）"}
              </button>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function GraphTab({ threads, edges }: { threads: ThreadItem[]; edges: GraphEdge[] }) {
  return (
    <div className="py-1">
      <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">线程（{threads.length}）</h3>
      {threads.length === 0 ? (
        <p className="px-3 py-1 text-xs text-faint">暂无线程投影。</p>
      ) : (
        <ul className="divide-y divide-border">
          {threads.map((th) => (
            <li key={th.thread_id} className="px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <span className="font-mono text-text" title={th.thread_id}>
                  {shortId(th.thread_id)}
                </span>
                <Badge tone={th.state === "active" ? "ok" : th.state === "merged" ? "warn" : "dim"}>
                  {th.state}
                </Badge>
                <span className="ml-auto text-faint">{th.message_count ?? 0} 条</span>
              </div>
              <div className="mt-0.5 text-faint">
                {th.parent ? `派生自 ${shortId(th.parent)}` : "根线程"}
                {th.goal ? ` · ${truncate(th.goal, 40)}` : ""}
                {th.merged_into ? ` · 合并入 ${shortId(th.merged_into)}` : ""}
              </div>
            </li>
          ))}
        </ul>
      )}
      <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">关系边（{edges.length}）</h3>
      {edges.length === 0 ? (
        <p className="px-3 py-1 text-xs text-faint">
          暂无——发言时声明 relations（支持/质疑/…）或从消息分叉线程后此处可查。
        </p>
      ) : (
        <ul className="px-3">
          {edges.map((e, i) => (
            <li key={i} className="py-1 text-xs">
              <Badge>{e.kind}</Badge>{" "}
              <span className="font-mono text-dim">
                {shortId(e.from)} → {shortId(e.to)}
              </span>
              {e.inferred && <span className="text-faint">（推断）</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Badge({
  children,
  tone = "dim",
}: {
  children: React.ReactNode;
  tone?: "ok" | "warn" | "dim";
}) {
  const cls =
    tone === "ok"
      ? "bg-accent-soft text-accent"
      : tone === "warn"
        ? "bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] text-warn"
        : "bg-surface-3 text-dim";
  return <span className={`rounded px-1.5 py-px text-[10px] leading-4 ${cls}`}>{children}</span>;
}
