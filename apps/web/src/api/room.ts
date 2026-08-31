// 房间订阅状态机（RFC-0001 订阅契约的客户端侧）：
// - 初载/重同步走快照（Timeline + opaque watermark），EventSource 自水位续传；
// - 断线由浏览器 EventSource 自动重连（Last-Event-ID）——不重不漏；
// - resync_required 具名信号 → 弃流、快照重建、重新订阅（慢消费者恢复路径）；
// - 时间线按 event_id 去重兜底（双通道交叠窗口）。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, type EventView, type Snapshot } from "./client";

export type Connection = "idle" | "connecting" | "live" | "reconnecting" | "resync";

export interface TimelineEntry {
  key: string;
  kind: "message" | "round-open" | "round-close" | "pause" | "resume";
  actorID: string;
  actorKind: string;
  occurredAt: string;
  body?: string;
  detail?: string;
}

/** 座位级进行中状态（计划 v1.11：静默期反馈）。 */
export interface TypingState {
  phase: "evaluating" | "generating" | "drafting";
  text?: string;
}

const SUBSCRIBED_EVENTS = [
  "message.posted",
  "round.opened",
  "round.closed",
  "intent.recorded",
  "floor.granted",
  "room.paused",
  "room.started",
] as const;

interface RoomModelState {
  roomID: string;
  version: number;
  entries: TimelineEntry[];
  typing: Record<string, TypingState>;
  roundOpen: boolean;
  paused: boolean;
}

export interface RoomHandle {
  roomID: string | null;
  version: number;
  entries: TimelineEntry[];
  typing: Record<string, TypingState>;
  roundOpen: boolean;
  paused: boolean;
  connection: Connection;
  error: string | null;
  createRoom(): Promise<void>;
  send(body: string, addressedTo?: string[]): Promise<void>;
  pause(): Promise<void>;
  resume(): Promise<void>;
}

export function useRoom(): RoomHandle {
  const [state, setState] = useState<RoomModelState | null>(null);
  const [connection, setConnection] = useState<Connection>("idle");
  const [error, setError] = useState<string | null>(null);
  const esRef = useRef<EventSource | null>(null);
  const versionRef = useRef(0);

  const closeStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
  }, []);

  const applyEvent = useCallback((type: string, view: EventView) => {
    setState((prev) => {
      if (!prev) return prev;
      const next: RoomModelState = {
        ...prev,
        entries: prev.entries,
        typing: { ...prev.typing },
      };
      switch (type) {
        case "message.posted": {
          if (prev.entries.some((e) => e.key === view.event_id)) break; // 去重兜底
          const body = (view.payload as { body?: string } | null)?.body;
          next.entries = [
            ...prev.entries,
            {
              key: view.event_id,
              kind: "message",
              actorID: view.actor.participant_id,
              actorKind: view.actor.kind,
              occurredAt: view.occurred_at,
              body,
            },
          ];
          if (view.actor.kind === "agent") {
            delete next.typing[view.actor.participant_id];
          }
          break;
        }
        case "round.opened":
          next.roundOpen = true;
          break;
        case "round.closed": {
          if (prev.entries.some((e) => e.key === view.event_id)) break;
          const outcome = (view.payload as { outcome?: string } | null)?.outcome ?? "";
          next.entries = [
            ...prev.entries,
            {
              key: view.event_id,
              kind: "round-close",
              actorID: "",
              actorKind: "system",
              occurredAt: view.occurred_at,
              detail: outcome,
            },
          ];
          next.roundOpen = false;
          next.typing = {};
          break;
        }
        case "floor.granted": {
          const pid = (view.payload as { participant_id?: string } | null)?.participant_id;
          if (pid) next.typing[pid] = { phase: "generating" };
          break;
        }
        case "intent.recorded": {
          const pid = (view.payload as { participant_id?: string } | null)?.participant_id;
          if (pid && !next.typing[pid]) next.typing[pid] = { phase: "evaluating" };
          break;
        }
        case "room.paused":
          next.paused = true;
          break;
        case "room.started":
          next.paused = false;
          break;
      }
      return next;
    });
  }, []);

  /** 快照重建 + 自水位订阅（初载与 resync_required 共用路径）。 */
  const loadAndSubscribe = useCallback(
    async (roomID: string) => {
      closeStream();
      setConnection("connecting");
      let snap: Snapshot;
      try {
        snap = await api.snapshot(roomID);
      } catch (e) {
        setConnection("idle");
        setError(`快照获取失败：${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      versionRef.current = snap.room_version;
      setState({
        roomID,
        version: snap.room_version,
        entries: snap.timeline.map((item) => ({
          key: item.event_id,
          kind: "message",
          actorID: item.actor_id,
          actorKind: item.actor_kind,
          occurredAt: item.occurred_at,
          body: item.body,
        })),
        typing: {},
        roundOpen: false,
        paused: false,
      });
      const es = new EventSource(
        `/v1/rooms/${encodeURIComponent(roomID)}/events?cursor=${encodeURIComponent(snap.watermark)}`,
      );
      esRef.current = es;
      es.onopen = () => setConnection("live");
      es.onerror = () => {
        if (es.readyState === EventSource.CLOSED) {
          setConnection("resync");
        } else {
          setConnection("reconnecting"); // 浏览器将带 Last-Event-ID 自动重连
        }
      };
      for (const type of SUBSCRIBED_EVENTS) {
        es.addEventListener(type, (ev) => {
          applyEvent(type, JSON.parse((ev as MessageEvent).data) as EventView);
        });
      }
      es.addEventListener("resync_required", () => {
        setConnection("resync");
        void loadAndSubscribe(roomID);
      });
      setConnection("live");
    },
    [applyEvent, closeStream],
  );

  useEffect(() => closeStream, [closeStream]);

  /** 发命令前以快照校准版本（引擎轮推进版本，SSE 帧不含版本）；409 兜底重试一次。 */
  const withVersion = useCallback(
    async (roomID: string, run: (version: number) => Promise<unknown>): Promise<void> => {
      try {
        const snap = await api.snapshot(roomID);
        versionRef.current = snap.room_version;
      } catch {
        // 校准失败沿用本地版本，由 409 兜底
      }
      try {
        await run(versionRef.current);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          const snap = await api.snapshot(roomID);
          versionRef.current = snap.room_version;
          await run(versionRef.current);
          return;
        }
        throw e;
      }
    },
    [],
  );

  const createRoom = useCallback(async () => {
    setError(null);
    try {
      const created = await api.createRoom("Web 房间");
      await loadAndSubscribe(created.room_id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [loadAndSubscribe]);

  const send = useCallback(
    async (body: string, addressedTo: string[] = []) => {
      if (!state) return;
      try {
        await withVersion(state.roomID, (v) => api.postMessage(state.roomID, v, body, addressedTo));
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [state, withVersion],
  );

  const pause = useCallback(async () => {
    if (!state) return;
    try {
      await withVersion(state.roomID, (v) => api.pauseRoom(state.roomID, v, "web"));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [state, withVersion]);

  const resume = useCallback(async () => {
    if (!state) return;
    try {
      await withVersion(state.roomID, (v) => api.resumeRoom(state.roomID, v));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [state, withVersion]);

  return {
    roomID: state?.roomID ?? null,
    version: state?.version ?? 0,
    entries: state?.entries ?? [],
    typing: state?.typing ?? {},
    roundOpen: state?.roundOpen ?? false,
    paused: state?.paused ?? false,
    connection,
    error,
    createRoom,
    send,
    pause,
    resume,
  };
}
