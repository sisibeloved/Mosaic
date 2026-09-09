// 房间列表 store（零依赖：useSyncExternalStore + 模块级状态）。
// 刷新时机：进入壳、建房/改名后、房间内新事件（RoomPage 防抖触发）；不轮询。
import { useSyncExternalStore } from "react";
import { api, ApiError, type RoomSummary } from "../api/client";

export interface RoomsState {
  rooms: RoomSummary[] | null; // null = 尚未加载
  error: string | null;
}

let state: RoomsState = { rooms: null, error: null };
const listeners = new Set<() => void>();
let inflight: Promise<void> | null = null;

function emit(next: RoomsState): void {
  state = next;
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getState(): RoomsState {
  return state;
}

export function useRooms(): RoomsState {
  return useSyncExternalStore(subscribe, getState);
}

/** 拉取房间列表（单飞去重——并发调用共享一次请求）。 */
export async function refreshRooms(): Promise<void> {
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const { rooms } = await api.listRooms();
      emit({ rooms, error: null });
    } catch (e) {
      emit({ rooms: state.rooms, error: e instanceof Error ? e.message : String(e) });
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

/** 建房（默认名"新房间"；agents = 入房 Agent 选择，空 = 全部在席）→ 刷新列表 → 返回 room_id 供跳转。 */
export async function createRoom(displayName = "新房间", agents: string[] = []): Promise<string> {
  const created = await api.createRoom(displayName, agents);
  await refreshRooms();
  return created.room_id;
}

/**
 * 房间命令的独立执行入口（v1.73 侧栏右键动作用——无 useRoom 上下文）：
 * 先拉快照校准版本，409 时重拉重试一次（与 room.ts withVersion 同策略）。
 * 成功后刷新房间列表（侧栏是发起方，列表是它的事实源）。
 */
export async function execRoomCommand<T>(
  roomID: string,
  run: (version: number) => Promise<T>,
): Promise<T> {
  let snap = await api.snapshot(roomID);
  try {
    const out = await run(snap.room_version);
    await refreshRooms();
    return out;
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) {
      snap = await api.snapshot(roomID);
      const out = await run(snap.room_version);
      await refreshRooms();
      return out;
    }
    throw e;
  }
}
