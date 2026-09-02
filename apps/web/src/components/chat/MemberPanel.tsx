// 右侧抽屉（成员 / 发言评估 / 话题线，开发者模式追加"调试"）：数据源为房间快照投影（随 SSE 事件由 room.ts
// 防抖重取），无手动刷新。请优先发言沿用 endorse_intent 命令链（room.endorse 内部做版本校准+409 重试）。
// 成员 Tab 顶部"邀请"沿用 invite_agent 命令链（同校准/重试）——房间级操作只出现在房间上下文。
// RFC-0012 群聊制：讨论模式/策略 Tab 已退役（房间即群聊，无模式概念）。
// 内部枚举一律经 lib/copy 映射层转为用户语言，不裸显。
import { useEffect, useState } from "react";
import { api, type AgentSeatInfo, type ParticipantView } from "../../api/client";
import type { GraphEdge, ScorecardItem, ThreadItem } from "../../api/room";
import { useDevMode } from "../../state/dev";
import { DevPanel } from "../DevPanel";
import {
  adapterLabel,
  channelLabel,
  intentActionLabel,
  intentTypeLabel,
  kindLabel,
  relationKindLabel,
  scoreBandLabel,
  seatStatusLabel,
  threadStateLabel,
} from "../../lib/copy";
import { displayNameOf, shortId, truncate } from "../../lib/ui";
import { Avatar } from "./Avatar";

type Tab = "members" | "scorecard" | "graph" | "debug";

const TABS: { id: Tab; label: string }[] = [
  { id: "members", label: "成员" },
  { id: "scorecard", label: "发言评估" },
  { id: "graph", label: "话题线" },
];

/** 开发者模式（全局开关）开启时追加"调试"Tab——调试数据是本房间的局部信息。 */
const DEBUG_TAB: { id: Tab; label: string } = { id: "debug", label: "调试" };


export function MemberPanel({
  roomID,
  participants,
  roster,
  scorecard,
  threads,
  edges,
  endorseBusy,
  onEndorse,
  inviteBusy,
  onInvite,
  onTabActive,
  onClose,
  describeEvent,
}: {
  /** 本房间 id（调试 Tab 的数据目标；房间局部信息的来源）。 */
  roomID: string | null;
  participants: ParticipantView[];
  /** 入房 Agent 名单（null = 全席模式，无邀请入口语义上的候选）。 */
  roster: string[] | null;
  scorecard: ScorecardItem[];
  threads: ThreadItem[];
  edges: GraphEdge[];
  endorseBusy: string | null;
  onEndorse: (intentID: string) => void;
  inviteBusy: string | null;
  onInvite: (participantID: string) => void;
  /** Tab 打开/切换时回调（触发投影刷新）。 */
  onTabActive: (tab: Tab) => void;
  onClose: () => void;
  /** event_id → "名字：摘要" 可读引用；解析不到返回 null（回退短 hash）。 */
  describeEvent: (eventID: string) => string | null;
}) {
  const [tab, setTab] = useState<Tab>("members");
  const devMode = useDevMode();
  const tabs = devMode ? [...TABS, DEBUG_TAB] : TABS;

  // 开发者模式关闭时若正停在调试 Tab，回退到成员 Tab。
  useEffect(() => {
    if (!devMode && tab === "debug") setTab("members");
  }, [devMode, tab]);

  useEffect(() => {
    onTabActive(tab);
  }, [tab, onTabActive]);

  return (
    <aside className="animate-fade-in flex w-80 shrink-0 flex-col border-l border-border bg-surface">
      <div className="flex items-center border-b border-border px-2 py-1.5">
        <div className="flex flex-1 gap-1">
          {tabs.map((t) => (
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
        {tab === "members" && (
          <MembersTab participants={participants} roster={roster} inviteBusy={inviteBusy} onInvite={onInvite} />
        )}
        {tab === "scorecard" && (
          <ScorecardTab
            scorecard={scorecard}
            participants={participants}
            busy={endorseBusy}
            onEndorse={onEndorse}
          />
        )}
        {tab === "graph" && <GraphTab threads={threads} edges={edges} describeEvent={describeEvent} />}
        {tab === "debug" && (
          <div className="px-3 py-3">
            <DevPanel
              roomID={roomID}
              nameOf={(pid) =>
                participants.find((p) => p.participant_id === pid)?.display_name ?? pid
              }
              describeEvent={describeEvent}
            />
          </div>
        )}
      </div>
    </aside>
  );
}

function MembersTab({
  participants,
  roster,
  inviteBusy,
  onInvite,
}: {
  participants: ParticipantView[];
  roster: string[] | null;
  inviteBusy: string | null;
  onInvite: (participantID: string) => void;
}) {
  const [inviting, setInviting] = useState(false);
  // 快照 participants 是全局座位视图（含未入房 Agent）；房间成员 = 人类 + roster
  // 名单内的 Agent（roster null = 全席模式，所有在席 Agent 均在房内）。
  const members = participants.filter(
    (p) => p.kind !== "agent" || roster === null || roster.includes(p.participant_id),
  );
  return (
    <div className="py-1">
      <div className="flex items-center justify-between px-3 pb-1 pt-1">
        <h3 className="text-xs font-medium text-dim">成员（{members.length}）</h3>
        <button
          type="button"
          onClick={() => setInviting((v) => !v)}
          aria-expanded={inviting}
          className="rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          + 邀请
        </button>
      </div>
      {inviting && <InviteList roster={roster} busy={inviteBusy} onInvite={onInvite} />}
      {members.length === 0 ? (
        <p className="px-3 py-3 text-xs text-faint">暂无参与者信息。</p>
      ) : (
        <ul className="py-1">
          {members.map((p) => (
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
                  <Badge>{kindLabel(p.kind)}</Badge>
                  {p.adapter && <Badge>{adapterLabel(p.adapter)}</Badge>}
                  {p.channel && <Badge>{channelLabel(p.channel)}</Badge>}
                </div>
              </div>
              <Badge tone={p.seat_status === "seated" ? "ok" : "dim"}>{seatStatusLabel(p.seat_status)}</Badge>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** 邀请候选：在席（GET /v1/agents）且不在 roster 内（roster 即房间成员名单）。 */
function InviteList({
  roster,
  busy,
  onInvite,
}: {
  roster: string[] | null;
  busy: string | null;
  onInvite: (participantID: string) => void;
}) {
  const [agents, setAgents] = useState<AgentSeatInfo[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .agents()
      .then(({ agents }) => setAgents(agents))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  if (roster === null) {
    return (
      <p className="mx-2 mb-2 rounded-lg border border-border bg-surface-2 px-2.5 py-2 text-[11px] text-faint">
        本房间创建于旧版本且尚无 Agent 参与记录，暂按全部在席处理；发起一轮讨论后名单即按参与历史固化，此后新启用的 Agent 走邀请。
      </p>
    );
  }
  if (error) return <p className="mx-2 mb-2 px-1 text-[11px] text-danger">{error}</p>;
  if (agents === null) return <p className="mx-2 mb-2 px-1 text-[11px] text-faint">座位加载中…</p>;

  const candidates = agents.filter((a) => !roster.includes(a.participant_id));
  if (candidates.length === 0) {
    return <p className="mx-2 mb-2 px-1 text-[11px] text-faint">没有可邀请的在席 Agent。</p>;
  }
  return (
    <ul className="mx-2 mb-2 rounded-lg border border-border bg-surface-2 py-1">
      {candidates.map((a) => (
        <li key={a.participant_id} className="flex items-center gap-2 px-2.5 py-1.5 text-xs">
          <Avatar participantID={a.participant_id} displayName={a.display_name} size={20} />
          <span className="min-w-0 flex-1 truncate">{a.display_name}</span>
          <span className="shrink-0 text-[10px] text-faint">{adapterLabel(a.adapter)}</span>
          <button
            type="button"
            disabled={busy === a.participant_id}
            onClick={() => onInvite(a.participant_id)}
            className="shrink-0 rounded-lg bg-surface-3 px-2 py-0.5 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
          >
            {busy === a.participant_id ? "邀请中…" : "邀请"}
          </button>
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
        每轮讨论中，各智能体的发言意向与遴选结果会记录在这里。评估只公开档位，不公开精确分数。
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
                评估：{scoreBandLabel(it.score_band)}
              </Badge>
              <span className="ml-auto text-dim">
                {it.selected ? "✓ 已发言" : it.endorsed ? "点名优先" : "未发言"}
              </span>
            </div>
            <div className="mt-0.5 text-dim">
              意向：{it.action === "silent" ? "本轮不发言" : it.type ? intentTypeLabel(it.type) : it.action ? intentActionLabel(it.action) : "—"}
            </div>
            {it.unselected_reason && <div className="text-faint">未获发言权：{it.unselected_reason}</div>}
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
                {busy === it.intent_id ? "处理中…" : "请 TA 优先发言"}
              </button>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function GraphTab({
  threads,
  edges,
  describeEvent,
}: {
  threads: ThreadItem[];
  edges: GraphEdge[];
  describeEvent: (eventID: string) => string | null;
}) {
  return (
    <div className="py-1">
      <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">话题线（{threads.length}）</h3>
      {threads.length === 0 ? (
        <p className="px-3 py-1 text-xs text-faint">暂无话题线。</p>
      ) : (
        <ul className="divide-y divide-border">
          {threads.map((th) => (
            <li key={th.thread_id} className="px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <span className="font-mono text-text" title={th.thread_id}>
                  {shortId(th.thread_id)}
                </span>
                <Badge tone={th.state === "active" ? "ok" : th.state === "merged" ? "warn" : "dim"}>
                  {threadStateLabel(th.state)}
                </Badge>
                <span className="ml-auto text-faint">{th.message_count ?? 0} 条</span>
              </div>
              <div className="mt-0.5 text-faint">
                {th.parent ? `分支自 ${shortId(th.parent)}` : "主话题"}
                {th.goal ? ` · ${truncate(th.goal, 40)}` : ""}
                {th.merged_into ? ` · 合并入 ${shortId(th.merged_into)}` : ""}
              </div>
            </li>
          ))}
        </ul>
      )}
      <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">观点关系（{edges.length}）</h3>
      {edges.length === 0 ? (
        <p className="px-3 py-1 text-xs text-faint">
          暂无——发言时声明观点关系（支持/质疑/…）或从消息分出话题线后此处可查。
        </p>
      ) : (
        <ul className="px-3">
          {edges.map((e, i) => (
            <li key={i} className="py-1 text-xs">
              <Badge>{relationKindLabel(e.kind)}</Badge>{" "}
              <span className="text-dim">
                <EventRef eventID={e.from} describeEvent={describeEvent} />
                {" → "}
                <EventRef eventID={e.to} describeEvent={describeEvent} />
              </span>
              {e.inferred && <span className="text-faint">（系统推断）</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** 边的端点：能解析为消息就显示"名字：摘要"，否则回退短 hash（mono）。 */
function EventRef({
  eventID,
  describeEvent,
}: {
  eventID: string;
  describeEvent: (eventID: string) => string | null;
}) {
  const text = describeEvent(eventID);
  if (text) return <>{text}</>;
  return <span className="font-mono">{shortId(eventID)}</span>;
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
