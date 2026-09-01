// 房间列表 store（零依赖：useSyncExternalStore + 模块级状态）。
// 刷新时机：进入壳、建房/改名后、房间内新事件（RoomPage 防抖触发）；不轮询。
import { useSyncExternalStore } from "react";
import { api, type RoomSummary } from "../api/client";

export interface RoomsState {
  rooms: RoomSummary[] | null; // null = 尚未加载
  error: string | null;
}

const LAST_ROOM_KEY = "mosaic.lastRoomId";

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

export function getLastRoomId(): string | null {
  try {
    return localStorage.getItem(LAST_ROOM_KEY);
  } catch {
    return null;
  }
}

export function setLastRoomId(roomID: string): void {
  try {
    localStorage.setItem(LAST_ROOM_KEY, roomID);
  } catch {
    // 存储不可用时跳过记忆
  }
}
