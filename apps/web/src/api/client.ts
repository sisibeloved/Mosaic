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
export type CuratedEntry = NonNullable<NonNullable<components["schemas"]["MemoryView"]>["curated"]>[number];
export type CuratedBudget = NonNullable<NonNullable<components["schemas"]["MemoryView"]>["curated_budget"]>;
export type ContextPanorama = Schemas["ContextPanorama"];
export type ReceiptItem = NonNullable<NonNullable<Schemas["ReceiptListView"]>["receipts"]>[number];

/** RFC-0014 文档面类型（契约 api/http-api/openapi.yaml）。 */
export type DocSummary = Schemas["DocSummary"];
export type DocState = Schemas["DocState"];
export type DocBlock = Schemas["DocBlock"];
export type DocRef = Schemas["DocRef"];
export type DocSearchHit = Schemas["DocSearchHit"];
export type DocCommandResponse = Schemas["DocCommandResponse"];
export type AttachedDoc = Schemas["AttachedDoc"];

/** 文档命令种类（DocCommand.command_kind 封闭枚举）。 */
export type DocCommandKind =
  | "create_doc"
  | "rename_doc"
  | "commit_doc_revision"
  | "archive_doc"
  | "restore_doc"
  | "delete_doc"
  | "duplicate_doc";

/**
 * 修订批单条块操作（commit_doc_revision 载荷；契约
 * api/room-protocol/events/doc.revision_committed.schema.json）：
 * insert_after（block_id 锚点，null=文首；携带 block）/ append（block_id 恒
 * null，携带 block）/ replace（block_id + text）/ delete（block_id）。
 * 插入块的非空 block_id 服务端原样保留（空才分配）——编辑器自造稳定 ID。
 */
export interface DocOp {
  op: "insert_after" | "append" | "replace" | "delete";
  block_id: string | null;
  block?: DocBlock;
  text?: string;
}

/** 记忆查看面（GET /v1/rooms/{id}/memory）。 */
export interface MemoryView {
  room_id: string;
  capsules: MemoryCapsule[];
  capsule_budget: { budget_runes: number; injected_runes: number; injected_count: number; dropped_count: number };
  curated: CuratedEntry[];
  curated_budget: CuratedBudget;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  /** 409 文档命令冲突响应携带的当前态（RFC-0014 §2.3：version_conflict 时
   *  响应体另含 current_version + state——编辑器在新态上重放未提交 ops）。 */
  readonly docConflict?: { docID?: string; currentVersion?: number; state?: DocState };
  constructor(status: number, code: string, message: string, docConflict?: ApiError["docConflict"]) {
    super(message);
    this.status = status;
    this.code = code;
    this.docConflict = docConflict;
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
    let docConflict: ApiError["docConflict"];
    if (resp.status === 409) {
      const c = body as { doc_id?: string; current_version?: number; state?: DocState } | undefined;
      if (c?.state || c?.current_version !== undefined) {
        docConflict = { docID: c.doc_id, currentVersion: c.current_version, state: c.state };
      }
    }
    throw new ApiError(resp.status, err?.code ?? "unknown", err?.message ?? `HTTP ${resp.status}`, docConflict);
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

/** 文档命令信封（RFC-0014 / ADR-0014：镜像房间命令纪律——幂等 + 文档级乐观并发）。 */
function docCommandBody(kind: DocCommandKind, expectedVersion: number, payload: unknown) {
  return {
    command_kind: kind,
    expected_doc_version: expectedVersion,
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
    refs: DocRef[] = [],
  ): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("post_message", version, {
        body,
        reply_to: replyTo,
        addressed_to: addressedTo,
        relations: [],
        ...(attachments.length > 0 ? { attachments } : {}),
        // RFC-0014 §2.4：分享文档到房间 = 消息携带 doc_ref 描述子（服务端校验存在性）
        ...(refs.length > 0 ? { refs } : {}),
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
  editMemory(roomID: string, version: number, memoryID: string, edits: { conclusions?: string[]; assumptions?: string[]; curatedContent?: string }, note: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("edit_memory", version, {
        memory_id: memoryID,
        conclusions: edits.conclusions ?? null,
        assumptions: edits.assumptions ?? null,
        curated_content: edits.curatedContent ?? null,
        note,
      }),
    );
  },
  editCuratedMemory(roomID: string, version: number, entryID: string, content: string, note: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("edit_memory", version, {
        memory_id: entryID,
        conclusions: null,
        assumptions: null,
        curated_content: content,
        note,
      }),
    );
  },
  roomMemory(roomID: string): Promise<MemoryView> {
    return request<MemoryView>(`/v1/rooms/${encodeURIComponent(roomID)}/memory`);
  },
  roomContext(roomID: string): Promise<ContextPanorama> {
    return request<ContextPanorama>(`/v1/rooms/${encodeURIComponent(roomID)}/context`);
  },
  roomReceipts(roomID: string, limit = 20): Promise<{ room_id: string; receipts: ReceiptItem[] }> {
    return request(`/v1/rooms/${encodeURIComponent(roomID)}/receipts?limit=${limit}`);
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
  // ---- RFC-0014 文档面 ----
  /** 文档列表（§2.8 主页：updated_at 倒序；created_by/status 服务端筛选）。 */
  listDocs(filter: { createdBy?: string; status?: "active" | "archived" } = {}): Promise<{ docs: DocSummary[] }> {
    const params = new URLSearchParams();
    if (filter.createdBy) params.set("created_by", filter.createdBy);
    if (filter.status) params.set("status", filter.status);
    const q = params.toString();
    return request<{ docs: DocSummary[] }>(`/v1/docs${q ? `?${q}` : ""}`);
  },
  /** 文档快照（当前态：标题/版本/状态/块清单）。 */
  getDoc(docID: string): Promise<DocState> {
    return request<DocState>(`/v1/docs/${encodeURIComponent(docID)}`);
  },
  /** 创建文档（create_doc 命令便捷包装；doc_id 服务端分配）。 */
  createDoc(title: string, initialBlocks?: DocBlock[]): Promise<DocCommandResponse> {
    return post<DocCommandResponse>(
      "/v1/docs",
      docCommandBody("create_doc", 0, {
        title,
        ...(initialBlocks && initialBlocks.length > 0 ? { initial_blocks: initialBlocks } : {}),
      }),
    );
  },
  /** 文档命令统一入口（rename/archive/restore/delete/duplicate 等；409 经 ApiError.docConflict 携带当前态）。 */
  docCommand(docID: string, kind: DocCommandKind, expectedVersion: number, payload: unknown): Promise<DocCommandResponse> {
    return post<DocCommandResponse>(
      `/v1/docs/${encodeURIComponent(docID)}/commands`,
      docCommandBody(kind, expectedVersion, payload),
    );
  },
  /** 修订批（§2.3：base_version CAS；ops 为一批块操作——编辑器防抖聚合产物）。 */
  commitDocRevision(docID: string, baseVersion: number, ops: DocOp[], note?: string): Promise<DocCommandResponse> {
    return post<DocCommandResponse>(
      `/v1/docs/${encodeURIComponent(docID)}/commands`,
      docCommandBody("commit_doc_revision", baseVersion, {
        base_version: baseVersion,
        ops,
        ...(note ? { note } : {}),
      }),
    );
  },
  /** 文档全文检索（FTS5 trigram；CJK ≥3 字子串语义）。 */
  searchDocs(q: string, limit = 20): Promise<{ hits: DocSearchHit[] }> {
    const params = new URLSearchParams({ q, limit: String(limit) });
    return request<{ hits: DocSearchHit[] }>(`/v1/docs/search?${params.toString()}`);
  },
  /** 导出 markdown 下载地址（GET 读端点，同源 <a download> 直接可用）。 */
  exportDocUrl(docID: string): string {
    return `/v1/docs/${encodeURIComponent(docID)}/export`;
  },
  /** §2.4 房间文档附着/解除（房间命令；附着不复制不锁定，重复/未附着 = 幂等空操作）。 */
  attachDoc(roomID: string, version: number, docID: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("attach_doc_to_room", version, { doc_id: docID }),
    );
  },
  detachDoc(roomID: string, version: number, docID: string): Promise<CommandResponse> {
    return post(
      `/v1/rooms/${encodeURIComponent(roomID)}/commands`,
      commandBody("detach_doc_from_room", version, { doc_id: docID }),
    );
  },
};
