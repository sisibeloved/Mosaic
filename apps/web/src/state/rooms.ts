// 房间列表 + 会话席位 store（零依赖：useSyncExternalStore + 模块级状态）。
// 刷新时机：进入壳、建房/改名后、房间内新事件（RoomPage 防抖触发）；不轮询。
// v1.74：在席 Agent 席位（联系人收敛进侧栏私聊组的数据源）——启停/登记/扫描后
// 显式 refreshAgentSeats；房间事件不刷（席位不随消息变化）。
import { useSyncExternalStore } from "react";
import { api, ApiError, type AgentSeatInfo, type DisabledAgentInfo, type RoomSummary } from "../api/client";

export interface RoomsState {
  rooms: RoomSummary[] | null; // null = 尚未加载
  error: string | null;
  seats: AgentSeatInfo[] | null; // null = 尚未加载（echo 桩已过滤）
  disabledSeats: DisabledAgentInfo[]; // 已发现未启用（灰芯片展示）
}

let state: RoomsState = { rooms: null, error: null, seats: null, disabledSeats: [] };
const listeners = new Set<() => void>();
let inflight: Promise<void> | null = null;
let seatsInflight: Promise<void> | null = null;

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

/** 拉取在席 Agent 席位 + 未启用项（单飞去重；echo 测试桩过滤——不出会话面）。 */
export async function refreshAgentSeats(): Promise<void> {
  if (seatsInflight) return seatsInflight;
  seatsInflight = (async () => {
    try {
      const { agents, disabled } = await api.agents();
      emit({
        ...state,
        seats: agents.filter((a) => a.participant_id !== "par_echo"),
        disabledSeats: disabled ?? [],
      });
    } catch {
      // 席位拉取失败保持原状（列表主刷新的错误面在 refreshRooms）
    } finally {
      seatsInflight = null;
    }
  })();
  return seatsInflight;
}

/** 拉取房间列表（单飞去重——并发调用共享一次请求）。 */
export async function refreshRooms(): Promise<void> {
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const { rooms } = await api.listRooms();
      emit({ ...state, rooms, error: null });
    } catch (e) {
      emit({ ...state, rooms: state.rooms, error: e instanceof Error ? e.message : String(e) });
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
 * 私聊 find-or-create（v1.74 联系人收敛进侧栏）：用列表 agents 字段直接判定
 * 该 Agent 是否已有私聊房（单员房间）——不逐房拉快照；没有则建房（名字 = Agent
 * 显示名，roster 恰为该 bot——与私聊判定语义闭环）。
 */
export async function findOrCreateDM(bot: { participant_id: string; display_name: string }): Promise<string> {
  const { rooms } = await api.listRooms();
  const dm = rooms.find(
    (r) => (r.agents?.length ?? 0) === 1 && r.agents![0] === bot.participant_id,
  );
  if (dm) return dm.room_id;
  const created = await api.createRoom(bot.display_name || bot.participant_id, [bot.participant_id]);
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
