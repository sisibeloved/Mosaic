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

/** M3-2 收束摘要（快照 closures 视图）。 */
export type ClosureSummary = NonNullable<Snapshot["closures"]>[number];

/** M3-3 任务清单项（快照 tasks 视图；owner = 责任人）。 */
export type TaskItem = NonNullable<Snapshot["tasks"]>[number];

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
  "participant.admitted",
  "closure.proposed",
  "closure.evaluated",
  "closure.rejected",
  "closure.accepted",
  "pause_capsule.created",
  "evidence_request.claimed",
  "task.resolved",
  "memory.edited",
] as const;


/** 波跳过原因 → 开发者模式文案（engine.waveSkip 的 reason 集合）。 */
const WAVE_SKIP_TEXT: Record<string, string> = {
  paused: "波未开启：房间已暂停",
  thread_inactive: "波未开启：锚点线程已暂停/关闭/合并",
  no_seats: "波未开启：房内无 agent 席位",
};

/** wave.skipped 线上帧（httpapi.WaveSkipConsumer → SSE 瞬态帧：无 id、不补发）。 */
interface WaveSkipFrame {
  room_id: string;
  reason?: string;
}

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
  closures: ClosureSummary[];
  /** M3-3 任务清单（带责任人的承诺追踪）。 */
  tasks: TaskItem[];
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
  closures: ClosureSummary[];
  tasks: TaskItem[];
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
  /** M3-2 收束：提议（threadID 空 = 根线程）；接受待决收束（closureID 空 = 唯一待决）。 */
  proposeClosure(threadID: string | null, hint?: string): Promise<void>;
  acceptClosure(closureID?: string): Promise<void>;
  /** M3-3 任务裁定（task.resolved：delivered=已交付；dismissed=误报/撤销）。 */
  resolveTask(taskID: string, resolution: "delivered" | "dismissed", note?: string): Promise<void>;
  /** M3-3 记忆编辑（memory.edited：整组替换，生效于下次组装）。 */
  editMemory(memoryID: string, edits: { conclusions?: string[]; assumptions?: string[] }, note: string): Promise<void>;
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
    closures: snap.closures ?? [],
    tasks: snap.tasks ?? [],
    roster: snap.roster ?? null,
  };
}


/** 开发者模式内联格式化（直播与重启回放同一出口——[dev] 条目不随刷新失真）。 */
function devDetailOf(
  type: string,
  payload: unknown,
  resolveName: (pid?: string) => string,
): string | null {
  const p = ((payload ?? {}) as Record<string, any>) ?? {};
  switch (type) {
    case "intent.recorded":
      return `[dev] 意向 ${resolveName(p.participant_id)}：${p.action ?? "?"}${
        p.type ? ` · ${p.type}` : ""
      }${p.public_rationale ? `（${p.public_rationale}）` : ""}`;
    case "floor.granted":
      return `[dev] 发言权 → ${resolveName(p.participant_id)}${p.grant_id ? `（${p.grant_id}）` : ""}`;
    case "floor.revoked":
      return `[dev] 发言权撤销${p.reason ? `：${p.reason}` : ""}`;
    case "participant.admitted":
      return `[dev] ${resolveName(p.participant_id)} 入房（invite_agent）`;
    case "closure.proposed":
      return `[dev] 收束提议（${p.trigger ?? "human"}）`;
    case "closure.evaluated":
      return `[dev] 收束表态 ${resolveName(p.participant_id)}：${p.action ?? "?"}${
        p.qualified ? "（合格异议）" : ""
      }${p.rationale ? `（${p.rationale}）` : ""}`;
    case "closure.rejected":
      return `[dev] 收束被合格异议中止（${p.reason ?? "?"}）`;
    case "closure.accepted":
      return `[dev] 收束已接受（${p.closure_type ?? "?"}）`;
    case "pause_capsule.created":
      return `[dev] 预算暂停胶囊（${p.pause_reason ?? "?"}，未收敛非结论）`;
    case "evidence_request.created":
      return `[dev] 证据需求单：${p.question ?? ""}`;
    case "evidence_request.claimed":
      return `[dev] 证据需求单认领：${resolveName(p.claimed_by)}${p.note ? `（${p.note}）` : ""}`;
    case "evidence_request.resolved":
      return `[dev] 证据需求单${p.resolution === "dismissed" ? "驳回" : "解决"}`;
    case "task.resolved":
      return `[dev] 任务裁定（${resolveName(p.owner)}）：${p.resolution === "dismissed" ? "误报/撤销" : "已交付"}${
        p.note ? `（${p.note}）` : ""
      }`;
    case "memory.edited":
      return `[dev] 记忆编辑 ${p.memory_id ?? ""} v${p.edit_version ?? "?"}${
        p.note ? `（${p.note}）` : ""
      }`;
    default:
      return null;
  }
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
  /** wave.skipped 瞬态条目的本地序号（无 event_id，React key 需自造唯一值）。 */
  const waveSkipSeqRef = useRef(0);
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
          case "round.opened":
            // RFC-0012：round 内部化——不进时间线，仅维护进行中状态
            next.roundOpen = true;
            scheduleRefresh();
            break;
          case "round.closed":
            next.roundOpen = false;
            next.typing = {};
            grantSeatRef.current = {};
            scheduleRefresh();
            break;
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
            const p = view.payload as { participant_id?: string; action?: string } | null;
            const pid = p?.participant_id;
            // 意愿座保留/补置"正在评估"直至 floor.granted 转生成；非意愿座
            // （silent/弃权/fork/summarize）自决不回——立即撤下，不留虚影。
            if (pid && (p?.action === "speak" || p?.action === "react")) {
              if (!next.typing[pid]) next.typing[pid] = { phase: "evaluating" };
            } else if (pid) {
              delete next.typing[pid];
            }
            scheduleRefresh();
            break;
          }
          case "closure.proposed":
          scheduleRefresh();
          break;
        case "closure.evaluated":
          scheduleRefresh();
          break;
        case "closure.rejected":
          scheduleRefresh();
          break;
        case "closure.accepted":
          scheduleRefresh();
          break;
        case "pause_capsule.created":
          scheduleRefresh();
          break;
        case "task.resolved": // M3-3：任务裁定 → 快照 tasks 重投影
          scheduleRefresh();
          break;
        case "memory.edited": // M3-3：记忆编辑 → 胶囊视图重取
          scheduleRefresh();
          break;
        case "intent.endorsed":
        }
        // 开发者模式（v1.25 dogfood #3）：基建事件内联时间线——房间怎么运转、
        // 卡在哪一步，在使用过程中直接可见（瞬态，不入快照）。
        if (devMode) {
          const nm = (pid?: string) =>
            pid ? (prev.participants.find((x) => x.participant_id === pid)?.display_name ?? pid) : "?";
          let detail: string | null = devDetailOf(type, view.payload, nm);
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

  /** wave.skipped 瞬态帧：开发者模式下内联静默原因（门控跳过不落事件——
      没有这条路房间里只剩死寂；瞬态不入快照，刷新后不重现）。 */
  const applyWaveSkip = useCallback(
    (frame: WaveSkipFrame) => {
      if (!devMode) return;
      setState((prev) => {
        if (!prev || frame.room_id !== prev.roomID) return prev;
        waveSkipSeqRef.current += 1;
        const entry: TimelineEntry = {
          key: `wave-skip-${waveSkipSeqRef.current}`,
          kind: "system",
          actorID: "",
          actorKind: "system",
          occurredAt: new Date().toISOString(),
          detail: WAVE_SKIP_TEXT[frame.reason ?? ""] ?? `波未开启（原因 ${frame.reason ?? "未知"}）`,
        };
        return { ...prev, entries: [...prev.entries, entry] };
      });
    },
    [devMode],
  );

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
      // 开发者模式回放（持久化补全 v1.37）：事件支撑的 [dev] 条目随快照还原——
      // 重启/刷新不再失真；wave.skipped 等无事件瞬态仍不入史。
      const replayDev = devMode
        ? (snap.dev_notes ?? []).map((n) => ({
            key: n.event_id,
            kind: "system" as const,
            actorID: "",
            actorKind: "system" as const,
            occurredAt: n.occurred_at,
            detail:
              devDetailOf(n.type, n.payload, (pid) =>
                pid
                  ? (snap.participants.find((x2) => x2.participant_id === pid)?.display_name ?? pid)
                  : "?",
              ) ?? `[dev] ${n.type}`,
          }))
        : [];
      const baseEntries: TimelineEntry[] = snap.timeline.map((item) =>
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
                detail: SYSTEM_EVENT_TEXT[item.type] ?? item.type,
              },
        );
      baseEntries.push(...replayDev);
      baseEntries.sort((a, b) => (a.occurredAt < b.occurredAt ? -1 : 1));
      setState({
        roomID: id,
        entries: baseEntries,
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
      es.addEventListener("wave.skipped", (ev) => {
        applyWaveSkip(JSON.parse((ev as MessageEvent).data) as WaveSkipFrame);
      });
      es.addEventListener("resync_required", () => {
        setConnection("resync");
        void loadAndSubscribe(id);
      });
    },
    [applyEvent, applyDraft, applyWaveSkip, closeStream],
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
  const proposeClosure = useCallback(
    (threadID: string | null, hint?: string) =>
      runCommand((id, v) => api.proposeClosure(id, v, threadID, hint)),
    [runCommand],
  );
  const acceptClosure = useCallback(
    (closureID?: string) => runCommand((id, v) => api.acceptClosure(id, v, closureID ?? null)),
    [runCommand],
  );
  const resolveTask = useCallback(
    (taskID: string, resolution: "delivered" | "dismissed", note?: string) =>
      runCommand((id, v) => api.resolveTask(id, v, taskID, resolution, note)),
    [runCommand],
  );
  const editMemory = useCallback(
    (memoryID: string, edits: { conclusions?: string[]; assumptions?: string[] }, note: string) =>
      runCommand((id, v) => api.editMemory(id, v, memoryID, edits, note)),
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
    closures: state?.closures ?? [],
    tasks: state?.tasks ?? [],
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
    proposeClosure,
    acceptClosure,
    resolveTask,
    editMemory,
    refreshProjections: refreshProjectionsNow,
  };
}
