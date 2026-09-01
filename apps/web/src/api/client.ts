// HTTP 契约客户端：类型消费 api/http-api/openapi.yaml 的生成产物（ADR-0007）。
// 错误约定：非 2xx 抛 ApiError（稳定 code + message）；X-Trace-Id 始终记录最近值。
import type { components } from "./schema.gen";

export type Schemas = components["schemas"];
export type EventView = Schemas["EventView"];
export type Snapshot = Schemas["Snapshot"];
export type Executable = Schemas["Executable"];
export type CommandResponse = Schemas["CommandResponse"];
export type RoomSummary = Schemas["RoomSummary"];
export type ParticipantView = Schemas["ParticipantView"];
export type PolicyParams = Schemas["PolicyParams"];
export type ManualExecutableRequest = Schemas["ManualExecutableRequest"];

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

/** 最近一次命令响应的 trace id（开发者模式面板显示）。 */
export let lastTrace = "";

// Owner token（M2 写端点凭据）：同源 bootstrap 惰性获取、401 触发重试一次。
// 旧装配（无 token）bootstrap 404 → 保持 null，请求行为不变。
let ownerToken: string | null = null;
let bootstrapped = false;

async function ensureToken(): Promise<void> {
  if (bootstrapped) return;
  bootstrapped = true;
  try {
    const resp = await fetch("/v1/owner/bootstrap");
    if (resp.ok) {
      const d = (await resp.json()) as { token?: string };
      ownerToken = d.token ?? null;
    }
  } catch {
    ownerToken = null;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const doFetch = () => {
    const headers = new Headers(init?.headers);
    if (ownerToken && init?.method) {
      headers.set("X-Owner-Token", ownerToken);
    }
    return fetch(path, { ...init, headers });
  };
  let resp = await doFetch();
  if (resp.status === 401) {
    bootstrapped = false;
    await ensureToken();
    if (ownerToken) {
      resp = await doFetch();
    }
  }
  const trace = resp.headers.get("X-Trace-Id");
  if (trace) lastTrace = trace;
  const text = await resp.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = undefined;
    }
  }
  if (!resp.ok) {
    const err = (body as { error?: { code?: string; message?: string } })?.error;
    throw new ApiError(resp.status, err?.code ?? "unknown", err?.message ?? `HTTP ${resp.status}`);
  }
  return body as T;
}

function post<T = CommandResponse>(path: string, payload: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
}

/** UUIDv7（服务端幂等键契约：48bit 毫秒时间戳 + 版本/变体位 + 随机尾）。 */
export function uuidv7(): string {
  const rnd = new Uint8Array(9);
  crypto.getRandomValues(rnd);
  const hex = Array.from(rnd, (b) => b.toString(16).padStart(2, "0")).join("");
  const t = Date.now().toString(16).padStart(12, "0");
  return `${t.slice(0, 8)}-${t.slice(8, 12)}-7${hex.slice(0, 3)}-9${hex.slice(3, 6)}-${hex.slice(6, 18)}`;
}

function commandBody(kind: string, expectedVersion: number, payload: unknown) {
  return {
    command_kind: kind,
    expected_room_version: expectedVersion,
    idempotency_key: uuidv7(),
    issued_at: new Date().toISOString(),
    payload,
  };
}

export interface AgentSeatInfo {
  participant_id: string;
  adapter: string;
  display_name: string;
}

export const api = {
  agents(): Promise<{ agents: AgentSeatInfo[] }> {
    return request<{ agents: AgentSeatInfo[] }>("/v1/agents");
  },
  listRooms(): Promise<{ rooms: RoomSummary[] }> {
    return request<{ rooms: RoomSummary[] }>("/v1/rooms");
  },
  createRoom(displayName: string, agentsSel: string[] = []): Promise<CommandResponse> {
    return post("/v1/rooms", commandBody("create_room", 0, {
      display_name: displayName,
      ...(agentsSel.length > 0 ? { agents: agentsSel } : {}),
    }));
  },
  inviteAgent(roomID: string, version: number, participantID: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("invite_agent", version, { participant_id: participantID }),
    );
  },
  renameRoom(roomID: string, version: number, displayName: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("rename_room", version, { display_name: displayName }),
    );
  },
  postMessage(roomID: string, version: number, body: string, addressedTo: string[] = []): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("post_message", version, {
        body,
        reply_to: null,
        addressed_to: addressedTo,
        relations: [],
      }),
    );
  },
  pauseRoom(roomID: string, version: number, reason: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("pause_room", version, { reason }),
    );
  },
  resumeRoom(roomID: string, version: number): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("resume_room", version, {}),
    );
  },
  setPolicy(roomID: string, version: number, params: Schemas["PolicyParams"]): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("set_policy", version, params),
    );
  },
  endorseIntent(roomID: string, version: number, intentID: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("endorse_intent", version, { intent_id: intentID, effect: "grant" }),
    );
  },
  snapshot(roomID: string): Promise<Snapshot> {
    return request<Snapshot>(`/v1/rooms/${encodeURIComponent(roomID)}/snapshot`);
  },
  executables(): Promise<{ executables: Executable[] }> {
    return request<{ executables: Executable[] }>("/v1/harness/executables");
  },
  registerExecutable(req: ManualExecutableRequest): Promise<{ status: string }> {
    return post<{ status: string }>(`/v1/harness/executables`, req);
  },
  setEnabled(id: string, enabled: boolean): Promise<{ status: string; enabled: boolean }> {
    return request(`/v1/harness/executables/${encodeURIComponent(id)}/${enabled ? "enable" : "disable"}`, {
      method: "POST",
    });
  },
  debugState(roomID: string): Promise<unknown> {
    return request(`/v1/debug/rooms/${encodeURIComponent(roomID)}/state`);
  },
  debugEvents(roomID: string): Promise<unknown> {
    return request(`/v1/debug/rooms/${encodeURIComponent(roomID)}/events`);
  },
};
