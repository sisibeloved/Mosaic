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
export type ManualExecutableRequest = Schemas["ManualExecutableRequest"];
export type TaskItem = Schemas["TaskItem"];
export type SearchHit = Schemas["SearchHit"];
export type RuntimeOptions = Schemas["RuntimeOptions"];
export type MemoryCapsule = NonNullable<NonNullable<components["schemas"]["MemoryView"]>["capsules"]>[number];

/** 记忆查看面（GET /v1/rooms/{id}/memory）。 */
export interface MemoryView {
  room_id: string;
  capsules: MemoryCapsule[];
  capsule_budget: { budget_runes: number; injected_runes: number; injected_count: number; dropped_count: number };
}

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

function put<T>(path: string, payload: unknown): Promise<T> {
  return request<T>(path, {
    method: "PUT",
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

/** 已发现但未启用的可执行项（v1.24：选人页如实展示，指路设置）。 */
export interface DisabledAgentInfo {
  adapter: string;
  channel?: string;
  version?: string;
}

/** M4-0 备份摘要（GET/POST /v1/system/backups）。 */
export interface BackupSummary {
  backup_id: string;
  created_at: string;
  size_bytes: number;
}

/** OQ-B 设置族文档（M4-1 首员 run_timeout_seconds）。 */
export type SettingsDoc = Schemas["SettingsDoc"];

/** M4-5 监控源 + 运行态视图。 */
export type MonitorView = Schemas["MonitorView"];
export type MonitorCreate = Schemas["MonitorCreate"];

export const api = {
  agents(): Promise<{ agents: AgentSeatInfo[]; disabled?: DisabledAgentInfo[] }> {
    return request<{ agents: AgentSeatInfo[]; disabled?: DisabledAgentInfo[] }>("/v1/agents");
  },
  listBackups(): Promise<{ backups: BackupSummary[] }> {
    return request<{ backups: BackupSummary[] }>("/v1/system/backups");
  },
  createBackup(): Promise<BackupSummary> {
    return post("/v1/system/backups", {});
  },
  requestRestore(backupID: string): Promise<{ restart_required: boolean }> {
    return post(`/v1/system/restore`, { backup_id: backupID, confirm: true });
  },
  diagnostics(): Promise<Record<string, unknown>> {
    return request<Record<string, unknown>>("/v1/system/diagnostics");
  },
  /** OQ-B 设置族（M4-1 首员）：run_timeout_seconds 读写（写后对新拉起的 run 即时生效）。 */
  settings(): Promise<SettingsDoc> {
    return request<SettingsDoc>("/v1/system/settings");
  },
  updateSettings(doc: SettingsDoc): Promise<SettingsDoc> {
    return put("/v1/system/settings", doc);
  },
  /** M4-5 监控面。 */
  monitors(): Promise<{ monitors: MonitorView[] }> {
    return request<{ monitors: MonitorView[] }>("/v1/monitors");
  },
  createMonitor(src: MonitorCreate): Promise<MonitorView> {
    return post<MonitorView>("/v1/monitors", src);
  },
  deleteMonitor(id: string): Promise<void> {
    return request<void>(`/v1/monitors/${encodeURIComponent(id)}`, { method: "DELETE" });
  },
  monitorAction(id: string, action: "enable" | "disable" | "check"): Promise<void> {
    return request<void>(`/v1/monitors/${encodeURIComponent(id)}/${action}`, { method: "POST" });
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
  postMessage(
    roomID: string,
    version: number,
    body: string,
    addressedTo: string[] = [],
    replyTo: string | null = null,
    attachments: string[] = [],
  ): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("post_message", version, {
        body,
        reply_to: replyTo,
        addressed_to: addressedTo,
        relations: [],
        ...(attachments.length > 0 ? { attachments } : {}),
      }),
    );
  },
  /** RFC-0013 第一步：multipart 上传 → 令牌（进 postMessage attachments）。 */
  uploadAttachment(roomID: string, file: File): Promise<{ token: string; name: string; size_bytes: number }> {
    const fd = new FormData();
    fd.append("file", file);
    return request(`/v1/rooms/${encodeURIComponent(roomID)}/attachments`, {
      method: "POST",
      body: fd, // Content-Type 由浏览器带 multipart boundary；不经 JSON 序列化
    });
  },
  attachmentURL(roomID: string, attachmentID: string): string {
    return `/v1/rooms/${encodeURIComponent(roomID)}/attachments/${encodeURIComponent(attachmentID)}`;
  },
  proposeClosure(roomID: string, version: number, threadID: string | null, hint?: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("propose_closure", version, { thread_id: threadID, closure_hint: hint ?? null }),
    );
  },
  acceptClosure(roomID: string, version: number, closureID: string | null): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("accept_closure", version, { closure_id: closureID }),
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
  /** M4-1 任务执行通道：发起独立执行（结果回传房间；assignee 须为支持执行的 agent 座位）。 */
  runTask(roomID: string, version: number, assignee: string, instruction: string, taskID?: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("run_task", version, { assignee, instruction, ...(taskID ? { task_id: taskID } : {}) }),
    );
  },
  cancelRun(roomID: string, version: number, runID: string, reason: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("cancel_run", version, { run_id: runID, reason }),
    );
  },
  deleteRoom(roomID: string, version: number, reason: string): Promise<CommandResponse> {
    // M3-6：reason 必填 1..280 字（删除不可逆、级联清库，理由留痕）。
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("delete_room", version, { reason }),
    );
  },
  endorseIntent(roomID: string, version: number, intentID: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("endorse_intent", version, { intent_id: intentID, effect: "grant" }),
    );
  },
  resolveTask(roomID: string, version: number, taskID: string, resolution: "delivered" | "dismissed", note?: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("resolve_task", version, { task_id: taskID, resolution, note: note ?? null }),
    );
  },
  editMemory(roomID: string, version: number, memoryID: string, edits: { conclusions?: string[]; assumptions?: string[] }, note: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("edit_memory", version, {
        memory_id: memoryID,
        conclusions: edits.conclusions ?? null,
        assumptions: edits.assumptions ?? null,
        note,
      }),
    );
  },
  roomMemory(roomID: string): Promise<MemoryView> {
    return request<MemoryView>(`/v1/rooms/${encodeURIComponent(roomID)}/memory`);
  },
  searchMessages(roomID: string, q: string, actor?: string, limit = 20): Promise<{ hits: SearchHit[] }> {
    const params = new URLSearchParams({ q, limit: String(limit) });
    if (actor) params.set("actor", actor);
    return request(`/v1/rooms/${encodeURIComponent(roomID)}/search?${params.toString()}`);
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
  /** v1.48 运行参数：模型覆盖与思考强度（全量替换；空串 = 清除回 CLI 默认）。 */
  updateExecutable(id: string, model: string, reasoningEffort: string): Promise<{ status: string }> {
    return request(`/v1/harness/executables/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model, reasoning_effort: reasoningEffort }),
    });
  },
  /** v1.48 模型候选与强度档位（kimi 实查；codex 五档强度；mcode 空面）。 */
  executableModels(id: string): Promise<RuntimeOptions> {
    return request(`/v1/harness/executables/${encodeURIComponent(id)}/models`);
  },
  debugState(roomID: string): Promise<unknown> {
    return request(`/v1/debug/rooms/${encodeURIComponent(roomID)}/state`);
  },
  debugEvents(roomID: string): Promise<unknown> {
    return request(`/v1/debug/rooms/${encodeURIComponent(roomID)}/events`);
  },
  debugWaves(roomID: string, cursor?: string): Promise<unknown> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
    return request(`/v1/debug/rooms/${encodeURIComponent(roomID)}/waves${q}`);
  },
};
