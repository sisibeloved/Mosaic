// 房间订阅状态机（RFC-0001 订阅契约的客户端侧）：
// - 初载/重同步走快照（Timeline + opaque watermark），EventSource 自水位续传；
// - 断线由浏览器 EventSource 自动重连（Last-Event-ID）——不重不漏；
// - resync_required 具名信号 → 弃流、快照重建、重新订阅（慢消费者恢复路径）；
// - 时间线按 event_id 去重兜底（双通道交叠窗口）；
// - draft.update 瞬态帧（无 id、不入日志、断线不补发）驱动座位级打字预览；
// - 投影区（participants/scorecard/graph/threads/policy）由事件触发防抖重取快照。
// - 系统事件（轮次/暂停）随快照 Timeline 持久化（v1.25）——切房间/刷新不丢；
//   开发者模式下意向/授予/撤销等基建事件内联进时间线（[dev] 前缀，瞬态）。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, type EventView, type ParticipantView, type Snapshot } from "./client";
import { useDevMode } from "../state/dev";

export type Connection = "idle" | "connecting" | "live" | "reconnecting" | "resync";

export type ScorecardItem = Snapshot["scorecard"][number];
export type ThreadItem = Snapshot["threads"][number];
export type GraphEdge = Snapshot["graph"][number];
export type PolicyView = Snapshot["policy"];

export interface TimelineEntry {
  key: string;
  kind: "message" | "system";
  actorID: string;
  actorKind: string;
  occurredAt: string;
  body?: string;
  addressedTo?: string[];
  detail?: string;
}

/** 系统事件 → 时间线文案（快照 Timeline 与 SSE 双路共用；outcome 用 ROUND_OUTCOME）。 */
const SYSTEM_EVENT_TEXT: Record<string, string> = {
  "round.opened": "新一轮讨论开始",
  "room.paused": "房间已暂停",
  "room.started": "房间已恢复",
};

/** 座位级进行中状态（计划 v1.11：静默期反馈 + draft.update 草稿预览）。 */
export interface TypingState {
  phase: "queued" | "evaluating" | "generating" | "validating" | "drafting";
  text?: string;
}

/** draft.update 线上帧（httpapi.DraftConsumer → SSE 瞬态帧：无 id、不补发）。 */
interface DraftFrame {
  room_id: string;
  participant_id: string;
  kind: "text_delta" | "stage";
  text?: string;
  stage?: "queued" | "evaluating" | "generating" | "validating";
}

const SUBSCRIBED_EVENTS = [
  "message.posted",
  "round.opened",
  "round.closed",
  "intent.recorded",
  "intent.endorsed",
  "floor.granted",
  "floor.revoked",
  "room.paused",
  "room.started",
  "room.renamed",
  "policy.changed",
  "participant.admitted",
] as const;

const ROUND_OUTCOME: Record<string, string> = {
  published: "本轮讨论结束",
  quiescent: "本轮无人发言（静默结束）",
  budget_stopped: "本轮因预算上限停止",
  revoked_all: "本轮发言被全部撤销",
};

/** 草稿文本内存上界（展示侧另截断 140 字）。 */
const DRAFT_TEXT_CAP = 800;

interface RoomModelState {
  roomID: string;
  version: number;
  displayName: string;
  entries: TimelineEntry[];
  typing: Record<string, TypingState>;
  participants: ParticipantView[];
  scorecard: ScorecardItem[];
  threads: ThreadItem[];
  edges: GraphEdge[];
  policy: PolicyView | null;
  roundOpen: boolean;
  paused: boolean;
  /** 入房 Agent 名单（null = 全席模式：建房未选人，所有在席 Agent 均在房内）。 */
  roster: string[] | null;
}

export interface RoomHandle {
  roomID: string | null;
  version: number;
  displayName: string;
  entries: TimelineEntry[];
  typing: Record<string, TypingState>;
  participants: ParticipantView[];
  scorecard: ScorecardItem[];
  threads: ThreadItem[];
  edges: GraphEdge[];
  policy: PolicyView | null;
  roundOpen: boolean;
  paused: boolean;
  roster: string[] | null;
  connection: Connection;
  error: string | null;
  send(body: string, addressedTo?: string[]): Promise<void>;
  pause(): Promise<void>;
  resume(): Promise<void>;
  rename(displayName: string): Promise<void>;
  endorse(intentID: string): Promise<void>;
  /** invite_agent 拉人入房（RFC-0001 Membership：participant.admitted）。 */
  invite(participantID: string): Promise<void>;
  /** 重取快照投影区（成员/记分卡/谱系/策略）——抽屉 Tab 打开时调用。 */
  refreshProjections(): Promise<void>;
}

function projections(snap: Snapshot) {
  return {
    version: snap.room_version,
    displayName: snap.display_name,
    participants: snap.participants,
    scorecard: snap.scorecard,
    threads: snap.threads,
    edges: snap.graph,
    policy: snap.policy,
    roster: snap.roster ?? null,
  };
}

export function useRoom(roomID: string | null): RoomHandle {
  const [state, setState] = useState<RoomModelState | null>(null);
  const [connection, setConnection] = useState<Connection>("idle");
  const [error, setError] = useState<string | null>(null);
  const [devMode] = useDevMode();
  const esRef = useRef<EventSource | null>(null);
  const versionRef = useRef(0);
  const roomRef = useRef<string | null>(null);
  /** 自愈重订入口：onerror CLOSED 时经此回到 loadAndSubscribe（ref 于 effect 中同步）。 */
  const reloadRef = useRef<(id: string) => Promise<void>>(async () => {});
  /** floor.granted 的 grant_id → 座位（floor.revoked 载荷无 participant_id，靠它回收打字态）。 */
  const grantSeatRef = useRef<Record<string, string>>({});
  const refreshTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const refreshBusyRef = useRef(false);

  const closeStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
  }, []);

  const systemEntry = (view: EventView, detail: string): TimelineEntry => ({
    key: view.event_id,
    kind: "system",
    actorID: "",
    actorKind: "system",
    occurredAt: view.occurred_at,
    detail,
  });

  /** 投影区防抖重取（尾随 300ms + 单飞）：事件驱动，不轮询。 */
  const refreshProjectionsNow = useCallback(async () => {
    const id = roomRef.current;
    if (!id || refreshBusyRef.current) return;
    refreshBusyRef.current = true;
    try {
      const snap = await api.snapshot(id);
      if (roomRef.current !== id) return; // 期间已切换房间
      versionRef.current = snap.room_version;
      setState((prev) => (prev ? { ...prev, ...projections(snap) } : prev));
    } catch {
      // 投影刷新失败不打扰主流程——下个事件会再触发
    } finally {
      refreshBusyRef.current = false;
    }
  }, []);

  const scheduleRefresh = useCallback(() => {
    if (refreshTimerRef.current) clearTimeout(refreshTimerRef.current);
    refreshTimerRef.current = setTimeout(() => {
      refreshTimerRef.current = null;
      void refreshProjectionsNow();
    }, 300);
  }, [refreshProjectionsNow]);

  const applyEvent = useCallback(
    (type: string, view: EventView) => {
      setState((prev) => {
        if (!prev) return prev;
        const next: RoomModelState = {
          ...prev,
          entries: prev.entries,
          typing: { ...prev.typing },
        };
        const append = (entry: TimelineEntry) => {
          if (prev.entries.some((e) => e.key === entry.key)) return; // 去重兜底
          next.entries = [...prev.entries, entry];
        };
        switch (type) {
          case "message.posted": {
            const payload = view.payload as { body?: string; addressed_to?: string[] | null } | null;
            append({
              key: view.event_id,
              kind: "message",
              actorID: view.actor.participant_id,
              actorKind: view.actor.kind,
              occurredAt: view.occurred_at,
              body: payload?.body,
              addressedTo: payload?.addressed_to ?? undefined,
            });
            if (view.actor.kind === "agent") {
              delete next.typing[view.actor.participant_id];
            }
            break;
          }
          case "round.opened": {
            // 自动续聊轮（v1.27）：payload.auto_index>0 → 接力轮，标签区分
            const auto = (view.payload as { auto_index?: number } | null)?.auto_index ?? 0;
            append(systemEntry(view, auto > 0 ? `自动续聊 · 第 ${auto} 轮` : "新一轮讨论开始"));
            next.roundOpen = true;
            scheduleRefresh();
            break;
          }
          case "round.closed": {
            const outcome = (view.payload as { outcome?: string } | null)?.outcome ?? "";
            append(systemEntry(view, ROUND_OUTCOME[outcome] ?? `本轮结束（${outcome}）`));
            next.roundOpen = false;
            next.typing = {};
            grantSeatRef.current = {};
            scheduleRefresh();
            break;
          }
          case "room.paused":
            append(systemEntry(view, "房间已暂停"));
            next.paused = true;
            next.typing = {};
            break;
          case "room.started":
            append(systemEntry(view, "房间已恢复"));
            next.paused = false;
            break;
          case "room.renamed": {
            const name = (view.payload as { display_name?: string } | null)?.display_name;
            if (name) {
              next.displayName = name;
              append(systemEntry(view, `房间更名为「${name}」`));
            }
            break;
          }
          case "floor.granted": {
            const payload = view.payload as { participant_id?: string; grant_id?: string } | null;
            if (payload?.participant_id) {
              next.typing[payload.participant_id] = { phase: "generating" };
              if (payload.grant_id) grantSeatRef.current[payload.grant_id] = payload.participant_id;
            }
            break;
          }
          case "floor.revoked": {
            const grantID = (view.payload as { grant_id?: string } | null)?.grant_id;
            const pid = grantID ? grantSeatRef.current[grantID] : undefined;
            if (grantID && pid) {
              delete next.typing[pid];
              delete grantSeatRef.current[grantID];
            }
            break;
          }
          case "intent.recorded": {
            const pid = (view.payload as { participant_id?: string } | null)?.participant_id;
            if (pid && !next.typing[pid]) next.typing[pid] = { phase: "evaluating" };
            scheduleRefresh();
            break;
          }
          case "intent.endorsed":
          case "policy.changed":
          case "participant.admitted": // invite_agent 拉人 → 成员/roster 投影刷新
            scheduleRefresh();
            break;
        }
        // 开发者模式（v1.25 dogfood #3）：基建事件内联时间线——房间怎么运转、
        // 卡在哪一步，在使用过程中直接可见（瞬态，不入快照）。
        if (devMode) {
          const nm = (pid?: string) =>
            pid ? (prev.participants.find((x) => x.participant_id === pid)?.display_name ?? pid) : "?";
          let detail: string | null = null;
          switch (type) {
            case "intent.recorded": {
              const p = view.payload as
                | { participant_id?: string; action?: string; type?: string; public_rationale?: string }
                | null;
              detail = `[dev] 意向 ${nm(p?.participant_id)}：${p?.action ?? "?"}${
                p?.type ? ` · ${p.type}` : ""
              }${p?.public_rationale ? `（${p.public_rationale}）` : ""}`;
              break;
            }
            case "floor.granted": {
              const p = view.payload as { participant_id?: string; grant_id?: string } | null;
              detail = `[dev] 发言权 → ${nm(p?.participant_id)}${p?.grant_id ? `（${p.grant_id}）` : ""}`;
              break;
            }
            case "floor.revoked": {
              const p = view.payload as { reason?: string } | null;
              detail = `[dev] 发言权撤销${p?.reason ? `：${p.reason}` : ""}`;
              break;
            }
            case "participant.admitted": {
              const p = view.payload as { participant_id?: string } | null;
              detail = `[dev] ${nm(p?.participant_id)} 入房（invite_agent）`;
              break;
            }
            case "policy.changed": {
              const p = view.payload as { policy_version?: string; mode?: string } | null;
              detail = `[dev] 策略变更 → ${p?.mode ?? "?"}（${p?.policy_version ?? "?"}）`;
              break;
            }
          }
          if (detail) append(systemEntry(view, detail));
        }
        return next;
      });
    },
    [scheduleRefresh, devMode],
  );

  /** draft.update 瞬态帧：text_delta 累积文本（截尾上限）；stage 原值更新阶段。 */
  const applyDraft = useCallback((frame: DraftFrame) => {
    setState((prev) => {
      if (!prev || frame.room_id !== prev.roomID) return prev;
      const typing = { ...prev.typing };
      const cur = typing[frame.participant_id] ?? { phase: "evaluating" as const };
      if (frame.kind === "text_delta") {
        const text = ((cur.text ?? "") + (frame.text ?? "")).slice(-DRAFT_TEXT_CAP);
        typing[frame.participant_id] = {
          phase: cur.phase === "queued" || cur.phase === "evaluating" ? "drafting" : cur.phase,
          text,
        };
      } else if (frame.kind === "stage" && frame.stage) {
        typing[frame.participant_id] = { ...cur, phase: frame.stage };
      }
      return { ...prev, typing };
    });
  }, []);

  /** 快照重建 + 自水位订阅（初载与 resync_required 共用路径）。 */
  const loadAndSubscribe = useCallback(
    async (id: string) => {
      closeStream();
      setConnection("connecting");
      setError(null);
      let snap: Snapshot;
      try {
        snap = await api.snapshot(id);
      } catch (e) {
        setConnection("idle");
        setState(null);
        setError(`快照获取失败：${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      if (roomRef.current !== id) return; // 等待期间已切换房间
      versionRef.current = snap.room_version;
      grantSeatRef.current = {};
      setState({
        roomID: id,
        entries: snap.timeline.map((item) =>
          item.type === "message.posted"
            ? {
                key: item.event_id,
                kind: "message" as const,
                actorID: item.actor_id,
                actorKind: item.actor_kind,
                occurredAt: item.occurred_at,
                body: item.body,
              }
            : {
                // 系统事件持久化项（v1.25）：round/pause 提醒不再随 SSE 瞬态丢失
                key: item.event_id,
                kind: "system" as const,
                actorID: item.actor_id,
                actorKind: item.actor_kind,
                occurredAt: item.occurred_at,
                detail:
                  item.type === "round.closed"
                    ? (ROUND_OUTCOME[item.outcome ?? ""] ?? `本轮结束（${item.outcome ?? "?"}）`)
                    : item.type === "round.opened" && (item.auto_index ?? 0) > 0
                      ? `自动续聊 · 第 ${item.auto_index} 轮`
                      : (SYSTEM_EVENT_TEXT[item.type] ?? item.type),
              },
        ),
        typing: {},
        roundOpen: false,
        paused: false,
        ...projections(snap),
      });
      const es = new EventSource(
        `/v1/rooms/${encodeURIComponent(id)}/events?cursor=${encodeURIComponent(snap.watermark)}`,
      );
      esRef.current = es;
      es.onopen = () => setConnection("live");
      es.onerror = () => {
        if (es.readyState === EventSource.CLOSED) {
          // 永久断流（非 200 / 错误 Content-Type 等，如嵌入 WebView 的资产桥）：
          // 浏览器不再自动重连，也等不到 resync_required——快照重建 + 退避重订，
          // 避免 UI 永卡"恢复中"。
          setConnection("resync");
          setTimeout(() => {
            if (esRef.current === es) void reloadRef.current(id);
          }, 3000);
        } else {
          setConnection("reconnecting"); // 浏览器将带 Last-Event-ID 自动重连
        }
      };
      for (const type of SUBSCRIBED_EVENTS) {
        es.addEventListener(type, (ev) => {
          applyEvent(type, JSON.parse((ev as MessageEvent).data) as EventView);
        });
      }
      es.addEventListener("draft.update", (ev) => {
        applyDraft(JSON.parse((ev as MessageEvent).data) as DraftFrame);
      });
      es.addEventListener("resync_required", () => {
        setConnection("resync");
        void loadAndSubscribe(id);
      });
    },
    [applyEvent, applyDraft, closeStream],
  );

  useEffect(() => {
    reloadRef.current = loadAndSubscribe;
  }, [loadAndSubscribe]);

  // 房间切换：复位并重新装载；卸载/切换时断流。
  useEffect(() => {
    roomRef.current = roomID;
    grantSeatRef.current = {};
    if (!roomID) {
      closeStream();
      setState(null);
      setConnection("idle");
      return;
    }
    void loadAndSubscribe(roomID);
    return closeStream;
  }, [roomID, loadAndSubscribe, closeStream]);

  /** 发命令前以快照校准版本（引擎轮推进版本，SSE 帧不含版本）；409 兜底重试一次。 */
  const withVersion = useCallback(
    async (run: (version: number) => Promise<unknown>): Promise<void> => {
      const id = roomRef.current;
      if (!id) throw new Error("房间未连接");
      try {
        const snap = await api.snapshot(id);
        versionRef.current = snap.room_version;
      } catch {
        // 校准失败沿用本地版本，由 409 兜底
      }
      try {
        await run(versionRef.current);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          const snap = await api.snapshot(id);
          versionRef.current = snap.room_version;
          await run(versionRef.current);
          return;
        }
        throw e;
      }
    },
    [],
  );

  const runCommand = useCallback(
    async (run: (id: string, version: number) => Promise<unknown>) => {
      const id = roomRef.current;
      if (!id) return;
      try {
        await withVersion((v) => run(id, v));
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        throw e;
      }
    },
    [withVersion],
  );

  const send = useCallback(
    (body: string, addressedTo: string[] = []) =>
      runCommand((id, v) => api.postMessage(id, v, body, addressedTo)),
    [runCommand],
  );
  const pause = useCallback(
    () => runCommand((id, v) => api.pauseRoom(id, v, "web")),
    [runCommand],
  );
  const resume = useCallback(() => runCommand((id, v) => api.resumeRoom(id, v)), [runCommand]);
  const rename = useCallback(
    async (displayName: string) => {
      await runCommand((id, v) => api.renameRoom(id, v, displayName));
      setState((prev) => (prev ? { ...prev, displayName } : prev)); // SSE room.renamed 到达前的即时反馈
    },
    [runCommand],
  );
  const endorse = useCallback(
    (intentID: string) => runCommand((id, v) => api.endorseIntent(id, v, intentID)),
    [runCommand],
  );
  const invite = useCallback(
    (participantID: string) => runCommand((id, v) => api.inviteAgent(id, v, participantID)),
    [runCommand],
  );

  return {
    roomID: state?.roomID ?? null,
    version: state?.version ?? 0,
    displayName: state?.displayName ?? "",
    entries: state?.entries ?? [],
    typing: state?.typing ?? {},
    participants: state?.participants ?? [],
    scorecard: state?.scorecard ?? [],
    threads: state?.threads ?? [],
    edges: state?.edges ?? [],
    policy: state?.policy ?? null,
    roundOpen: state?.roundOpen ?? false,
    paused: state?.paused ?? false,
    roster: state?.roster ?? null,
    connection,
    error,
    send,
    pause,
    resume,
    rename,
    endorse,
    invite,
    refreshProjections: refreshProjectionsNow,
  };
}
