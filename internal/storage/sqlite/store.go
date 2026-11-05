// Package sqlite 是 Room Event Log 的 SQLite（WAL）存储层（ADR-0008 个人版形态）。
// 机制映射：房间串行写 → BEGIN IMMEDIATE 互斥事务；订阅续传 → 全局位（global_pos）
// 构造的 opaque cursor；outbox SKIP LOCKED → 进程内提交后分发 + 崩溃后按表重放。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/delivery"
	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/execctx"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
	"github.com/sisibeloved/Mosaic/internal/slot"

	_ "modernc.org/sqlite"
)

// ErrDuplicateEvent 表示 event_id 已存在（幂等追加语义：上游应视作已成功）。
var ErrDuplicateEvent = errors.New("sqlite: duplicate event_id")

const schemaDDL = `
CREATE TABLE IF NOT EXISTS room_events (
	global_pos    INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id      TEXT NOT NULL UNIQUE,
	tenant_id     TEXT NOT NULL,
	room_id       TEXT NOT NULL,
	seq           INTEGER NOT NULL,
	type          TEXT NOT NULL,
	schema_version INTEGER NOT NULL,
	occurred_at   TEXT NOT NULL,
	envelope      TEXT NOT NULL,
	UNIQUE (room_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_room_events_room_pos ON room_events(room_id, global_pos);

CREATE TABLE IF NOT EXISTS outbox (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id      TEXT NOT NULL,
	global_pos   INTEGER NOT NULL UNIQUE,
	event_id     TEXT NOT NULL UNIQUE,
	envelope     TEXT NOT NULL,
	dispatched_at TEXT,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS command_receipts (
	tenant_id           TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	command_kind        TEXT NOT NULL,
	room_id             TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	event_id            TEXT NOT NULL,
	room_version        INTEGER NOT NULL,
	executed_at         TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idempotency_key, command_kind)
);
CREATE TABLE IF NOT EXISTS context_receipts (
	receipt_id    TEXT PRIMARY KEY,
	room_id       TEXT NOT NULL,
	task_id       TEXT NOT NULL,
	watermark     INTEGER NOT NULL,
	layer_digests TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS engine_claims (
	room_id           TEXT NOT NULL,
	stimulus_event_id TEXT NOT NULL,
	envelope          TEXT NOT NULL,
	position          INTEGER NOT NULL DEFAULT 0,
	claimed_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY (room_id, stimulus_event_id)
);
CREATE TABLE IF NOT EXISTS room_tombstones (
  room_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (2, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (3, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (4, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (5, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(id) WHERE dispatched_at IS NULL;

-- v4（RFC-0014 / ADR-0014）：per-doc 事件流（同一 SQLite 库、不同表）——
-- version per-doc 单调递增，兼任文档级 CAS 序号与 doc:{id} 频道 SSE cursor。
-- outbox/receipts/tombstones 独立成表：房间表面 room_id NOT NULL 且假设房间信封，不可复用。
CREATE TABLE IF NOT EXISTS doc_events (
	event_id       TEXT NOT NULL UNIQUE,
	tenant_id      TEXT NOT NULL,
	doc_id         TEXT NOT NULL,
	version        INTEGER NOT NULL,
	type           TEXT NOT NULL,
	schema_version INTEGER NOT NULL,
	occurred_at    TEXT NOT NULL,
	envelope       TEXT NOT NULL,
	UNIQUE (doc_id, version)
);
CREATE INDEX IF NOT EXISTS idx_doc_events_doc ON doc_events(doc_id, version);

CREATE TABLE IF NOT EXISTS doc_outbox (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id       TEXT NOT NULL,
	version      INTEGER NOT NULL,
	event_id     TEXT NOT NULL UNIQUE,
	envelope     TEXT NOT NULL,
	dispatched_at TEXT,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_doc_outbox_pending ON doc_outbox(id) WHERE dispatched_at IS NULL;

CREATE TABLE IF NOT EXISTS doc_command_receipts (
	tenant_id           TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	command_kind        TEXT NOT NULL,
	doc_id              TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	event_id            TEXT NOT NULL,
	doc_version         INTEGER NOT NULL,
	executed_at         TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idempotency_key, command_kind)
);
CREATE TABLE IF NOT EXISTS doc_tombstones (
  doc_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);

-- v5（RFC-0015）：文档级外部共享映射（快照语义——记录创建时点，远端内容
-- 不回同步）。非事件派生的权威状态（外部副作用不可由本地事件流重建），
-- 故独立成表而非投影；文档删除不级联（解除映射是显式动作）。
CREATE TABLE IF NOT EXISTS doc_external_shares (
  doc_id       TEXT NOT NULL,
  provider     TEXT NOT NULL,
  external_url TEXT NOT NULL,
  shared_by    TEXT NOT NULL,
  shared_at    TEXT NOT NULL,
  PRIMARY KEY (doc_id, provider)
);

-- v6（RFC-0017 M2）：表格手动交换记录（副本语义——远端副作用为权威事实，
-- 非事件派生，独立成表）。幂等键唯一支撑 execute 重放去重；running 态
-- 仅崩溃窗口存在（启动扫成 unknown，M2 不做自动恢复）。
CREATE TABLE IF NOT EXISTS sheet_transfers (
  transfer_id     TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  direction       TEXT NOT NULL,
  provider        TEXT NOT NULL,
  status          TEXT NOT NULL,
  doc_id          TEXT NOT NULL DEFAULT '',
  external_url    TEXT NOT NULL DEFAULT '',
  title           TEXT NOT NULL DEFAULT '',
  source_version  INTEGER NOT NULL DEFAULT 0,
  snapshot_hash   TEXT NOT NULL DEFAULT '',
  report_json     TEXT NOT NULL DEFAULT '',
  error           TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  -- M3 回写：进度与恢复状态（state_json 含目标前态/剩余计划——不出网）
  cells_total     INTEGER NOT NULL DEFAULT 0,
  cells_written   INTEGER NOT NULL DEFAULT 0,
  state_json      TEXT NOT NULL DEFAULT ''
);

-- v7（RFC-0018 / ADR-0017，0.5.0）：独立 Agent 身份域。agents 为聚合快照
-- （0.5.0 无状态转移，事件流随 0.5.1 引入后本表降为投影重建基线）；
-- agent_aliases 为旧 ID → AgentID 的只增解析面（bot_id / profile_id /
-- participant_id 三类），(kind, value) 唯一——别名一经登记不改写、不删除。
CREATE TABLE IF NOT EXISTS agents (
  agent_id     TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  adapter      TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'active',
  source       TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_aliases (
  alias_kind  TEXT NOT NULL,
  alias_value TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (alias_kind, alias_value)
);
CREATE INDEX IF NOT EXISTS idx_agent_aliases_agent ON agent_aliases(agent_id);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (7, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v8（RFC-0018 §3.2 / ADR-0018，0.5.1）：Agent 生命周期事件流（append-only）。
-- agents 自 v8 起为投影——转移 = 单事务 [状态校验 → 更新投影 → 追加事件]，
-- 事件流 ⊇ 全部历史，投影可由流重建。回填：v7 存量行按创建序（rowid）补
-- created 事件（payload 对齐行值、occurred_at 取行内 created_at）；
-- NOT EXISTS 守卫使回填幂等——新库空集、重开不重复。
CREATE TABLE IF NOT EXISTS agent_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  agent_id    TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_events_agent ON agent_events(agent_id, seq);
INSERT INTO agent_events (agent_id, event_type, payload, occurred_at)
SELECT a.agent_id, 'created',
       json_object('source', a.source, 'adapter', a.adapter, 'display_name', a.display_name),
       a.created_at
  FROM agents a
 WHERE NOT EXISTS (SELECT 1 FROM agent_events e
                    WHERE e.agent_id = a.agent_id AND e.event_type = 'created')
 ORDER BY a.rowid;
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (8, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v9（RFC-0018 §5.1 / ADR-0019，0.5.2）：职责模板。slot_specs 为不可变版本行
-- ——(slot_spec_id, version) 主键，修订即追加新版本，无 UPDATE/DELETE 路径
-- （版本行本身即不可变历史，不另设事件流）。content_json 为完整内容（权威
-- 事实；name 冗余列便于谱系列表直查）。新域首表，无存量回填。
CREATE TABLE IF NOT EXISTS slot_specs (
  slot_spec_id    TEXT NOT NULL,
  version         INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL,
  name            TEXT NOT NULL,
  responsibility  TEXT NOT NULL,
  content_json    TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (slot_spec_id, version)
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (9, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v10（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3）：职责实例。slot_instances 为
-- 某作用域内实际承担的一份具体职责：引用模板谱系不固定版本（Binding 才
-- 固定，随 0.5.4）；knowledge_namespace 全局唯一、终身稳定、永不重派——
-- 职责知识（0.7 分域记忆）挂该命名空间，不挂任职者私有库。实例无状态机
-- （RFC §3.2 未定义），无 UPDATE/DELETE 路径。
CREATE TABLE IF NOT EXISTS slot_instances (
  slot_instance_id    TEXT PRIMARY KEY,
  slot_spec_id        TEXT NOT NULL,
  scope               TEXT NOT NULL DEFAULT 'workspace',
  title               TEXT NOT NULL,
  knowledge_namespace TEXT NOT NULL UNIQUE,
  created_from_version INTEGER NOT NULL,
  created_at          TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (10, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v11（RFC-0018 §3.2/§5.2 / ADR-0021，0.5.4）：Binding 任职。slot_bindings 为
-- 投影（转移 = 单事务[校验→投影→事件]）；单 active 由部分唯一索引库级兜底
-- （AT-13：并发激活恒 ≤1 在任）；released 终态留审计，无 UPDATE-外/DELETE
-- 路径。binding_events 为 append-only 流（created / status_changed）。
CREATE TABLE IF NOT EXISTS slot_bindings (
  binding_id        TEXT PRIMARY KEY,
  agent_id          TEXT NOT NULL,
  slot_instance_id  TEXT NOT NULL,
  slot_spec_version INTEGER NOT NULL,
  scope             TEXT NOT NULL,
  status            TEXT NOT NULL,
  handoff_note      TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_slot_bindings_single_active
  ON slot_bindings(slot_instance_id) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_slot_bindings_instance ON slot_bindings(slot_instance_id);
CREATE TABLE IF NOT EXISTS binding_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  binding_id  TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_binding_events_binding ON binding_events(binding_id, seq);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (11, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v12（RFC-0018 §5.2 / ADR-0022，0.5.5）：职责准入——AgentGrant 授权档案
-- （用户授权侧）。一 Agent 一档（agent_id 主键），PUT 全量替换（upsert）；
-- 无档案 = 空授权（安全缺省：一切声明要求缺失 → blocked）。清单列存 JSON
-- 数组；不建事件流（单用户 owner-token 本地应用，权衡见 ADR-0022）。
CREATE TABLE IF NOT EXISTS slot_agent_grants (
  agent_id             TEXT PRIMARY KEY,
  skills               TEXT NOT NULL DEFAULT '[]',
  tools                TEXT NOT NULL DEFAULT '[]',
  context              TEXT NOT NULL DEFAULT '[]',
  runtime_capabilities TEXT NOT NULL DEFAULT '[]',
  workspace_access     TEXT NOT NULL DEFAULT 'none',
  updated_at           TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (12, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v13（RFC-0018 §3.1/§3.2/§6.1 / ADR-0023，0.6.0）：Mission 契约独立事实源。
-- missions 为投影（修订/取消 = 单事务[校验→投影→版本行→事件]）；CAS 经
-- BEGIN IMMEDIATE 临界区（读在写锁内，败者 version_conflict 可见拒绝）。
-- mission_contract_versions 不可变版本行（修订即追加，无 UPDATE-外/DELETE
-- 路径）；mission_events 为 append-only 流。objective 列为列表摘要冗余，
-- 契约全文在 content JSON（写入单源）。
CREATE TABLE IF NOT EXISTS missions (
  mission_id       TEXT PRIMARY KEY,
  contract_version INTEGER NOT NULL,
  status           TEXT NOT NULL,
  owner_agent_id   TEXT NOT NULL DEFAULT '',
  requester        TEXT NOT NULL DEFAULT 'owner',
  objective        TEXT NOT NULL,
  content          TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  slot_instance_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS mission_contract_versions (
  mission_id       TEXT NOT NULL,
  version          INTEGER NOT NULL,
  status           TEXT NOT NULL,
  owner_agent_id   TEXT NOT NULL,
  requester        TEXT NOT NULL DEFAULT 'owner',
  content          TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  PRIMARY KEY (mission_id, version)
);
CREATE TABLE IF NOT EXISTS mission_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  mission_id  TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mission_events_mission ON mission_events(mission_id, seq);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (13, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v14（RFC-0018 §4.4–4.8/§6.1/§11.2 / ADR-0024，0.6.1）：房间交办与网关
-- 首切片。mission_sources 为 (room_id, task_id) → mission_id 唯一映射
--（同一任务卡至多交办一次；映射为创建事实）；dispatch_decisions 存路由
-- 决策（含请求全文——投递以存档为准）；dispatch_receipts 为投递回执，
-- (caller_ref, scope, idempotency_key) 唯一约束兜底幂等（同键重放/异载荷
-- 拒绝；并发孪生恰一胜者）。
CREATE TABLE IF NOT EXISTS mission_sources (
  room_id    TEXT NOT NULL,
  task_id    TEXT NOT NULL,
  mission_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (room_id, task_id)
);
CREATE INDEX IF NOT EXISTS idx_mission_sources_mission ON mission_sources(mission_id);
CREATE TABLE IF NOT EXISTS dispatch_decisions (
  route_id         TEXT PRIMARY KEY,
  decision_version INTEGER NOT NULL,
  request_digest   TEXT NOT NULL,
  status           TEXT NOT NULL,
  selection_source TEXT NOT NULL,
  reason_code      TEXT NOT NULL DEFAULT '',
  reason_summary   TEXT NOT NULL DEFAULT '',
  target_refs      TEXT NOT NULL DEFAULT '[]',
  request          TEXT NOT NULL,
  resolved_agent_id    TEXT NOT NULL DEFAULT '',
  resolved_instance_id TEXT NOT NULL DEFAULT '',
  expires_at       TEXT NOT NULL,
  created_at       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dispatch_receipts (
  receipt_id       TEXT PRIMARY KEY,
  caller_ref       TEXT NOT NULL,
  scope            TEXT NOT NULL,
  idempotency_key  TEXT NOT NULL,
  request_id       TEXT NOT NULL DEFAULT '',
  request_digest   TEXT NOT NULL,
  route_id         TEXT NOT NULL,
  decision_version INTEGER NOT NULL,
  domain_command_id TEXT NOT NULL,
  target_refs      TEXT NOT NULL DEFAULT '[]',
  return_refs      TEXT NOT NULL DEFAULT '[]',
  reason           TEXT NOT NULL DEFAULT '',
  committed_at     TEXT NOT NULL,
  UNIQUE (caller_ref, scope, idempotency_key)
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (14, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
-- v15（RFC-0018 §7.3/§6 / ADR-0025，0.6.2）：执行上下文清单。
-- exec_context_manifests 存档装配（钉契约版本/任职/摘要 + 层账本含内容
-- 块 + 水位向量）；(mission_id, digest) 唯一——确定性重建命中即重放原清
-- 单，状态变化追加新行（每 Mission 保留各时点装配历史）。
-- exec_context_receipts 为交付回执（只记账目不复制内容；run_id 随 0.6.5
-- Run 对象存在时增列）。表名冠 exec_ 与房间域 RFC-0007 context_receipts
--（对话上下文回执，v1 表）区分。两表是有明确所有者的持久记录（RFC §6），
-- 不进事件流。
CREATE TABLE IF NOT EXISTS exec_context_manifests (
  manifest_id     TEXT PRIMARY KEY,
  mission_id      TEXT NOT NULL,
  contract_version INTEGER NOT NULL,
  binding_id      TEXT NOT NULL,
  agent_id        TEXT NOT NULL,
  state           TEXT NOT NULL,
  reason_code     TEXT NOT NULL DEFAULT '',
  digest          TEXT NOT NULL,
  policy_version  TEXT NOT NULL,
  layers          TEXT NOT NULL,
  watermarks      TEXT NOT NULL,
  total_bytes     INTEGER NOT NULL,
  created_at      TEXT NOT NULL,
  UNIQUE (mission_id, digest)
);
CREATE INDEX IF NOT EXISTS idx_exec_context_manifests_mission ON exec_context_manifests(mission_id);
CREATE TABLE IF NOT EXISTS exec_context_receipts (
  receipt_id     TEXT PRIMARY KEY,
  manifest_id    TEXT NOT NULL UNIQUE,
  digest         TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  accounting     TEXT NOT NULL,
  total_bytes    INTEGER NOT NULL,
  created_at     TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (15, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
-- v16（RFC-0018 §3/§6 / ADR-0026，0.6.3）：本地 Environment / Runtime。
-- environments 为命名工作区目录（workspace_dir 唯一；is_default 部分唯一
-- 索引——全库恰一默认，装配启动确保）。runtimes 镜像 harness 登记事实 +
-- 本域状态与绑定，(adapter, host, distro, path) 唯一为发现幂等键（sync
-- 单向搬运不回写）。runtime_events 为运行时流（发现/健康/状态——健康
-- 历史可查）；environments 无事件流（有明确所有者的持久记录）。
CREATE TABLE IF NOT EXISTS environments (
  environment_id TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  workspace_dir  TEXT NOT NULL UNIQUE,
  is_default     INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_environments_single_default ON environments(is_default) WHERE is_default = 1;
CREATE TABLE IF NOT EXISTS runtimes (
  runtime_id     TEXT PRIMARY KEY,
  adapter        TEXT NOT NULL,
  host           TEXT NOT NULL,
  distro         TEXT NOT NULL DEFAULT '',
  path           TEXT NOT NULL,
  channel        TEXT NOT NULL DEFAULT '',
  version        TEXT NOT NULL DEFAULT '',
  login          TEXT NOT NULL DEFAULT 'unknown',
  state          TEXT NOT NULL,
  environment_id TEXT NOT NULL DEFAULT '',
  last_health_at TEXT NOT NULL DEFAULT '',
  last_health_note TEXT NOT NULL DEFAULT '',
  discovered_at  TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  UNIQUE (adapter, host, distro, path)
);
CREATE INDEX IF NOT EXISTS idx_runtimes_state ON runtimes(state);
CREATE TABLE IF NOT EXISTS runtime_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  runtime_id  TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_events_runtime ON runtime_events(runtime_id, seq);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (16, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
-- v17（RFC-0018 §5.2/§10.3.1 / ADR-0027，0.6.4）：RuntimeLease——工作区
-- 占用、互斥与围栏代次。两个部分唯一索引承载互斥：单运行时并发 1
--（容量）+ 同一可写工作区（环境）独占（跨运行时，§5.2"同一可写工作区
-- 须独占租约"）。代次单源于本表（运行时当代 = MAX(generation)，无运行
-- 时列——单事实源免列迁移）；到期惰性判定（触碰即转 expired）。租约行
-- 即事实（无事件流）；run_id 随 0.6.5 Run 对象增列（不落空壳）。
CREATE TABLE IF NOT EXISTS runtime_leases (
  lease_id        TEXT PRIMARY KEY,
  runtime_id      TEXT NOT NULL,
  environment_id  TEXT NOT NULL,
  workspace_ref   TEXT NOT NULL,
  mission_id      TEXT NOT NULL,
  generation      INTEGER NOT NULL,
  fence_token     TEXT NOT NULL,
  state           TEXT NOT NULL,
  resource_limits TEXT NOT NULL DEFAULT '{}',
  acquired_at     TEXT NOT NULL,
  expires_at      TEXT NOT NULL,
  released_at     TEXT NOT NULL DEFAULT '',
  updated_at      TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_runtime_leases_single_active_runtime
  ON runtime_leases(runtime_id) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_runtime_leases_single_active_env
  ON runtime_leases(environment_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_runtime_leases_runtime_gen ON runtime_leases(runtime_id, generation);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (17, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV18（ADR-0027 v0.2 审校 P2#2，0.6.4）：工作区别名排他——
// environments 增 workspace_key（规范工作区身份 = EvalSymlinks(Clean(dir))），
// 部分唯一索引按身份判重；租约工作区独占经 JOIN 以本键计（同一物理目录
// 的路径别名不得双注册/双租约）。空键 = 历史行（0.6.4 未发布期开发库）
// ——部分唯一索引放行，不参与新排他。ALTER 无 IF NOT EXISTS——schemaDDL
// 每次 Open 全量执行，故独立成块并容忍 duplicate column（v2 迁移同型）。
const migrationV18 = `
ALTER TABLE environments ADD COLUMN workspace_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_environments_workspace_key
  ON environments(workspace_key) WHERE workspace_key != '';
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (18, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV19（RFC-0018 §6/§10.2 / ADR-0028，0.6.5）：单 Agent Mission 执行。
// runs 为一次执行尝试（(mission_id, attempt) 唯一；每 Mission 至多一个
// 未终态行——部分唯一索引兜底服务层复核）；run_events 为执行流（含围栏
// 拒绝审计）。Run 事实独立成流（Mission 流只承载状态投影 running↔ready）。
const migrationV19 = `
CREATE TABLE IF NOT EXISTS runs (
  run_id       TEXT PRIMARY KEY,
  mission_id   TEXT NOT NULL,
  attempt      INTEGER NOT NULL,
  lease_id     TEXT NOT NULL,
  manifest_id  TEXT NOT NULL,
  runtime_id   TEXT NOT NULL,
  binding_id   TEXT NOT NULL,
  agent_id     TEXT NOT NULL,
  state        TEXT NOT NULL,
  error_text   TEXT NOT NULL DEFAULT '',
  result_block TEXT NOT NULL DEFAULT '',
  fence_token  TEXT NOT NULL,
  generation   INTEGER NOT NULL,
  requested_at TEXT NOT NULL,
  started_at   TEXT NOT NULL DEFAULT '',
  finished_at  TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL,
  UNIQUE (mission_id, attempt)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_single_unfinished ON runs(mission_id) WHERE state IN ('pending', 'running');
CREATE INDEX IF NOT EXISTS idx_runs_mission ON runs(mission_id, attempt);
CREATE TABLE IF NOT EXISTS run_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id      TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (19, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV20（RFC-0018 §3.2/§11.3 / ADR-0030，0.6.7）：Run 崩溃恢复——
// runs 处置三列（unknown 的核验结论/备注/时刻）+ missions.waiting_reason（等待
// 原因，waiting 态一等事实）。逐语句执行、重复加列容忍（新库经 v19 建 runs
// 后再加列；旧库直接加列；再次打开全部 duplicate column 容忍）。
var migrationV20 = []string{
	"ALTER TABLE runs ADD COLUMN resolution TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE runs ADD COLUMN resolution_note TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE runs ADD COLUMN resolved_at TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE missions ADD COLUMN waiting_reason TEXT NOT NULL DEFAULT ''",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (20, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV21（RFC-0018 §3.3/§10.2 / ADR-0031，0.6.8）：DeliveryPackage 成果提交
// ——append-only 交付包行（溯源 Run/代次/契约版本 + RFC §10.2 最小结构 JSON 列）。
// 幂等 DDL。
const migrationV21 = `
CREATE TABLE IF NOT EXISTS delivery_packages (
  package_id            TEXT PRIMARY KEY,
  mission_id            TEXT NOT NULL,
  run_id                TEXT NOT NULL,
  attempt               INTEGER NOT NULL,
  generation            INTEGER NOT NULL,
  contract_version      INTEGER NOT NULL,
  artifact_refs         TEXT NOT NULL DEFAULT '[]',
  acceptance_mapping    TEXT NOT NULL DEFAULT '[]',
  verification_evidence TEXT NOT NULL DEFAULT '',
  open_items            TEXT NOT NULL DEFAULT '[]',
  risks                 TEXT NOT NULL DEFAULT '[]',
  action_receipts       TEXT NOT NULL DEFAULT '[]',
  suggested_memory      TEXT NOT NULL DEFAULT '[]',
  notes                 TEXT NOT NULL DEFAULT '',
  submitted_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_delivery_mission ON delivery_packages(mission_id, submitted_at);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (21, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV22（RFC-0018 §10.2 / ADR-0032，0.6.9）：Mission 验收——missions 增
// review_package_id（受审/已验收包锚，AT-10 任务卡→产物→Run 互追踪）。逐语句
// 执行、重复加列容忍。
var migrationV22 = []string{
	"ALTER TABLE missions ADD COLUMN review_package_id TEXT NOT NULL DEFAULT ''",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (22, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV23（0.6.9 审校 #3 / ADR-0032 v0.2）：missions 增 review_round（送验
// 轮次——每次 request_review 单调递增，裁定须核对轮次，防旧裁定落到下一轮）。
// 逐语句执行、重复加列容忍。
var migrationV23 = []string{
	"ALTER TABLE missions ADD COLUMN review_round INTEGER NOT NULL DEFAULT 0",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (23, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV30（0.6.13 / ADR-0036）：墙钟强制——runs 增 deadline（进入 running
// 时按执行开始契约快照；空 = 无墙钟，历史行兼容）。逐语句加列 + 登记。
var migrationV30 = []string{
	"ALTER TABLE runs ADD COLUMN deadline TEXT NOT NULL DEFAULT ''",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (30, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV31（0.6.13 审校 #1/#2/#3/#16）：职责派发意图与续接回执——
// missions += slot_instance_id（slot 寻址派发的创建事实，执行装配按实例固定
// 任职）；dispatch_decisions += resolved_agent_id/resolved_instance_id（解析
// 事实另存——原请求摘要稳定，#10）；dispatch_receipts += reason（续接说明
// 随回执持久化，#16）。逐语句加列（重复容忍）+ 登记。
var migrationV31 = []string{
	"ALTER TABLE missions ADD COLUMN slot_instance_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE dispatch_decisions ADD COLUMN resolved_agent_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE dispatch_decisions ADD COLUMN resolved_instance_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE dispatch_receipts ADD COLUMN reason TEXT NOT NULL DEFAULT ''",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (31, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV32（0.7.0 / ADR-0037）：分域记忆——memory_items 表（§7.2 拟定字段
// 一次带全：策展/召回/Bundle 字段列预留，0.7.0 写入面只开放稳定子集）。
// 幂等 DDL。
const migrationV32 = `
CREATE TABLE IF NOT EXISTS memory_items (
  memory_id        TEXT PRIMARY KEY,
  version          INTEGER NOT NULL DEFAULT 1,
  namespace        TEXT NOT NULL,
  scope_id         TEXT NOT NULL,
  kind             TEXT NOT NULL,
  title            TEXT NOT NULL,
  content          TEXT NOT NULL,
  source_refs      TEXT NOT NULL DEFAULT '[]',
  observed_at      TEXT NOT NULL,
  valid_from       TEXT NOT NULL DEFAULT '',
  valid_to         TEXT NOT NULL DEFAULT '',
  confidence       REAL NOT NULL DEFAULT 1.0,
  status           TEXT NOT NULL DEFAULT 'active',
  visibility       TEXT NOT NULL DEFAULT 'namespace',
  sensitivity      TEXT NOT NULL DEFAULT 'normal',
  supersedes       TEXT NOT NULL DEFAULT '[]',
  derived_from     TEXT NOT NULL DEFAULT '',
  edit_history     TEXT NOT NULL DEFAULT '[]',
  extractor_version TEXT NOT NULL DEFAULT 'manual',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_items(namespace, scope_id, created_at);
CREATE INDEX IF NOT EXISTS idx_memory_status ON memory_items(namespace, status);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (32, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV29（0.6.12 / ADR-0035）：责任交接——missions 增 pending_owner_agent_id
// （在途交接承接人；空 = 无在途交接）。逐语句加列（重复容忍）+ 登记。
var migrationV29 = []string{
	"ALTER TABLE missions ADD COLUMN pending_owner_agent_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE missions ADD COLUMN pending_offer_id TEXT NOT NULL DEFAULT ''",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (29, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV24（0.6.9 复审 R2/R9 / ADR-0032 v0.2）：v23 升级残留数据补正——
// 在审/已验收任务的轮次 0 补为 1（否则裁定 0 被拒、1 与库存 0 冲突——卡死）；
// v22 已取消行的受审包引用清空（与新取消同口径；在审与已接受的包及事件保留）。
// 纯 UPDATE 天然幂等，重复执行安全。
const migrationV24 = `
UPDATE missions SET review_round = 1
 WHERE status IN ('reviewing', 'accepted') AND review_round = 0;
UPDATE missions SET review_package_id = ''
 WHERE status IN ('canceling', 'canceled') AND review_package_id != '';
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (24, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// roomFTSDDL v3（M3-3 按需平面）：FTS5 trigram 全文索引（v1.46 spike 实证
// modernc v1.57.0 / SQLite 3.53.3：unicode61 对中文整串成单 token 不可用，
// trigram 对 CJK 子串 ≥3 字与英文均正确命中；<3 字查询由 SearchMessages
// 回退 LIKE 子串）。虚拟表 CREATE 不在 schemaDDL 事务语义里幂等问题：
// IF NOT EXISTS 即幂等；存量库由 Open 的 v3 迁移执行。
const roomFTSDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS room_fts USING fts5(
	room_id UNINDEXED,
	event_id UNINDEXED,
	actor UNINDEXED,
	actor_kind UNINDEXED,
	thread_id UNINDEXED,
	occurred_at UNINDEXED,
	seq UNINDEXED,
	global_pos UNINDEXED,
	body,
	tokenize='trigram'
);
`

// docFTSDDL v4（RFC-0014 §2.8 文档全文搜索）：FTS5 trigram 派生索引（标题+正文；
// 与 room_fts 同 tokenizer 裁定——trigram 对 CJK 子串 ≥3 字与英文均正确命中，
// <3 字查询由 SearchDocs 回退 LIKE 子串）。doc.created 索标题；doc.revision_committed
// 索本批 ops 文本（增量派生，索引缺行由 SearchDocs 的自愈校验兜底重建）。
const docFTSDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS doc_fts USING fts5(
	doc_id UNINDEXED,
	event_id UNINDEXED,
	version UNINDEXED,
	actor UNINDEXED,
	occurred_at UNINDEXED,
	title,
	body,
	tokenize='trigram'
);
`

// migrationV2 既有库补 engine_claims.position 列（四轮复审 #14：恢复重驱动需按
// 持久顺序）。CREATE TABLE 只对新建库生效；ALTER 幂等（duplicate column 忽略）。
const migrationV2 = `ALTER TABLE engine_claims ADD COLUMN position INTEGER NOT NULL DEFAULT 0`

// Store 持有单个 SQLite 文件的连接池。个人版单进程内使用。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）path 处的数据库，启用 WAL 与 busy 等待，确保 schema 就绪。
// per-connection pragma 必须走 DSN：database/sql 连接池里每条连接都要生效，
// 用 db.Exec 设置只会命中当时那条连接（spike 实测并发下其余连接立刻 SQLITE_BUSY）。
func Open(path string) (*Store, error) {
	dsn := "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F") +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// 个人版：进程内并发写由 BEGIN IMMEDIATE + busy_timeout 串行化；
	// 多连接读写在 WAL 下安全。写竞争激烈时可收敛为 1（机制不变）。
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ensure schema: %w", err)
	}
	// v2 迁移（四轮复审 #14）：既有库补 engine_claims.position；duplicate column 忽略
	if _, err := db.Exec(migrationV2); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v2: %w", err)
	}
	// v3 迁移（M3-3）：FTS5 trigram 全文索引（存量库建虚拟表；新库由同 DDL 幂等）
	if _, err := db.Exec(roomFTSDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v3 (room_fts): %w", err)
	}
	// v4 迁移（RFC-0014 / ADR-0014）：doc_fts 全文索引（doc 事件表在 schemaDDL 内幂等）
	if _, err := db.Exec(docFTSDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v4 (doc_fts): %w", err)
	}
	// v18 迁移（ADR-0027 v0.2 / 0.6.4 审校 P2#2）：工作区别名排他——
	// workspace_key 列 + 身份部分唯一索引；重复加列（新库已建）容忍。
	if _, err := db.Exec(migrationV18); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v18: %w", err)
	}
	// v19 迁移（ADR-0028 / 0.6.5）：runs/run_events（幂等 DDL）
	if _, err := db.Exec(migrationV19); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v19: %w", err)
	}
	// v20 迁移（ADR-0030 / 0.6.7）：runs 处置三列 + missions.waiting_reason——
	// 逐语句执行，重复加列容忍（多语句单 Exec 会在首个 duplicate 处中止）。
	for _, stmt := range migrationV20 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v20: %w", err)
		}
	}
	// v21 迁移（ADR-0031 / 0.6.8）：delivery_packages（幂等 DDL）
	if _, err := db.Exec(migrationV21); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v21: %w", err)
	}
	// v22 迁移（ADR-0032 / 0.6.9）：missions.review_package_id——逐语句执行，
	// 重复加列容忍。
	for _, stmt := range migrationV22 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v22: %w", err)
		}
	}
	// v23 迁移（0.6.9 审校 #3）：missions.review_round
	for _, stmt := range migrationV23 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v23: %w", err)
		}
	}
	// v24 迁移（0.6.9 复审 R2/R9）：升级残留补正（幂等 UPDATE）
	if _, err := db.Exec(migrationV24); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v24: %w", err)
	}
	// v25 迁移（ADR-0033 / 0.6.10）：回房通知——逐语句执行，重复加列容忍
	for _, stmt := range migrationV25 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v25: %w", err)
		}
	}
	// v26 迁移（0.6.10 审校 #8）：升级前已删房的来源墓碑回填（幂等 UPDATE，
	// v24 先例——按 room_tombstones 原删除时间补 mission_sources.deleted_at）
	if _, err := db.Exec(migrationV26); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v26: %w", err)
	}
	// v27 迁移（0.6.10 四审·既有补正）：回房投影游标重置——老式 to=returned
	// 退回事件此前不识别且游标已越过；重扫 INSERT OR IGNORE 幂等补投
	if _, err := db.Exec(migrationV27); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v27: %w", err)
	}
	// v28 迁移（0.6.11 / ADR-0034）：重开通知快照列——逐语句，重复加列容忍
	for _, stmt := range migrationV28 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v28: %w", err)
		}
	}
	// v29 迁移（0.6.12 / ADR-0035）：missions.pending_owner_agent_id——逐语句，重复加列容忍
	for _, stmt := range migrationV29 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v29: %w", err)
		}
	}
	// v30 迁移（0.6.13 / ADR-0036）：runs.deadline——逐语句，重复加列容忍
	for _, stmt := range migrationV30 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v30: %w", err)
		}
	}
	// v31 迁移（0.6.13 审校）：职责派发意图/解析事实/回执说明——逐语句，重复加列容忍
	for _, stmt := range migrationV31 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("sqlite: migration v31: %w", err)
		}
	}
	// v32 迁移（0.7.0 / ADR-0037）：memory_items——幂等 DDL
	if _, err := db.Exec(migrationV32); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v32: %w", err)
	}
	// 二轮审校 #19：DB 文件 owner-only（目录 0700 之外的兜底；WAL/SHM 由目录权限覆盖）
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("sqlite: chmod db: %w", err)
	}
	// 复审 #17：既有目录权限补收紧（MkdirAll 只在新建时生效；WAL/SHM 也在该目录）。
	// best-effort：失败不阻断打开（只读介质等场景）；Windows 无 POSIX 权限语义，
	// 依赖用户目录 ACL（platform-notes W-6）。
	_ = os.Chmod(filepath.Dir(path), 0o700)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// JournalMode 暴露当前日志模式（spike 断言 WAL 用）。
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}

// BackupTo 生成一致快照到 destPath（M4-0 备份面：VACUUM INTO——SQLite 官方
// 备份路径，无需停写、不搬 WAL/SHM，产物自包含单文件）。destPath 不得已存在
// （VACUUM INTO 语义：已存在即报错——由调用方保证新目录）。
func (s *Store) BackupTo(ctx context.Context, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("sqlite: backup dest 已存在: %s", destPath)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("sqlite: vacuum into: %w", err)
	}
	return os.Chmod(destPath, 0o600)
}

// AppendEvents 在单个 BEGIN IMMEDIATE 事务内追加一批事件并同事务写 outbox：
// seq 由存储按房间分配（调用方不指定）；任一 event_id 重复则整批回滚。
// 返回落库后的信封（含分配的 seq），顺序与入参一致。
func (s *Store) AppendEvents(ctx context.Context, envelopes []protocol.Envelope) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, nil, nil)
}

// AppendWithReceipt 实现 room.AtomicStore：事件 + 幂等回执同事务原子落库；
// 事件 ID 或回执键冲突（并发同命令竞态的后到者）返回 room.ErrDuplicateReceipt，整批回滚。
func (s *Store) AppendWithReceipt(ctx context.Context, envelopes []protocol.Envelope, receipt room.CommandReceipt) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, &receipt, nil)
}

// AppendEventsIf 实现 room.CASStore（四轮复审 #12）：当前房间版本 != expected 即
// ErrVersionConflict（BEGIN IMMEDIATE 临界区内判定），整批回滚。
func (s *Store) AppendEventsIf(ctx context.Context, envelopes []protocol.Envelope, expectedRoomVersion int64) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, nil, &expectedRoomVersion)
}

// appendTx 共享事务体：BEGIN IMMEDIATE → 分配 seq → 写 room_events + outbox（+ 可选回执）→ COMMIT。
func (s *Store) appendTx(ctx context.Context, envelopes []protocol.Envelope, receipt *room.CommandReceipt, expectedVersion *int64) ([]protocol.Envelope, error) {
	if len(envelopes) == 0 && receipt == nil {
		return nil, nil
	}
	roomID := ""
	if len(envelopes) > 0 {
		roomID = envelopes[0].RoomID
		for i := range envelopes {
			if envelopes[i].RoomID != roomID {
				return nil, fmt.Errorf("sqlite: 一批事件必须同 room（%s vs %s）", roomID, envelopes[i].RoomID)
			}
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var maxSeq int64
	if roomID != "" {
		if err := conn.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", roomID,
		).Scan(&maxSeq); err != nil {
			return nil, fmt.Errorf("sqlite: read max seq: %w", err)
		}
	}

	// CAS 期望版本（四轮复审 #12）：与回执的乐观并发同在 BEGIN IMMEDIATE 临界区内判定
	if expectedVersion != nil && roomID != "" && *expectedVersion != maxSeq {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			room.ErrVersionConflict, *expectedVersion, maxSeq)
	}

	if receipt != nil && roomID != "" {
		// 回执键先查（先于版本校验）：预检与提交之间的竞态重放优先于版本冲突
		var receiptExists bool
		if err := conn.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM command_receipts
			WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind,
		).Scan(&receiptExists); err != nil {
			return nil, fmt.Errorf("sqlite: check receipt: %w", err)
		}
		if receiptExists {
			return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, receipt.IdempotencyKey)
		}
		// 乐观并发在 BEGIN IMMEDIATE 临界区内强制（P-03：冲突判定在提交事务内）
		if receipt.ExpectedRoomVersion != maxSeq {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				room.ErrVersionConflict, receipt.ExpectedRoomVersion, maxSeq)
		}
	}

	appended := make([]protocol.Envelope, len(envelopes))
	for i := range envelopes {
		env := envelopes[i]
		maxSeq++
		env.Seq = maxSeq
		// ADR-0033：信封落库（room_events + outbox + FTS）抽为共享路径，
		// 回房通知的原子投递（ReturnStore.Deliver）与本路径同一事务语义。
		if _, err := insertEnvelopeOnConn(ctx, conn, env); err != nil {
			if isUniqueViolation(err) {
				if receipt != nil {
					// 回执式追加中事件撞车 = 并发同命令竞态后到者
					return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, env.EventID)
				}
				return nil, err // 已是 ErrDuplicateEvent 包装
			}
			return nil, err
		}
		appended[i] = env
	}

	if receipt != nil {
		// RoomVersion 权威回填：追加后的最新 seq（调用方传入值不信任，D3 修复）
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO command_receipts
			(tenant_id, idempotency_key, command_kind, room_id, request_fingerprint, event_id, room_version, executed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind, receipt.RoomID,
			receipt.RequestFingerprint, receipt.EventID, maxSeq, receipt.ExecutedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, receipt.IdempotencyKey)
			}
			return nil, fmt.Errorf("sqlite: insert receipt: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return appended, nil
}

// LookupReceipt 实现 room.AtomicStore：未命中返回 (nil, nil)。
func (s *Store) LookupReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*room.CommandReceipt, error) {
	rc := &room.CommandReceipt{}
	err := s.db.QueryRowContext(ctx, `
		SELECT room_id, request_fingerprint, event_id, room_version, executed_at
		FROM command_receipts
		WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?`,
		tenantID, idempotencyKey, commandKind,
	).Scan(&rc.RoomID, &rc.RequestFingerprint, &rc.EventID, &rc.RoomVersion, &rc.ExecutedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: lookup receipt: %w", err)
	}
	rc.TenantID, rc.IdempotencyKey, rc.CommandKind = tenantID, idempotencyKey, commandKind
	return rc, nil
}

// RoomVersion 实现 room.AtomicStore：房间当前版本（最新 seq；空房 0）。
func (s *Store) RoomVersion(ctx context.Context, roomID string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", roomID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: room version: %w", err)
	}
	return v, nil
}

// RoomExists 实现 room.AtomicStore：是否已见 room.created。
func (s *Store) RoomExists(ctx context.Context, roomID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM room_events WHERE room_id = ? AND type = ?)",
		roomID, protocol.EventRoomCreated).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: room exists: %w", err)
	}
	return exists, nil
}

// ListRooms 实现 room.RoomLister：单查询聚合（个人版房间/事件规模，相关子查询足够）。
// display_name 取序内最新 room.created/room.renamed 载荷；paused = 最新生命周期事件为
// room.paused；message_count 计 message.posted。按 last_event_at 倒序（同刻 room_id
// 升序兜底，排序确定性——与 MemStore 实现同规则）。
// v1.72：agents 聚合与 room.RosterOf 同语义分层（侧栏私聊/群聊分型依据）——
// explicit = room.created.payload.agents（建房间时物化快照）+ participant.admitted
// 载荷；无 explicit 的旧房间 → 历史推导（agent actor 参与者 + intent.recorded/
// floor.granted 载荷目标）；两者皆无 → NULL = 全席（首轮兼容；引擎写库数据均为
// par_* 形态，SQL 不重复 participantIDPattern 校验）。
func (s *Store) ListRooms(ctx context.Context) ([]room.RoomSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH roster_rows AS (
			SELECT e.room_id AS room_id, je.value AS pid, 1 AS is_explicit
				FROM room_events e, json_each(COALESCE(json_extract(e.envelope, '$.payload.agents'), '[]')) je
				WHERE e.type = 'room.created'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.payload.participant_id'), 1
				FROM room_events e
				WHERE e.type = 'participant.admitted'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.actor.participant_id'), 0
				FROM room_events e
				WHERE json_extract(e.envelope, '$.actor.kind') = 'agent'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.payload.participant_id'), 0
				FROM room_events e
				WHERE e.type IN ('intent.recorded', 'floor.granted')
		),
		roster AS (
			SELECT room_id,
				CASE WHEN SUM(is_explicit) > 0
					THEN GROUP_CONCAT(CASE WHEN is_explicit = 1 THEN pid END)
					ELSE GROUP_CONCAT(pid)
				END AS agents
			FROM roster_rows
			GROUP BY room_id
		)
		SELECT
			r.room_id,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.display_name') FROM room_events e
				WHERE e.room_id = r.room_id AND e.type IN ('room.created', 'room.renamed')
				ORDER BY e.seq DESC LIMIT 1), '') AS display_name,
			(SELECT e.occurred_at FROM room_events e
				WHERE e.room_id = r.room_id AND e.type = 'room.created'
				ORDER BY e.seq ASC LIMIT 1) AS created_at,
			(SELECT e.occurred_at FROM room_events e
				WHERE e.room_id = r.room_id
				ORDER BY e.seq DESC LIMIT 1) AS last_event_at,
			COALESCE((SELECT e.type = 'room.paused' FROM room_events e
				WHERE e.room_id = r.room_id AND e.type IN ('room.paused', 'room.started')
				ORDER BY e.seq DESC LIMIT 1), 0) AS paused,
			(SELECT COUNT(*) FROM room_events e
				WHERE e.room_id = r.room_id AND e.type = 'message.posted') AS message_count,
			ro.agents
		FROM (SELECT DISTINCT room_id FROM room_events WHERE type = 'room.created') r
		LEFT JOIN roster ro ON ro.room_id = r.room_id
		ORDER BY last_event_at DESC, r.room_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list rooms: %w", err)
	}
	defer rows.Close()
	out := []room.RoomSummary{}
	for rows.Next() {
		var sum room.RoomSummary
		var agents sql.NullString
		if err := rows.Scan(&sum.RoomID, &sum.DisplayName, &sum.CreatedAt, &sum.LastEventAt,
			&sum.Paused, &sum.MessageCount, &agents); err != nil {
			return nil, fmt.Errorf("sqlite: scan room summary: %w", err)
		}
		if agents.Valid && agents.String != "" {
			// GROUP_CONCAT 无序：拆分后排序保输出确定性（与 MemStore 投影一致）。
			list := strings.Split(agents.String, ",")
			sort.Strings(list)
			sum.Agents = list
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: rows: %w", err)
	}
	return out, nil
}

// EventsAfter 实现 room.EventReader：从 cursor 之后按全局位续读某房间的事件
// （订阅续传与历史读共用）。limit ≤0 时取 100。返回下一游标；无更多事件时 next 为空串。
func (s *Store) EventsAfter(ctx context.Context, roomID, cursor string, limit int) (events []room.StoredEvent, next string, err error) {
	pos, err := protocol.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT global_pos, envelope FROM room_events
		WHERE room_id = ? AND global_pos > ?
		ORDER BY global_pos LIMIT ?`, roomID, pos, limit)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: query events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var globalPos int64
		var raw string
		if err := rows.Scan(&globalPos, &raw); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan event: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, "", fmt.Errorf("sqlite: unmarshal envelope %d: %w", globalPos, err)
		}
		events = append(events, room.StoredEvent{Envelope: env, Cursor: protocol.EncodeCursor(globalPos)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: rows: %w", err)
	}
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// OutboxEntry 待分发事件（outbox.Entry 的本地别名；进程内提交后分发，崩溃后重放）。
type OutboxEntry = outbox.Entry

// Pending 实现 outbox.Store：按提交序取未分发条目（崩溃恢复重放入口）。
func (s *Store) Pending(ctx context.Context, limit int) ([]outbox.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, room_id, event_id, global_pos, envelope FROM outbox
		WHERE dispatched_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query outbox: %w", err)
	}
	defer rows.Close()
	var entries []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		var raw string
		if err := rows.Scan(&e.ID, &e.RoomID, &e.EventID, &e.GlobalPos, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan outbox: %w", err)
		}
		e.Envelope = []byte(raw)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// MarkDispatched 幂等标记分发完成（进程内分发成功后调用）。
func (s *Store) MarkDispatched(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE outbox SET dispatched_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id IN ("+
			strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return fmt.Errorf("sqlite: mark dispatched: %w", err)
	}
	return nil
}

// isUniqueViolation 识别 modernc 驱动的 UNIQUE 约束错误（SQLSTATE 23 概念在 SQLite 中为 19/2067）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "constraint failed") &&
		(strings.Contains(msg, "UNIQUE") || strings.Contains(msg, "unique"))
}

// ---- ClaimStore：轮次交接声明（二轮审校 #9，room.ClaimStore 端口）----

// ClaimStimulus 实现 room.ClaimStore：INSERT OR IGNORE，true = 首次声明。
func (s *Store) ClaimStimulus(ctx context.Context, roomID, stimulusEventID string, envelope []byte, position int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO engine_claims (room_id, stimulus_event_id, envelope, position) VALUES (?, ?, ?, ?)`,
		roomID, stimulusEventID, string(envelope), position)
	if err != nil {
		return false, fmt.Errorf("sqlite: claim stimulus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: claim rows: %w", err)
	}
	return n > 0, nil
}

// DeleteClaim 实现 room.ClaimStore。
func (s *Store) DeleteClaim(ctx context.Context, roomID, stimulusEventID string) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM engine_claims WHERE room_id = ? AND stimulus_event_id = ?",
		roomID, stimulusEventID); err != nil {
		return fmt.Errorf("sqlite: delete claim: %w", err)
	}
	return nil
}

// PendingClaims 实现 room.ClaimStore：按持久位升序（四轮复审 #14——
// 恢复重驱动顺序 = 刺激到达顺序，无序查询会反转同房间人类消息顺序）。
func (s *Store) PendingClaims(ctx context.Context) ([]room.StimulusClaim, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT room_id, stimulus_event_id, envelope, position FROM engine_claims ORDER BY position ASC, room_id ASC")
	if err != nil {
		return nil, fmt.Errorf("sqlite: query claims: %w", err)
	}
	defer rows.Close()
	var out []room.StimulusClaim
	for rows.Next() {
		var c room.StimulusClaim
		var raw string
		if err := rows.Scan(&c.RoomID, &c.StimulusEventID, &raw, &c.Position); err != nil {
			return nil, fmt.Errorf("sqlite: scan claim: %w", err)
		}
		c.Envelope = []byte(raw)
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertReceipt 实现 room.ReceiptStore：上下文回执落库（可查可审计）。
func (s *Store) InsertReceipt(ctx context.Context, receipt contextx.Receipt) error {
	raw, err := json.Marshal(receipt.LayerDigests)
	if err != nil {
		return fmt.Errorf("sqlite: marshal digests: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO context_receipts (receipt_id, room_id, task_id, watermark, layer_digests, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(receipt_id) DO NOTHING`,
		receipt.ReceiptID, receipt.RoomID, receipt.TaskID, receipt.Watermark, string(raw), receipt.CreatedAt)
	if err != nil {
		return fmt.Errorf("sqlite: insert receipt: %w", err)
	}
	return nil
}

// ReceiptsOf 实现 room.ReceiptLister：回执流水（最新在前；v1.70 展示对齐——
// "模型实际看到了什么"自正式面可查，不再只进 -dev 调试面）。
func (s *Store) ReceiptsOf(ctx context.Context, roomID string, limit int) ([]contextx.Receipt, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT receipt_id, room_id, task_id, watermark, layer_digests, created_at
		FROM context_receipts WHERE room_id = ? ORDER BY created_at DESC, receipt_id DESC LIMIT ?`,
		roomID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query receipts: %w", err)
	}
	defer rows.Close()
	out := []contextx.Receipt{}
	for rows.Next() {
		var r contextx.Receipt
		var digests string
		if err := rows.Scan(&r.ReceiptID, &r.RoomID, &r.TaskID, &r.Watermark, &digests, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan receipt: %w", err)
		}
		_ = json.Unmarshal([]byte(digests), &r.LayerDigests)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoom 删除级联（M3-6，RFC-0010）：事务内清事件/outbox/回执/声明，并落
// 墓碑行——"曾存在"可审计、内容不可恢复。
func (s *Store) DeleteRoom(ctx context.Context, roomID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reason string
	_ = tx.QueryRowContext(ctx,
		"SELECT json_extract(envelope, '$.payload.reason') FROM room_events WHERE room_id=? AND type='room.deleted' LIMIT 1",
		roomID).Scan(&reason)
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO room_tombstones(room_id, reason, deleted_at) VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		roomID, reason); err != nil {
		return err
	}
	for _, table := range []string{"room_events", "outbox", "command_receipts", "engine_claims", "room_fts"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE room_id=?", roomID); err != nil {
			return err
		}
	}
	// ADR-0033 / 0.6.10 回房通知级联（同事务）：来源墓碑（mission_sources 保留映射，
	// deleted_at 即墓碑——结果与映射不删）；该房待投/持有通知取消（返回目标失效，
	// 已 delivered 的卡随房间事件一并清除——房间账本已物理删除）。
	if _, err := tx.ExecContext(ctx, `
		UPDATE mission_sources SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE room_id = ? AND deleted_at IS NULL`, roomID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mission_return_notices
		SET status = 'canceled', last_error = 'room_deleted',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE room_id = ? AND status IN ('pending', 'held')`, roomID); err != nil {
		return err
	}
	return tx.Commit()
}

// ftsFieldsOf message.posted 信封 → 索引字段（正文/作者/线程）。
func ftsFieldsOf(env protocol.Envelope) (body, actor, actorKind, threadID string, ok bool) {
	var p struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return "", "", "", "", false
	}
	if env.ThreadID != nil {
		threadID = *env.ThreadID
	}
	return p.Body, env.Actor.ParticipantID, env.Actor.Kind, threadID, true
}

// SearchMessages 实现 room.MessageSearcher（M3-3 按需平面）：FTS5 trigram 生产
// 路径。语义与线性基准 room.SearchMessages 一致：子串、大小写不敏感、最新在前、
// limit 1..100 默认 20。≥3 字查询走 MATCH（phrase 引号包裹防语法注入；trigram
// 对 CJK/英文子串均正确——v1.46 spike 实证）；<3 字 trigram 不可用，回退 LIKE
// 子串。自愈：索引行数与事件数不符（旧库升级/历史缺行）即全量重建该房间索引。
func (s *Store) SearchMessages(ctx context.Context, roomID, query, actor, threadID string, limit int) ([]room.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []room.SearchHit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if err := s.ensureFTS(ctx, roomID); err != nil {
		return nil, err
	}
	where := "room_id = ?"
	args := []any{roomID}
	if actor != "" {
		where += " AND actor = ?"
		args = append(args, actor)
	}
	if threadID != "" {
		where += " AND thread_id = ?"
		args = append(args, threadID)
	}
	// trigram tokenizer 要求查询 ≥3 字符（CJK 与 ASCII 同规）；短查询回退 LIKE
	// 子串（语义与线性基准一致——同为大小写不敏感子串）。
	if len([]rune(query)) >= 3 {
		phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		where += " AND room_fts MATCH ?"
		args = append(args, phrase)
	} else {
		where += " AND body LIKE ? COLLATE NOCASE"
		args = append(args, "%"+query+"%")
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, actor, actor_kind, thread_id, body, occurred_at, global_pos
		FROM room_fts WHERE `+where+`
		ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	defer rows.Close()
	hits := []room.SearchHit{}
	for rows.Next() {
		var h room.SearchHit
		var threadID string
		var globalPos int64
		if err := rows.Scan(&h.EventID, &h.Actor, &h.ActorKind, &threadID, &h.Body, &h.OccurredAt, &globalPos); err != nil {
			return nil, fmt.Errorf("sqlite: scan search: %w", err)
		}
		if threadID != "" {
			t := threadID
			h.ThreadID = &t
		}
		h.Position = protocol.EncodeCursor(globalPos)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ensureFTS 索引自愈校验：FTS 行数 != 事件数（v3 前旧库/异常缺口）即全量重建
// 该房间索引（个人版房间规模毫秒级；重建后增量路径恢复）。
func (s *Store) ensureFTS(ctx context.Context, roomID string) error {
	var ftsCount, evtCount int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM room_fts WHERE room_id = ?", roomID).Scan(&ftsCount); err != nil {
		return fmt.Errorf("sqlite: fts count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM room_events WHERE room_id = ? AND type = ?", roomID, protocol.EventMessagePosted,
	).Scan(&evtCount); err != nil {
		return fmt.Errorf("sqlite: event count: %w", err)
	}
	if ftsCount == evtCount {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: fts rebuild begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM room_fts WHERE room_id = ?", roomID); err != nil {
		return fmt.Errorf("sqlite: fts rebuild clear: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, envelope, global_pos FROM room_events
		WHERE room_id = ? AND type = ? ORDER BY seq`, roomID, protocol.EventMessagePosted)
	if err != nil {
		return fmt.Errorf("sqlite: fts rebuild scan: %w", err)
	}
	type row struct {
		id, env string
		pos     int64
	}
	var batch []row
	for rows.Next() {
		var r row
		var raw string
		if err := rows.Scan(&r.id, &raw, &r.pos); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: fts rebuild row: %w", err)
		}
		r.env = raw
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: fts rebuild rows: %w", err)
	}
	for _, r := range batch {
		var env protocol.Envelope
		if json.Unmarshal([]byte(r.env), &env) != nil {
			continue
		}
		body, actor, actorKind, threadID, ok := ftsFieldsOf(env)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO room_fts (room_id, event_id, actor, actor_kind, thread_id, occurred_at, seq, global_pos, body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			roomID, env.EventID, actor, actorKind, threadID, env.OccurredAt, env.Seq, r.pos, body); err != nil {
			return fmt.Errorf("sqlite: fts rebuild insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: fts rebuild commit: %w", err)
	}
	return nil
}

// ---- DocStore：per-doc 事件流（RFC-0014 / ADR-0014，appendTx 先例平移）----

// AppendDocEvents 实现 doc.DocStore：version 由存储按文档分配（调用方不指定）；
// 任一 event_id 重复则整批回滚。返回落库后的信封（含分配的 version），顺序与入参一致。
func (s *Store) AppendDocEvents(ctx context.Context, envelopes []protocol.DocEnvelope) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, nil, nil)
}

// AppendDocEventsWithReceipt 实现 doc.DocStore：事件 + 幂等回执同事务原子落库；
// 事件 ID 或回执键冲突（并发同命令竞态的后到者）返回 doc.ErrDuplicateReceipt，整批回滚。
func (s *Store) AppendDocEventsWithReceipt(ctx context.Context, envelopes []protocol.DocEnvelope, receipt doc.CommandReceipt) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, &receipt, nil)
}

// AppendDocEventsIf 实现 doc.DocCASStore：当前文档版本 != expected 即
// doc.ErrVersionConflict（BEGIN IMMEDIATE 临界区内判定），整批回滚。
func (s *Store) AppendDocEventsIf(ctx context.Context, envelopes []protocol.DocEnvelope, expectedDocVersion int64) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, nil, &expectedDocVersion)
}

// appendDocTx 共享事务体：BEGIN IMMEDIATE → 分配 version → 写 doc_events +
// doc_outbox（+ doc.created/doc.revision_committed 文本入 doc_fts；+ 可选回执）→ COMMIT。
func (s *Store) appendDocTx(ctx context.Context, envelopes []protocol.DocEnvelope, receipt *doc.CommandReceipt, expectedVersion *int64) ([]protocol.DocEnvelope, error) {
	if len(envelopes) == 0 && receipt == nil {
		return nil, nil
	}
	docID := ""
	if len(envelopes) > 0 {
		docID = envelopes[0].DocID
		for i := range envelopes {
			if envelopes[i].DocID != docID {
				return nil, fmt.Errorf("sqlite: 一批事件必须同 doc（%s vs %s）", docID, envelopes[i].DocID)
			}
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var maxVersion int64
	if docID != "" {
		if err := conn.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(version), 0) FROM doc_events WHERE doc_id = ?", docID,
		).Scan(&maxVersion); err != nil {
			return nil, fmt.Errorf("sqlite: read max version: %w", err)
		}
	}

	// CAS 期望版本（base_version）：与回执的乐观并发同在 BEGIN IMMEDIATE 临界区内判定
	if expectedVersion != nil && docID != "" && *expectedVersion != maxVersion {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			doc.ErrVersionConflict, *expectedVersion, maxVersion)
	}

	if receipt != nil && docID != "" {
		// 回执键先查（先于版本校验）：预检与提交之间的竞态重放优先于版本冲突
		var receiptExists bool
		if err := conn.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM doc_command_receipts
			WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind,
		).Scan(&receiptExists); err != nil {
			return nil, fmt.Errorf("sqlite: check receipt: %w", err)
		}
		if receiptExists {
			return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, receipt.IdempotencyKey)
		}
		// 乐观并发在 BEGIN IMMEDIATE 临界区内强制（P-03：冲突判定在提交事务内）
		if receipt.ExpectedDocVersion != maxVersion {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				doc.ErrVersionConflict, receipt.ExpectedDocVersion, maxVersion)
		}
	}

	appended := make([]protocol.DocEnvelope, len(envelopes))
	for i := range envelopes {
		env := envelopes[i]
		maxVersion++
		env.Version = maxVersion
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("sqlite: marshal doc envelope %s: %w", env.EventID, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_events (event_id, tenant_id, doc_id, version, type, schema_version, occurred_at, envelope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			env.EventID, env.TenantID, env.DocID, env.Version, env.Type, env.SchemaVersion, env.OccurredAt, string(raw),
		); err != nil {
			if isUniqueViolation(err) {
				if receipt != nil {
					// 回执式追加中事件撞车 = 并发同命令竞态后到者
					return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, env.EventID)
				}
				return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateEvent, env.EventID)
			}
			return nil, fmt.Errorf("sqlite: insert doc event: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_outbox (doc_id, version, event_id, envelope) VALUES (?, ?, ?, ?)`,
			env.DocID, env.Version, env.EventID, string(raw),
		); err != nil {
			return nil, fmt.Errorf("sqlite: insert doc outbox: %w", err)
		}
		// 全文索引（RFC-0014 §2.8：标题+正文派生）：doc.created 索标题、
		// doc.revision_committed 索本批 ops 文本，与事件账本同事务——
		// 索引缺行由 SearchDocs 的自愈校验兜底重建。
		if env.Type == protocol.EventDocCreated || env.Type == protocol.EventDocRevisionCommitted {
			if title, body, ok := docFTSFieldsOf(env); ok {
				if _, err := conn.ExecContext(ctx, `
					INSERT INTO doc_fts (doc_id, event_id, version, actor, occurred_at, title, body)
					VALUES (?, ?, ?, ?, ?, ?, ?)`,
					env.DocID, env.EventID, env.Version, env.Actor.ParticipantID, env.OccurredAt, title, body,
				); err != nil {
					return nil, fmt.Errorf("sqlite: insert doc fts: %w", err)
				}
			}
		}
		appended[i] = env
	}

	if receipt != nil {
		// DocVersion 权威回填：追加后的最新 version（调用方传入值不信任）
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_command_receipts
			(tenant_id, idempotency_key, command_kind, doc_id, request_fingerprint, event_id, doc_version, executed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind, receipt.DocID,
			receipt.RequestFingerprint, receipt.EventID, maxVersion, receipt.ExecutedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, receipt.IdempotencyKey)
			}
			return nil, fmt.Errorf("sqlite: insert doc receipt: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return appended, nil
}

// LookupDocReceipt 实现 doc.DocStore：未命中返回 (nil, nil)。
func (s *Store) LookupDocReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*doc.CommandReceipt, error) {
	rc := &doc.CommandReceipt{}
	err := s.db.QueryRowContext(ctx, `
		SELECT doc_id, request_fingerprint, event_id, doc_version, executed_at
		FROM doc_command_receipts
		WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?`,
		tenantID, idempotencyKey, commandKind,
	).Scan(&rc.DocID, &rc.RequestFingerprint, &rc.EventID, &rc.DocVersion, &rc.ExecutedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: lookup doc receipt: %w", err)
	}
	rc.TenantID, rc.IdempotencyKey, rc.CommandKind = tenantID, idempotencyKey, commandKind
	return rc, nil
}

// DocVersion 实现 doc.DocStore：文档当前版本（最新 version；未见事件为 0）。
func (s *Store) DocVersion(ctx context.Context, docID string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM doc_events WHERE doc_id = ?", docID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: doc version: %w", err)
	}
	return v, nil
}

// DocExists 实现 doc.DocStore：是否已见 doc.created。
func (s *Store) DocExists(ctx context.Context, docID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM doc_events WHERE doc_id = ? AND type = ?)",
		docID, protocol.EventDocCreated).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: doc exists: %w", err)
	}
	return exists, nil
}

// DocEventsAfter 实现 doc.DocEventReader：从 cursor 之后按 version 续读某文档的事件
// （doc:{id} 频道续传与历史读共用——version 即 SSE cursor，ADR-0014）。
// limit ≤0 时取 100。返回下一游标；无更多事件时 next 为空串。
func (s *Store) DocEventsAfter(ctx context.Context, docID, cursor string, limit int) (events []doc.StoredDocEvent, next string, err error) {
	pos, err := protocol.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, envelope FROM doc_events
		WHERE doc_id = ? AND version > ?
		ORDER BY version LIMIT ?`, docID, pos, limit)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: query doc events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var raw string
		if err := rows.Scan(&version, &raw); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan doc event: %w", err)
		}
		var env protocol.DocEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, "", fmt.Errorf("sqlite: unmarshal doc envelope %d: %w", version, err)
		}
		events = append(events, doc.StoredDocEvent{Envelope: env, Cursor: protocol.EncodeCursor(version)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: rows: %w", err)
	}
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// DeleteDoc 实现 doc.DocStore 删除级联（DeleteRoom 先例平移）：事务内清
// 事件/outbox/回执/FTS 索引，并落墓碑行——曾存在可审计、内容不可恢复。
func (s *Store) DeleteDoc(ctx context.Context, docID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reason string
	_ = tx.QueryRowContext(ctx,
		"SELECT json_extract(envelope, '$.payload.reason') FROM doc_events WHERE doc_id=? AND type='doc.deleted' LIMIT 1",
		docID).Scan(&reason)
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO doc_tombstones(doc_id, reason, deleted_at) VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		docID, reason); err != nil {
		return err
	}
	for _, table := range []string{"doc_events", "doc_outbox", "doc_command_receipts", "doc_fts"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE doc_id=?", docID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingDoc doc_outbox 待分发条目（房间 Pending 先例平移：按提交序取未分发）。
// Entry 结构复用：RoomID 字段承载 doc_id、GlobalPos 承载 doc version（version 即
// doc:{id} 频道 SSE cursor——分发消费者据此编码 position）。
func (s *Store) PendingDoc(ctx context.Context, limit int) ([]outbox.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, doc_id, event_id, version, envelope FROM doc_outbox
		WHERE dispatched_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query doc outbox: %w", err)
	}
	defer rows.Close()
	var entries []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		var raw string
		if err := rows.Scan(&e.ID, &e.RoomID, &e.EventID, &e.GlobalPos, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc outbox: %w", err)
		}
		e.Envelope = []byte(raw)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// MarkDocDispatched doc_outbox 条目分发完成标记（幂等，房间 MarkDispatched 先例）。
func (s *Store) MarkDocDispatched(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE doc_outbox SET dispatched_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id IN ("+
			strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return fmt.Errorf("sqlite: mark doc dispatched: %w", err)
	}
	return nil
}

// DocOutboxStore doc_outbox → outbox.Store 端口适配（doc 分发器接线用；
// 条目字段语义见 PendingDoc 注记）。
type DocOutboxStore struct{ Store *Store }

// Pending 实现 outbox.Store。
func (a DocOutboxStore) Pending(ctx context.Context, limit int) ([]outbox.Entry, error) {
	return a.Store.PendingDoc(ctx, limit)
}

// MarkDispatched 实现 outbox.Store。
func (a DocOutboxStore) MarkDispatched(ctx context.Context, ids []int64) error {
	return a.Store.MarkDocDispatched(ctx, ids)
}

// ListExternalShares doc_external_shares 查询（extdocs.ShareStore 端口；
// docID/provider 空串 = 全量该维度，按 shared_at 倒序）。
func (s *Store) ListExternalShares(ctx context.Context, docID, provider string) ([]extdocs.Share, error) {
	q := "SELECT doc_id, provider, external_url, shared_by, shared_at FROM doc_external_shares"
	var conds []string
	var args []any
	if docID != "" {
		conds = append(conds, "doc_id=?")
		args = append(args, docID)
	}
	if provider != "" {
		conds = append(conds, "provider=?")
		args = append(args, provider)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY shared_at DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list external shares: %w", err)
	}
	defer rows.Close()
	var out []extdocs.Share
	for rows.Next() {
		var sh extdocs.Share
		if err := rows.Scan(&sh.DocID, &sh.Provider, &sh.ExternalURL, &sh.SharedBy, &sh.SharedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan external share: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// GetExternalShare 单条映射（不存在 = extdocs.ErrShareNotFound）。
func (s *Store) GetExternalShare(ctx context.Context, docID, provider string) (extdocs.Share, error) {
	var sh extdocs.Share
	err := s.db.QueryRowContext(ctx,
		"SELECT doc_id, provider, external_url, shared_by, shared_at FROM doc_external_shares WHERE doc_id=? AND provider=?",
		docID, provider).Scan(&sh.DocID, &sh.Provider, &sh.ExternalURL, &sh.SharedBy, &sh.SharedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.Share{}, extdocs.ErrShareNotFound
	}
	if err != nil {
		return extdocs.Share{}, fmt.Errorf("sqlite: get external share: %w", err)
	}
	return sh, nil
}

// UpsertExternalShare 插入或替换映射（force 重共享 = 新 URL 覆盖旧值）。
func (s *Store) UpsertExternalShare(ctx context.Context, sh extdocs.Share) error {
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO doc_external_shares(doc_id, provider, external_url, shared_by, shared_at) VALUES(?,?,?,?,?) "+
			"ON CONFLICT(doc_id, provider) DO UPDATE SET external_url=excluded.external_url, shared_by=excluded.shared_by, shared_at=excluded.shared_at",
		sh.DocID, sh.Provider, sh.ExternalURL, sh.SharedBy, sh.SharedAt); err != nil {
		return fmt.Errorf("sqlite: upsert external share: %w", err)
	}
	return nil
}

// DeleteExternalShare 删除映射（行数 0 = ErrShareNotFound；不删远端文档）。
func (s *Store) DeleteExternalShare(ctx context.Context, docID, provider string) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM doc_external_shares WHERE doc_id=? AND provider=?", docID, provider)
	if err != nil {
		return fmt.Errorf("sqlite: delete external share: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return extdocs.ErrShareNotFound
	}
	return nil
}

// ---- sheet_transfers（RFC-0017 M2 extdocs.SheetTransferStore 端口）----

// transferColumns 全字段列序（INSERT/SELECT/Scan 共用）。
const transferColumns = "transfer_id, idempotency_key, direction, provider, status, doc_id, external_url, title, source_version, snapshot_hash, report_json, error, created_at, updated_at, cells_total, cells_written, state_json"

func scanTransfer(row interface{ Scan(...any) error }) (extdocs.SheetTransferRecord, error) {
	var r extdocs.SheetTransferRecord
	if err := row.Scan(&r.TransferID, &r.IdempotencyKey, &r.Direction, &r.Provider, &r.Status,
		&r.DocID, &r.ExternalURL, &r.Title, &r.SourceVersion, &r.SnapshotHash,
		&r.ReportJSON, &r.Error, &r.CreatedAt, &r.UpdatedAt,
		&r.CellsTotal, &r.CellsWritten, &r.StateJSON); err != nil {
		return extdocs.SheetTransferRecord{}, err
	}
	return r, nil
}

// InsertSheetTransfer 插入记录（幂等键被占 = extdocs.ErrTransferKeyConflict）。
func (s *Store) InsertSheetTransfer(ctx context.Context, rec extdocs.SheetTransferRecord) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sheet_transfers("+transferColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.TransferID, rec.IdempotencyKey, rec.Direction, rec.Provider, rec.Status,
		rec.DocID, rec.ExternalURL, rec.Title, rec.SourceVersion, rec.SnapshotHash,
		rec.ReportJSON, rec.Error, rec.CreatedAt, rec.UpdatedAt,
		rec.CellsTotal, rec.CellsWritten, rec.StateJSON)
	if err != nil {
		if strings.Contains(err.Error(), "sheet_transfers.idempotency_key") {
			return extdocs.ErrTransferKeyConflict
		}
		return fmt.Errorf("sqlite: insert sheet transfer: %w", err)
	}
	return nil
}

// GetSheetTransfer 按 ID 取（不存在 = extdocs.ErrTransferNotFound）。
func (s *Store) GetSheetTransfer(ctx context.Context, transferID string) (extdocs.SheetTransferRecord, error) {
	r, err := scanTransfer(s.db.QueryRowContext(ctx,
		"SELECT "+transferColumns+" FROM sheet_transfers WHERE transfer_id=?", transferID))
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.SheetTransferRecord{}, extdocs.ErrTransferNotFound
	}
	if err != nil {
		return extdocs.SheetTransferRecord{}, fmt.Errorf("sqlite: get sheet transfer: %w", err)
	}
	return r, nil
}

// GetSheetTransferByKey 按幂等键取（不存在 = extdocs.ErrTransferNotFound）。
func (s *Store) GetSheetTransferByKey(ctx context.Context, idempotencyKey string) (extdocs.SheetTransferRecord, error) {
	r, err := scanTransfer(s.db.QueryRowContext(ctx,
		"SELECT "+transferColumns+" FROM sheet_transfers WHERE idempotency_key=?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.SheetTransferRecord{}, extdocs.ErrTransferNotFound
	}
	if err != nil {
		return extdocs.SheetTransferRecord{}, fmt.Errorf("sqlite: get sheet transfer by key: %w", err)
	}
	return r, nil
}

// UpdateSheetTransfer 终态全量更新（按 transfer_id）。
func (s *Store) UpdateSheetTransfer(ctx context.Context, rec extdocs.SheetTransferRecord) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sheet_transfers SET direction=?, provider=?, status=?, doc_id=?, external_url=?, title=?, source_version=?, snapshot_hash=?, report_json=?, error=?, updated_at=?, cells_total=?, cells_written=?, state_json=? WHERE transfer_id=?",
		rec.Direction, rec.Provider, rec.Status, rec.DocID, rec.ExternalURL, rec.Title,
		rec.SourceVersion, rec.SnapshotHash, rec.ReportJSON, rec.Error, rec.UpdatedAt,
		rec.CellsTotal, rec.CellsWritten, rec.StateJSON, rec.TransferID)
	if err != nil {
		return fmt.Errorf("sqlite: update sheet transfer: %w", err)
	}
	return nil
}

// ListSheetTransfers 历史查询（docID/direction 空串 = 全量该维度；按
// created_at 倒序；limit ≤0 = 50）。
func (s *Store) ListSheetTransfers(ctx context.Context, docID, direction string, limit int) ([]extdocs.SheetTransferRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := "SELECT " + transferColumns + " FROM sheet_transfers"
	var conds []string
	var args []any
	if docID != "" {
		conds = append(conds, "doc_id=?")
		args = append(args, docID)
	}
	if direction != "" {
		conds = append(conds, "direction=?")
		args = append(args, direction)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sheet transfers: %w", err)
	}
	defer rows.Close()
	var out []extdocs.SheetTransferRecord
	for rows.Next() {
		r, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan sheet transfer: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkStaleRunningTransfers 启动扫残留 running → unknown（崩溃窗口收敛；
// 返回迁移行数）。
func (s *Store) MarkStaleRunningTransfers(ctx context.Context, now string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE sheet_transfers SET status=?, error=?, updated_at=? WHERE status=?",
		extdocs.TransferUnknown, "执行中断（应用重启）——请在历史中核对目标后手动处理", now, extdocs.TransferRunning)
	if err != nil {
		return 0, fmt.Errorf("sqlite: mark stale transfers: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// docFTSFieldsOf 文档信封 → 索引字段（标题 / 本批 ops 文本）。
func docFTSFieldsOf(env protocol.DocEnvelope) (title, body string, ok bool) {
	switch env.Type {
	case protocol.EventDocCreated:
		var p protocol.DocCreatedPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			return "", "", false
		}
		return p.Title, "", true
	case protocol.EventDocRevisionCommitted:
		var p protocol.DocRevisionCommittedPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			return "", "", false
		}
		var sb strings.Builder
		for _, op := range p.Ops {
			if op.Block != nil && op.Block.Text != "" {
				sb.WriteString(op.Block.Text)
				sb.WriteString("\n")
			}
			if op.Text != "" {
				sb.WriteString(op.Text)
				sb.WriteString("\n")
			}
		}
		return "", sb.String(), true
	}
	return "", "", false
}

// SearchDocs 实现 doc.DocSearcher（RFC-0014 §2.8 文档全文搜索）：FTS5 trigram
// 生产路径，语义与 SearchMessages 同规——子串、大小写不敏感、limit 1..100 默认 20；
// ≥3 字查询走 MATCH（phrase 引号包裹防语法注入），<3 字回退 LIKE 子串（标题或正文）。
// 每文档只取最新命中版本（同一文档的多条索引行归并，文档主页语义）。
// 自愈：索引行数与可索引事件数不符（旧库升级/历史缺行）即全量重建索引。
func (s *Store) SearchDocs(ctx context.Context, query string, limit int) ([]doc.DocSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []doc.DocSearchHit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if err := s.ensureDocFTS(ctx); err != nil {
		return nil, err
	}
	var where string
	var args []any
	// trigram tokenizer 要求查询 ≥3 字符（CJK 与 ASCII 同规）；短查询回退 LIKE 子串。
	if len([]rune(query)) >= 3 {
		phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		where = "doc_fts MATCH ?"
		args = append(args, phrase)
	} else {
		where = "(title LIKE ? COLLATE NOCASE OR body LIKE ? COLLATE NOCASE)"
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	args = append(args, limit)
	// SQLite 的 MAX() 聚合语义：裸列取自最大 version 行（每文档归并为最新命中）。
	rows, err := s.db.QueryContext(ctx, `
		SELECT doc_id, event_id, MAX(version) AS version, actor, occurred_at, title, body
		FROM doc_fts WHERE `+where+`
		GROUP BY doc_id ORDER BY version DESC, doc_id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search docs: %w", err)
	}
	defer rows.Close()
	hits := []doc.DocSearchHit{}
	for rows.Next() {
		var h doc.DocSearchHit
		if err := rows.Scan(&h.DocID, &h.EventID, &h.Version, &h.Actor, &h.OccurredAt, &h.Title, &h.Body); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc search: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ListDocs 实现 doc.DocLister（RFC-0014 §2.8 文档主页）：每文档聚合成摘要行——
// 标题取最新 created/renamed、状态取最新生命周期事件、更新者/时间取最新版本行。
// createdBy 空串 = 全部；删除级联后 doc_events 无行自然不出现。updated_at 倒序。
func (s *Store) ListDocs(ctx context.Context, createdBy string) ([]doc.DocSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.doc_id,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.title') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type IN ('doc.created', 'doc.renamed')
				ORDER BY e.version DESC LIMIT 1), '') AS title,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.format') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), 'markdown') AS format,
			(SELECT MAX(e.version) FROM doc_events e WHERE e.doc_id = d.doc_id) AS version,
			COALESCE((SELECT CASE WHEN e.type = 'doc.archived' THEN 'archived' ELSE 'active' END
				FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type IN ('doc.archived', 'doc.restored')
				ORDER BY e.version DESC LIMIT 1), 'active') AS status,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.created_by') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), '') AS created_by,
			COALESCE((SELECT e.occurred_at FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), '') AS created_at,
			COALESCE((SELECT json_extract(e.envelope, '$.actor.participant_id') FROM doc_events e
				WHERE e.doc_id = d.doc_id
				ORDER BY e.version DESC LIMIT 1), '') AS updated_by,
			COALESCE((SELECT e.occurred_at FROM doc_events e
				WHERE e.doc_id = d.doc_id
				ORDER BY e.version DESC LIMIT 1), '') AS updated_at
		FROM (SELECT DISTINCT doc_id FROM doc_events) d
		WHERE (? = '' OR COALESCE((SELECT json_extract(e.envelope, '$.payload.created_by') FROM doc_events e
			WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
			ORDER BY e.version ASC LIMIT 1), '') = ?)
		ORDER BY updated_at DESC, d.doc_id ASC`, createdBy, createdBy)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list docs: %w", err)
	}
	defer rows.Close()
	out := []doc.DocSummary{}
	for rows.Next() {
		var sum doc.DocSummary
		if err := rows.Scan(&sum.DocID, &sum.Title, &sum.Format, &sum.Version, &sum.Status,
			&sum.CreatedBy, &sum.CreatedAt, &sum.UpdatedBy, &sum.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// ensureDocFTS 索引自愈校验：FTS 行数 != 可索引事件数（v4 前旧库/异常缺口）即
// 全量重建索引（个人版文档规模毫秒级；重建后增量路径恢复）。
func (s *Store) ensureDocFTS(ctx context.Context) error {
	var ftsCount, evtCount int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_fts").Scan(&ftsCount); err != nil {
		return fmt.Errorf("sqlite: doc fts count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM doc_events WHERE type IN (?, ?)",
		protocol.EventDocCreated, protocol.EventDocRevisionCommitted,
	).Scan(&evtCount); err != nil {
		return fmt.Errorf("sqlite: doc event count: %w", err)
	}
	if ftsCount == evtCount {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM doc_fts"); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild clear: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT envelope FROM doc_events
		WHERE type IN (?, ?) ORDER BY doc_id, version`,
		protocol.EventDocCreated, protocol.EventDocRevisionCommitted)
	if err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild scan: %w", err)
	}
	var batch []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: doc fts rebuild row: %w", err)
		}
		batch = append(batch, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild rows: %w", err)
	}
	for _, raw := range batch {
		var env protocol.DocEnvelope
		if json.Unmarshal([]byte(raw), &env) != nil {
			continue
		}
		title, body, ok := docFTSFieldsOf(env)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO doc_fts (doc_id, event_id, version, actor, occurred_at, title, body)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			env.DocID, env.EventID, env.Version, env.Actor.ParticipantID, env.OccurredAt, title, body); err != nil {
			return fmt.Errorf("sqlite: doc fts rebuild insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild commit: %w", err)
	}
	return nil
}

// ---- Agent 身份域（RFC-0018 / ADR-0017，0.5.0；ADR-0018，0.5.1；端口实现）----

// identityExec 临界区内执行面（*sql.Conn / *sql.Tx 同构消费）。
type identityExec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// identityWriteTx 身份域写临界区（P1 修复，0.5.1 审校）：BEGIN IMMEDIATE——
// 状态/归属读取在写锁内进行，杜绝 WAL 下"读后升级"撞 SQLITE_BUSY_SNAPSHOT
// 把并发冲突外泄成 500（败者应按端口语义返回 ok=false/409）。等待发生在
// BEGIN 取锁处，由 busy_timeout 承担（与房间 appendTx 同机制）。
func (s *Store) identityWriteTx(ctx context.Context, fn func(q identityExec) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return nil
}

// appendAgentEvent 事件落流（临界区内调用）。
func appendAgentEvent(ctx context.Context, q identityExec, agentID, eventType string, payload map[string]string, occurredAt string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlite: event payload: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
INSERT INTO agent_events (agent_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
		agentID, eventType, string(raw), occurredAt); err != nil {
		return fmt.Errorf("sqlite: append %s: %w", eventType, err)
	}
	return nil
}

// appendAgentCreated 建 Agent 行的 created 事实落流（临界区内调用）。
func appendAgentCreated(ctx context.Context, q identityExec, a personalagent.Agent) error {
	return appendAgentEvent(ctx, q, a.AgentID, personalagent.EventCreated, map[string]string{
		"source": a.Source, "adapter": a.Adapter, "display_name": a.DisplayName,
	}, a.CreatedAt)
}

// EnsureAgent 幂等建 Agent（按 agent_id）；新建时 created 事件同事务落库。
func (s *Store) EnsureAgent(ctx context.Context, a personalagent.Agent) (bool, error) {
	var created bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
INSERT OR IGNORE INTO agents (agent_id, display_name, adapter, status, source, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			a.AgentID, a.DisplayName, a.Adapter, a.Status, a.Source, a.CreatedAt)
		if err != nil {
			return fmt.Errorf("sqlite: ensure agent: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: ensure agent rows: %w", err)
		}
		created = n > 0
		if created {
			return appendAgentCreated(ctx, q, a)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// EnsureAgentWithAlias 原子 get-or-create（审校 P1#1）：建 Agent 行（含 created
// 事件）与别名认领同一 BEGIN IMMEDIATE 事务——别名已被占（含并发竞争）时回滚
// 候选行并回读既有归属者；成功则三行同落。任一时点库内要么"整行落库"要么
// "什么都不留"。
func (s *Store) EnsureAgentWithAlias(ctx context.Context, a personalagent.Agent, kind personalagent.AliasKind, value string) (string, bool, error) {
	var owner string
	var created bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx,
			`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
			Scan(&owner)
		switch {
		case err == nil:
			return nil // 别名已被占（临界区内读到即最终态）：回读归属者
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: identity claim probe: %w", err)
		}
		res, err := q.ExecContext(ctx, `
INSERT OR IGNORE INTO agents (agent_id, display_name, adapter, status, source, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			a.AgentID, a.DisplayName, a.Adapter, a.Status, a.Source, a.CreatedAt)
		if err != nil {
			return fmt.Errorf("sqlite: identity claim agent: %w", err)
		}
		createdN, _ := res.RowsAffected()
		created = createdN > 0
		if created { // 新建行才落 created 事实（同 agent_id 重放不重复记）
			if err := appendAgentCreated(ctx, q, a); err != nil {
				return err
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := q.ExecContext(ctx, `
INSERT INTO agent_aliases (alias_kind, alias_value, agent_id, created_at) VALUES (?, ?, ?, ?)`,
			string(kind), value, a.AgentID, now); err != nil {
			// 防御兜底（BEGIN IMMEDIATE 下正常不可达）：唯一约束碰撞回读胜者
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				var winner string
				if qerr := q.QueryRowContext(ctx,
					`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
					Scan(&winner); qerr == nil {
					owner, created = winner, false
					return nil
				}
			}
			return fmt.Errorf("sqlite: identity claim alias: %w", err)
		}
		owner = a.AgentID
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return owner, created, nil
}

// AgentByID 单查 Agent 行。
func (s *Store) AgentByID(ctx context.Context, agentID string) (personalagent.Agent, bool, error) {
	var a personalagent.Agent
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id, display_name, adapter, status, source, created_at FROM agents WHERE agent_id = ?`, agentID).
		Scan(&a.AgentID, &a.DisplayName, &a.Adapter, &a.Status, &a.Source, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return personalagent.Agent{}, false, nil
	}
	if err != nil {
		return personalagent.Agent{}, false, fmt.Errorf("sqlite: agent by id: %w", err)
	}
	return a, true, nil
}

// TransitionAgent 事务转移（ADR-0018 §2；P1 修复：状态读取在 BEGIN IMMEDIATE
// 写临界区内）：读状态（allowedFrom 校验）→ 更新 agents 投影 → 追加
// status_changed 事件，同一事务。行缺失 ErrAgentNotFound；状态不匹配 ok=false
// （未动投影未落事件——并发败者按 409 语义返回，不外泄锁冲突）。
func (s *Store) TransitionAgent(ctx context.Context, agentID string, allowedFrom []string, to, reason, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE agent_id = ?`, agentID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return personalagent.ErrAgentNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: transition probe: %w", err)
		}
		if !allowedFromHas(allowedFrom, from) {
			return nil // ok=false：状态不匹配（含并发败者）
		}
		if _, err := q.ExecContext(ctx, `UPDATE agents SET status = ? WHERE agent_id = ?`, to, agentID); err != nil {
			return fmt.Errorf("sqlite: transition project: %w", err)
		}
		if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
			map[string]string{"from": from, "to": to, "reason": reason}, occurredAt); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// RetireAgent 原子退役（P2#4 修复，0.5.1 审校）：retiring 与 retired 两段
// status_changed 事件及投影终态在同一 BEGIN IMMEDIATE 事务内一次提交——
// 中途取消/失败整体回滚，不滞留 retiring。allowedFrom = {active, suspended}
// 或 {retiring}（0.6.12 审校 #3b：retiring 重试收尾也走此方法——只补终段且
// 过**未处置承诺事务门**；TransitionAgent 旁路不再用于 retiring→retired）。
func (s *Store) RetireAgent(ctx context.Context, agentID string, allowedFrom []string, reason, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE agent_id = ?`, agentID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return personalagent.ErrAgentNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: retire probe: %w", err)
		}
		if !allowedFromHas(allowedFrom, from) {
			return nil
		}
		// 0.6.12 审校 #3b：未处置承诺事务内核对（同库跨表直查——RequestReview
		// 复核 runs 同例）。承诺门先行判定与此处核验之间，承接可能翻转归属清空
		// 清单（或反序）——retired 终态不得在 owned 非终态任务仍存时落定；
		// 门非零 → 不写（done=false，服务层按承诺非空路径停留 retiring + 重查清单）。
		var unsettled int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM missions WHERE owner_agent_id = ? AND status NOT IN ('accepted', 'canceled')`,
			agentID).Scan(&unsettled); err != nil {
			return fmt.Errorf("sqlite: retire commitments probe: %w", err)
		}
		if unsettled > 0 {
			return nil // 事务内仍见未处置承诺（竞态窗口新承接）——停留，零写入
		}
		// 0.6.12 审校 #3b：retiring 入参（中断/承诺阻塞后的重试收尾）只补终段
		//——不重复记 retiring（既有语义）；承诺门对两个入口同效。
		if from != personalagent.StatusRetiring {
			if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
				map[string]string{"from": from, "to": personalagent.StatusRetiring, "reason": reason}, occurredAt); err != nil {
				return err
			}
		}
		if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
			map[string]string{"from": personalagent.StatusRetiring, "to": personalagent.StatusRetired, "reason": reason}, occurredAt); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE agents SET status = ? WHERE agent_id = ?`, personalagent.StatusRetired, agentID); err != nil {
			return fmt.Errorf("sqlite: retire project: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// allowedFromHas 来源集合判定。
func allowedFromHas(allowedFrom []string, status string) bool {
	for _, f := range allowedFrom {
		if f == status {
			return true
		}
	}
	return false
}

// AgentEvents 某 Agent 的事件流（seq 升序）。
func (s *Store) AgentEvents(ctx context.Context, agentID string) ([]personalagent.AgentEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, agent_id, event_type, payload, occurred_at FROM agent_events WHERE agent_id = ? ORDER BY seq`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: agent events: %w", err)
	}
	defer rows.Close()
	out := []personalagent.AgentEvent{}
	for rows.Next() {
		var ev personalagent.AgentEvent
		var payload string
		if err := rows.Scan(&ev.Seq, &ev.AgentID, &ev.EventType, &payload, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListAgents 全量 Agent，按创建序（rowid = 首次同步的确定性顺序）。
func (s *Store) ListAgents(ctx context.Context) ([]personalagent.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent_id, display_name, adapter, status, source, created_at FROM agents ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list agents: %w", err)
	}
	defer rows.Close()
	out := []personalagent.Agent{}
	for rows.Next() {
		var a personalagent.Agent
		if err := rows.Scan(&a.AgentID, &a.DisplayName, &a.Adapter, &a.Status, &a.Source, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentByAlias 别名解析；未登记 ok=false。
func (s *Store) AgentByAlias(ctx context.Context, kind personalagent.AliasKind, value string) (string, bool, error) {
	var agentID string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
		Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: agent by alias: %w", err)
	}
	return agentID, true, nil
}

// AddAlias 幂等登记别名：同键同目标 no-op（added=false）；同键异目标
// personalagent.ErrAliasConflict。先查后插（单进程写串行化下安全），唯一
// 主键为并发兜底——撞约束时回读归属定性。
func (s *Store) AddAlias(ctx context.Context, kind personalagent.AliasKind, value, agentID string) (bool, error) {
	var owner string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
		Scan(&owner)
	switch {
	case err == nil:
		if owner != agentID {
			return false, personalagent.ErrAliasConflict
		}
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("sqlite: alias owner: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO agent_aliases (alias_kind, alias_value, agent_id, created_at) VALUES (?, ?, ?, ?)`,
		string(kind), value, agentID, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			// 兜底路径：回读归属定性（同目标 = 幂等，异目标 = 冲突）
			var owner2 string
			if qerr := s.db.QueryRowContext(ctx,
				`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
				Scan(&owner2); qerr == nil && owner2 != agentID {
				return false, personalagent.ErrAliasConflict
			}
			return false, nil
		}
		return false, fmt.Errorf("sqlite: add alias: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AliasesOf 某 Agent 的全部别名行（登记序）。
func (s *Store) AliasesOf(ctx context.Context, agentID string) ([]personalagent.Alias, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias_kind, alias_value FROM agent_aliases WHERE agent_id = ? ORDER BY rowid`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: aliases of: %w", err)
	}
	defer rows.Close()
	out := []personalagent.Alias{}
	for rows.Next() {
		var kind string
		var a personalagent.Alias
		if err := rows.Scan(&kind, &a.Value); err != nil {
			return nil, fmt.Errorf("sqlite: scan alias: %w", err)
		}
		a.Kind = personalagent.AliasKind(kind)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- 职责模板域（RFC-0018 §5.1 / ADR-0019，0.5.2；v9 表，端口实现）----

const slotSpecCols = `slot_spec_id, version, schema_version, name, responsibility, content_json, created_at`

func scanSlotSpec(row interface{ Scan(...any) error }) (slot.SlotSpec, error) {
	var s slot.SlotSpec
	var content string
	if err := row.Scan(&s.SlotSpecID, &s.Version, &s.SchemaVersion, &s.Name, &s.Responsibility, &content, &s.CreatedAt); err != nil {
		return slot.SlotSpec{}, err
	}
	if err := json.Unmarshal([]byte(content), &s.SpecContent); err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content unmarshal: %w", err)
	}
	return s, nil
}

// CreateSpec 建谱系（首版）：谱系已存在 ErrSpecExists（行级探测在 BEGIN
// IMMEDIATE 临界区内——并发同建恰一胜者）。
func (s *Store) CreateSpec(ctx context.Context, spec slot.SlotSpec) (slot.SlotSpec, error) {
	raw, err := json.Marshal(spec.SpecContent)
	if err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content marshal: %w", err)
	}
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		var exists int
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM slot_specs WHERE slot_spec_id = ?)`, spec.SlotSpecID).
			Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: slot spec probe: %w", err)
		}
		if exists == 1 {
			return slot.ErrSpecExists
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_specs (`+slotSpecCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			spec.SlotSpecID, spec.Version, spec.SchemaVersion, spec.Name,
			spec.Responsibility, string(raw), spec.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: create slot spec: %w", err)
		}
		return nil
	})
	if err != nil {
		return slot.SlotSpec{}, err
	}
	return spec, nil
}

// AppendSpecVersion 追加下一不可变版本：latest+1 取号在 BEGIN IMMEDIATE
// 临界区内（并发追加串行化为连续版本，无空洞无重复）。
func (s *Store) AppendVersion(ctx context.Context, slotSpecID string, content slot.SpecContent, now string) (slot.SlotSpec, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content marshal: %w", err)
	}
	out := slot.SlotSpec{}
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		var latest int
		err := q.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM slot_specs WHERE slot_spec_id = ?`, slotSpecID).
			Scan(&latest)
		if err != nil {
			return fmt.Errorf("sqlite: slot latest: %w", err)
		}
		if latest == 0 {
			return slot.ErrNotFound
		}
		out = slot.SlotSpec{
			SlotSpecID: slotSpecID, Version: latest + 1, SchemaVersion: slot.SchemaVersion,
			SpecContent: content, CreatedAt: now,
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_specs (`+slotSpecCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			out.SlotSpecID, out.Version, out.SchemaVersion, out.Name,
			out.Responsibility, string(raw), out.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: append slot version: %w", err)
		}
		return nil
	})
	if err != nil {
		return slot.SlotSpec{}, err
	}
	return out, nil
}

// ListSummaries 谱系摘要（首版创建序）。
func (s *Store) ListSummaries(ctx context.Context) ([]slot.SpecSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_spec_id,
       (SELECT name FROM slot_specs t WHERE t.slot_spec_id = s.slot_spec_id ORDER BY version DESC LIMIT 1),
       MAX(version) AS latest_version,
       COUNT(*) AS version_count,
       MIN(created_at) AS created_at,
       MAX(created_at) AS updated_at
  FROM slot_specs s
 GROUP BY slot_spec_id
 ORDER BY MIN(rowid)`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: slot summaries: %w", err)
	}
	defer rows.Close()
	out := []slot.SpecSummary{}
	for rows.Next() {
		var sum slot.SpecSummary
		if err := rows.Scan(&sum.SlotSpecID, &sum.Name, &sum.LatestVersion,
			&sum.VersionCount, &sum.CreatedAt, &sum.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan slot summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// Latest 最新版本；谱系未知 ok=false。
func (s *Store) Latest(ctx context.Context, slotSpecID string) (slot.SlotSpec, bool, error) {
	var s0 slot.SlotSpec
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT `+slotSpecCols+` FROM slot_specs WHERE slot_spec_id = ? ORDER BY version DESC LIMIT 1`, slotSpecID).
		Scan(&s0.SlotSpecID, &s0.Version, &s0.SchemaVersion, &s0.Name, &s0.Responsibility, &content, &s0.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.SlotSpec{}, false, nil
	}
	if err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot latest: %w", err)
	}
	if err := json.Unmarshal([]byte(content), &s0.SpecContent); err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot content unmarshal: %w", err)
	}
	return s0, true, nil
}

// Versions 全版本升序；谱系未知 ok=false。
func (s *Store) Versions(ctx context.Context, slotSpecID string) ([]slot.SlotSpec, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+slotSpecCols+` FROM slot_specs WHERE slot_spec_id = ? ORDER BY version`, slotSpecID)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: slot versions: %w", err)
	}
	defer rows.Close()
	out := []slot.SlotSpec{}
	for rows.Next() {
		spec, err := scanSlotSpec(rows)
		if err != nil {
			return nil, false, fmt.Errorf("sqlite: scan slot spec: %w", err)
		}
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

// SpecByVersion 钉版本单查（0.5.5 准入取钉住版内容）；未知 ok=false。
func (s *Store) SpecByVersion(ctx context.Context, slotSpecID string, version int) (slot.SlotSpec, bool, error) {
	var s0 slot.SlotSpec
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT `+slotSpecCols+` FROM slot_specs WHERE slot_spec_id = ? AND version = ?`, slotSpecID, version).
		Scan(&s0.SlotSpecID, &s0.Version, &s0.SchemaVersion, &s0.Name, &s0.Responsibility, &content, &s0.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.SlotSpec{}, false, nil
	}
	if err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot by version: %w", err)
	}
	if err := json.Unmarshal([]byte(content), &s0.SpecContent); err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot content unmarshal: %w", err)
	}
	return s0, true, nil
}

// ---- 授权档案域（RFC-0018 §5.2 / ADR-0022，0.5.5；v12 表，端口实现）----

// PutGrant upsert 授权档案（BEGIN IMMEDIATE 临界区内整行替换——并发写
// 串行化，终态为某次请求的完整快照，无半写）。
func (s *Store) PutGrant(ctx context.Context, g slot.AgentGrant) error {
	skills, err := json.Marshal(g.Skills)
	if err != nil {
		return fmt.Errorf("sqlite: grant skills marshal: %w", err)
	}
	tools, err := json.Marshal(g.Tools)
	if err != nil {
		return fmt.Errorf("sqlite: grant tools marshal: %w", err)
	}
	contexts, err := json.Marshal(g.Context)
	if err != nil {
		return fmt.Errorf("sqlite: grant context marshal: %w", err)
	}
	caps, err := json.Marshal(g.Runtime.Capabilities)
	if err != nil {
		return fmt.Errorf("sqlite: grant capabilities marshal: %w", err)
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_agent_grants (agent_id, skills, tools, context, runtime_capabilities, workspace_access, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_id) DO UPDATE SET
  skills = excluded.skills,
  tools = excluded.tools,
  context = excluded.context,
  runtime_capabilities = excluded.runtime_capabilities,
  workspace_access = excluded.workspace_access,
  updated_at = excluded.updated_at`,
			g.AgentID, string(skills), string(tools), string(contexts), string(caps),
			g.Runtime.WorkspaceAccess, g.UpdatedAt); err != nil {
			return fmt.Errorf("sqlite: put grant: %w", err)
		}
		return nil
	})
}

// GrantByAgent 读取授权档案；未设置 ok=false（空授权安全缺省）。
func (s *Store) GrantByAgent(ctx context.Context, agentID string) (slot.AgentGrant, bool, error) {
	var g slot.AgentGrant
	var skills, tools, contexts, caps string
	err := s.db.QueryRowContext(ctx, `
SELECT agent_id, skills, tools, context, runtime_capabilities, workspace_access, updated_at
  FROM slot_agent_grants WHERE agent_id = ?`, agentID).
		Scan(&g.AgentID, &skills, &tools, &contexts, &caps, &g.Runtime.WorkspaceAccess, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.AgentGrant{}, false, nil
	}
	if err != nil {
		return slot.AgentGrant{}, false, fmt.Errorf("sqlite: grant by agent: %w", err)
	}
	for _, u := range []struct {
		raw string
		out any
	}{
		{skills, &g.Skills}, {tools, &g.Tools}, {contexts, &g.Context}, {caps, &g.Runtime.Capabilities},
	} {
		if err := json.Unmarshal([]byte(u.raw), u.out); err != nil {
			return slot.AgentGrant{}, false, fmt.Errorf("sqlite: grant unmarshal: %w", err)
		}
	}
	return g, true, nil
}

// ---- Mission 契约域（RFC-0018 §3.1/§3.2/§6.1 / ADR-0023，0.6.0；v13 表，端口实现）----

// missionCols missions 投影读取列。
const missionCols = `mission_id, contract_version, status, owner_agent_id, requester, objective, content, created_at, updated_at, waiting_reason, review_package_id, review_round, pending_owner_agent_id, pending_offer_id, slot_instance_id`

func scanMission(row interface{ Scan(...any) error }) (mission.Mission, error) {
	var m mission.Mission
	var content string
	if err := row.Scan(&m.MissionID, &m.ContractVersion, &m.Status, &m.OwnerAgentID,
		&m.Requester, &m.Objective, &content, &m.CreatedAt, &m.UpdatedAt, &m.WaitingReason, &m.ReviewPackageID, &m.ReviewRound, &m.PendingOwnerAgentID, &m.PendingOfferID, &m.SlotInstanceID); err != nil {
		return mission.Mission{}, err
	}
	if err := json.Unmarshal([]byte(content), &m.Content); err != nil {
		return mission.Mission{}, fmt.Errorf("sqlite: mission content unmarshal: %w", err)
	}
	return m, nil
}

func appendMissionEvent(ctx context.Context, q identityExec, missionID, eventType string, payload map[string]string, occurredAt string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlite: mission event marshal: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
		missionID, eventType, string(raw), occurredAt); err != nil {
		return fmt.Errorf("sqlite: append mission event: %w", err)
	}
	return nil
}

func insertMissionVersion(ctx context.Context, q identityExec, m mission.Mission, content string) error {
	if _, err := q.ExecContext(ctx, `
INSERT INTO mission_contract_versions (mission_id, version, status, owner_agent_id, requester, content, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.MissionID, m.ContractVersion, m.Status, m.OwnerAgentID, m.Requester,
		content, m.UpdatedAt); err != nil {
		return fmt.Errorf("sqlite: insert mission version: %w", err)
	}
	return nil
}

// Revise CAS 修订：版本匹配且非终态才落新版本行 + 投影 + 事件，单事务；
// ok=false = 版本不匹配或已终态（服务方重读区分，ADR-0023）。
func (s *Store) ReviseMission(ctx context.Context, missionID string, expectedVersion int, expectedOwner string, next mission.Mission) (mission.Mission, bool, error) {
	raw, err := json.Marshal(next.Content)
	if err != nil {
		return mission.Mission{}, false, fmt.Errorf("sqlite: mission content marshal: %w", err)
	}
	ok := false // 事务内判定（不得事后按版本号推断——CAS 败者会被误判胜）
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		var err error
		next, ok, err = reviseMissionTx(ctx, q, missionID, expectedVersion, expectedOwner, next, string(raw))
		return err
	})
	if err != nil {
		return mission.Mission{}, false, err
	}
	if !ok {
		return mission.Mission{}, false, nil
	}
	return next, true, nil
}

// reviseMissionTx 修订事务体（0.6.13 审校 #3 抽取：ReviseMission 与网关原子
// 续接 UpdateMissionAtomic 共用——探版本/状态/归属锚/冻结窗 → 条件更新 →
// 版本行 + 事件）。ok=false = 前置不符（调用方重读分辨）；next 返回事务内
// 修正后的投影（执行控制状态保持）。
func reviseMissionTx(ctx context.Context, q identityExec, missionID string, expectedVersion int, expectedOwner string, next mission.Mission, raw string) (mission.Mission, bool, error) {
	var version int
	var status, owner, pending string
	err := q.QueryRowContext(ctx,
		`SELECT contract_version, status, owner_agent_id, pending_owner_agent_id FROM missions WHERE mission_id = ?`, missionID).
		Scan(&version, &status, &owner, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return mission.Mission{}, false, mission.ErrMissionNotFound
	}
	if err != nil {
		return mission.Mission{}, false, fmt.Errorf("sqlite: revise probe: %w", err)
	}
	// 0.6.9：accepted 同终态拒修订；reviewing 基线冻结。0.6.12 / ADR-0035 §3：
	// 在途交接冻结窗（事务内核对——服务层先读后并发 offer 落地也拦）；
	// 审校 #2：归属锚——承接翻转 owner 不递增版本，携旧 owner 的修订拒。
	if version != expectedVersion || owner != expectedOwner || status == mission.StatusCanceled ||
		status == mission.StatusAccepted || status == mission.StatusReviewing || pending != "" {
		return mission.Mission{}, false, nil // ok=false：败因由调用方重读分辨
	}
	// 0.6.6 审校 P1：执行控制投影的事务内保持——next.Status 是服务层旧读
	// 快照（读到 ready 后暂停/取消落地，控制命令不递增版本，CAS 版本面
	// 放行），事务内以现行状态为准（running/paused/canceling 不被陈旧
	// ready 盖回调度列）。
	if status == mission.StatusRunning || status == mission.StatusPaused || status == mission.StatusCanceling || status == mission.StatusWaiting {
		next.Status = status
	}
	if _, err := q.ExecContext(ctx, `
UPDATE missions SET contract_version = ?, status = ?, owner_agent_id = ?, objective = ?, content = ?, updated_at = ?
 WHERE mission_id = ?`,
		next.ContractVersion, next.Status, next.OwnerAgentID, next.Objective,
		raw, next.UpdatedAt, missionID); err != nil {
		return mission.Mission{}, false, fmt.Errorf("sqlite: revise mission: %w", err)
	}
	if err := insertMissionVersion(ctx, q, next, raw); err != nil {
		return mission.Mission{}, false, err
	}
	if err := appendMissionEvent(ctx, q, missionID, mission.EventContractRevised, map[string]string{
		"from_version": strconv.Itoa(expectedVersion), "to_version": strconv.Itoa(next.ContractVersion),
	}, next.UpdatedAt); err != nil {
		return mission.Mission{}, false, err
	}
	if status != next.Status {
		if err := appendMissionEvent(ctx, q, missionID, mission.EventStatusChanged, map[string]string{
			"from": status, "to": next.Status,
		}, next.UpdatedAt); err != nil {
			return mission.Mission{}, false, err
		}
	}
	return next, true, nil
}

// UpdateMissionAtomic 网关续接的原子面（0.6.13 审校 #3/#11/#12/#13/#16，
// dispatch.AtomicUpdater 实装）：**同一写事务**[幂等键探查/认领（reason 随行）
// → 任务探查 → owner 在册且 active（事务内身份门）→ mission.PrepareRevision
// 纯域构造 → reviseMissionTx（CAS/冻结窗/终态）→ 回执完结（receipt_id 沿用
// pending 原行，target_refs 携修订后版本，reason 持久化）]。任一步失败整体
// 回滚：领域拒绝不占键（修正后重试）、修订已生效而回执失败不再出现。回执
// 幂等键异摘要 → dispatch.ErrIdempotencyConflict；同摘要已完结 → 原回执重放。
func (s *Store) UpdateMissionAtomic(ctx context.Context, cmd dispatch.UpdateCmd, dg, now, newReceiptID string) (dispatch.Receipt, bool, error) {
	var out dispatch.Receipt
	var replayed bool
	preExisting := false // 三审 #2/#6：认领行是否早于本事务（并发孪生重驱动/旧版遗留）
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		// —— 幂等键探查/认领 ——
		row := q.QueryRowContext(ctx, `
SELECT receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at
  FROM dispatch_receipts WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
			dispatch.CallerOwner, dispatch.ScopeWorkspace, cmd.IdempotencyKey)
		ex, found, err := scanReceipt(row)
		if err != nil {
			return fmt.Errorf("sqlite: atomic claim probe: %w", err)
		}
		claim := dispatch.Receipt{
			ReceiptID:      newReceiptID,
			CallerRef:      dispatch.CallerOwner,
			Scope:          dispatch.ScopeWorkspace,
			IdempotencyKey: cmd.IdempotencyKey,
			RequestID:      cmd.RequestID,
			RequestDigest:  dg,
			Reason:         cmd.Reason,
			TargetRefs:     []string{cmd.MissionID},
		}
		preExisting = found // 三审 #2/#6：认领来源（新插入可回滚释放 / 既有行为遗留或孪生）
		if found {
			if ex.RequestDigest != dg {
				return fmt.Errorf("%w: 键 %s 已绑定其他请求", dispatch.ErrIdempotencyConflict, cmd.IdempotencyKey)
			}
			if ex.DomainCommandID != "" {
				out, replayed = ex, true // 同摘要已完结 → 重放原回执
				return nil
			}
			// pending 同摘要 → 采纳原行（#13：receipt_id/request_id 稳定；复审
			// #12：reason 空值（v30 旧列缺省）回落本次命令值——摘要等值已证明
			// 同一请求，reason 在摘要内，不被旧空列抹掉）。
			claim.ReceiptID = ex.ReceiptID
			claim.RequestID = ex.RequestID
			claim.Reason = ex.Reason
			if claim.Reason == "" {
				claim.Reason = cmd.Reason
			}
		} else {
			targets, err := json.Marshal(claim.TargetRefs)
			if err != nil {
				return fmt.Errorf("sqlite: atomic claim targets marshal: %w", err)
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES (?, ?, ?, ?, ?, ?, '', 0, '', ?, '[]', ?, '')`,
				claim.ReceiptID, claim.CallerRef, claim.Scope, claim.IdempotencyKey,
				claim.RequestID, claim.RequestDigest, string(targets), claim.Reason); err != nil {
				return fmt.Errorf("sqlite: atomic claim key: %w", err)
			}
		}
		// —— 任务探查 + 事务内身份门（#12：未指派/不在册/非 active 皆拒——回滚） ——
		cur, found, err := missionByIDTx(ctx, q, cmd.MissionID)
		if err != nil {
			return err
		}
		if !found {
			return mission.ErrMissionNotFound
		}
		if cur.OwnerAgentID == "" {
			return fmt.Errorf("%w: 任务未指派负责人（%s）——先交办或交接", dispatch.ErrTargetNotActive, cmd.MissionID)
		}
		var status string
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM agents WHERE agent_id = ?`, cur.OwnerAgentID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: 负责人 %s 不在册", dispatch.ErrTargetNotActive, cur.OwnerAgentID)
			}
			return fmt.Errorf("sqlite: atomic agent probe: %w", err)
		}
		if status != mission.AgentStatusActive {
			return fmt.Errorf("%w: 负责人 %s 非 active（%s）", dispatch.ErrTargetNotActive, cur.OwnerAgentID, status)
		}
		// —— 纯域构造 + 修订事务体（守卫单源 PrepareRevision） ——
		next, err := mission.PrepareRevision(cur, cur.OwnerAgentID, cur.OwnerAgentID, cmd.Contract, now)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(next.Content)
		if err != nil {
			return fmt.Errorf("sqlite: atomic content marshal: %w", err)
		}
		up, ok, err := reviseMissionTx(ctx, q, cmd.MissionID, cmd.ExpectedVersion, cur.OwnerAgentID, next, string(raw))
		if err != nil {
			return err
		}
		if !ok {
			// 三审 #2/#6：CAS 败按认领来源分流——**新插入**认领（本事务）→
			// 败因分类透传（version_conflict 等，事务回滚连认领一并撤销——不
			// 占键，修正后重试）；**既有** pending（并发孪生重驱动或旧候选版遗
			// 留）→ 版本与正文等值不可证明修订归属（v2 可能由他键提交——新库
			// 冒领根因），返回待核验：携当前版本同键重试（确认未执行则正常执
			// 行）或 release 处置。认领行保留（事务回滚不影响既有行）。
			if !preExisting {
				return classifyReviseRefusal(ctx, q, cmd.MissionID, cmd.ExpectedVersion, cur.OwnerAgentID)
			}
			var curVersion int
			if err := q.QueryRowContext(ctx,
				`SELECT contract_version FROM missions WHERE mission_id = ?`, cmd.MissionID).Scan(&curVersion); err == nil && curVersion != cmd.ExpectedVersion {
				return fmt.Errorf("%w：键 %s 的认领早于本请求（当前 v%d，命令期望 v%d）",
					dispatch.ErrLegacyPendingUnresolved, cmd.IdempotencyKey, curVersion, cmd.ExpectedVersion)
			}
			return classifyReviseRefusal(ctx, q, cmd.MissionID, cmd.ExpectedVersion, cur.OwnerAgentID)
		}
		// —— 回执完结（与修订同一事务提交——#3/#16） ——
		out = claim
		out.DomainCommandID = fmt.Sprintf("%s:revise:v%d", up.MissionID, up.ContractVersion)
		out.TargetRefs = []string{"mission:" + up.MissionID + ":v" + strconv.Itoa(up.ContractVersion)}
		out.ReturnRefs = up.Content.ReturnTargets
		out.CommittedAt = now
		targets, err := json.Marshal(out.TargetRefs)
		if err != nil {
			return fmt.Errorf("sqlite: atomic targets marshal: %w", err)
		}
		returns, err := json.Marshal(out.ReturnRefs)
		if err != nil {
			return fmt.Errorf("sqlite: atomic returns marshal: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
UPDATE dispatch_receipts SET request_id = ?, domain_command_id = ?, target_refs = ?, return_refs = ?, reason = ?, committed_at = ?
 WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
			out.RequestID, out.DomainCommandID, string(targets), string(returns), out.Reason, out.CommittedAt,
			out.CallerRef, out.Scope, out.IdempotencyKey); err != nil {
			return fmt.Errorf("sqlite: atomic finalize receipt: %w", err)
		}
		return nil
	})
	if err != nil {
		return dispatch.Receipt{}, false, err
	}
	return out, replayed, nil
}

// classifyReviseRefusal 修订 CAS 败因分类（事务内重读：版本冲突/归属锚/
// 终态或冻结窗/未知——与服务层 casOrOwnerOrTransition 同语义，原子面复用）。
func classifyReviseRefusal(ctx context.Context, q identityExec, missionID string, expectedVersion int, expectedOwner string) error {
	var version int
	var status, owner, pending string
	err := q.QueryRowContext(ctx,
		`SELECT contract_version, status, owner_agent_id, pending_owner_agent_id FROM missions WHERE mission_id = ?`, missionID).
		Scan(&version, &status, &owner, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return mission.ErrMissionNotFound
	}
	if err != nil {
		return fmt.Errorf("sqlite: revise refusal probe: %w", err)
	}
	if owner != expectedOwner {
		return fmt.Errorf("%w: 期望负责人 %q，当前 %q", mission.ErrOwnerMismatch, expectedOwner, owner)
	}
	if version != expectedVersion {
		return fmt.Errorf("%w: 期望 v%d，当前 v%d", mission.ErrVersionConflict, expectedVersion, version)
	}
	return fmt.Errorf("%w: 当前 %s（终态/验收冻结/交接冻结窗）", mission.ErrInvalidTransition, status)
}

// missionByIDTx 事务连接上的单查（identityWriteTx 临界区内——池连接与事务
// 连接不得混用）。
func missionByIDTx(ctx context.Context, q identityExec, missionID string) (mission.Mission, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+missionCols+` FROM missions WHERE mission_id = ?`, missionID)
	m, err := scanMission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return mission.Mission{}, false, nil
	}
	if err != nil {
		return mission.Mission{}, false, fmt.Errorf("sqlite: mission by id (tx): %w", err)
	}
	return m, true, nil
}

// transitionMissionIf 通用控制转移（0.6.6 / ADR-0029）：单事务内探版本/状态
// → 条件更新 → status_changed 事件。guard 判定可进才转移；ok=false = 前置
// 不符（服务方重读分辨——幂等先读在服务层）。
func (s *Store) transitionMissionIf(ctx context.Context, missionID string, expectedVersion int,
	to, reason, occurredAt string, guard func(version int, status, pending string) bool) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var version int
		var status, pending string
		err := q.QueryRowContext(ctx,
			`SELECT contract_version, status, pending_owner_agent_id FROM missions WHERE mission_id = ?`, missionID).Scan(&version, &status, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return mission.ErrMissionNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: control probe: %w", err)
		}
		if !guard(version, status, pending) {
			return nil
		}
		// 0.6.7：等待原因——paused 保留（恢复据此回 waiting）、回 waiting 保留；
		// 0.6.9 审校 #13：受审包锚——canceling/canceled 清空（accepted 经
		// RecordReview 保留；退回已在 RecordReview 清）。
		setSQL := `UPDATE missions SET status = ?, updated_at = ?, waiting_reason = '', review_package_id = '' WHERE mission_id = ?`
		if to == mission.StatusPaused || to == mission.StatusWaiting {
			setSQL = `UPDATE missions SET status = ?, updated_at = ? WHERE mission_id = ?`
		}
		if _, err := q.ExecContext(ctx, setSQL, to, occurredAt, missionID); err != nil {
			return fmt.Errorf("sqlite: control mission: %w", err)
		}
		// 七审 #4：暂停/取消的 Run 停止标记随**同一事务**写入（信号路径零持久
		// 写——暂停落库与标记落库两段提交间的交错恢复窗口在此闭合；resume 不
		// 清除，终态写事务/恢复面核对 Run 自身停止事实）。cause 用公开动作码。
		if to == mission.StatusPaused || to == mission.StatusCanceling {
			cause := "pause"
			if to == mission.StatusCanceling {
				cause = "cancel"
			}
			mp, merr := runEventPayloadJSON(map[string]any{"cause": cause, "persisted_in_tx": true})
			if merr != nil {
				return merr
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at)
SELECT run_id, 'stop_requested', ?, ? FROM runs
 WHERE mission_id = ? AND state IN ('pending', 'running')
   AND NOT EXISTS (SELECT 1 FROM run_events e
                    WHERE e.run_id = runs.run_id AND e.event_type = 'stop_requested')`,
				mp, occurredAt, missionID); err != nil {
				return fmt.Errorf("sqlite: control stop markers: %w", err)
			}
		}
		payload := map[string]string{"from": status, "to": to}
		if reason != "" {
			payload["reason"] = reason
		}
		if err := appendMissionEvent(ctx, q, missionID, mission.EventStatusChanged, payload, occurredAt); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	got, found, err := s.MissionByID(ctx, missionID)
	if err != nil || !found {
		return mission.Mission{}, false, err
	}
	return got, true, nil
}

// PauseMission CAS 暂停（ready | running → paused）。
func (s *Store) PauseMission(ctx context.Context, missionID string, expectedVersion int, reason, occurredAt string) (mission.Mission, bool, error) {
	return s.transitionMissionIf(ctx, missionID, expectedVersion, mission.StatusPaused, reason, occurredAt,
		func(version int, status, _ string) bool {
			return version == expectedVersion && (status == mission.StatusReady || status == mission.StatusRunning || status == mission.StatusWaiting)
		})
}

// ResumeMission CAS 恢复（paused → target；target ∈ ready|draft 由服务层
// 按当前契约完备性判定——0.6.6 审校 P2）。
func (s *Store) ResumeMission(ctx context.Context, missionID string, expectedVersion int, target, occurredAt string) (mission.Mission, bool, error) {
	return s.transitionMissionIf(ctx, missionID, expectedVersion, target, "", occurredAt,
		func(version int, status, _ string) bool {
			return version == expectedVersion && status == mission.StatusPaused
		})
}

// MarkCanceling CAS 停止意图（非终态 → canceling；0.6.9：accepted 为终态拒；
// 0.6.12：在途交接冻结窗——事务内核对）。
func (s *Store) MarkCanceling(ctx context.Context, missionID string, expectedVersion int, reason, occurredAt string) (mission.Mission, bool, error) {
	return s.transitionMissionIf(ctx, missionID, expectedVersion, mission.StatusCanceling, reason, occurredAt,
		func(version int, status, pending string) bool {
			return version == expectedVersion && status != mission.StatusCanceled && status != mission.StatusAccepted && pending == ""
		})
}

// FinishCancel 收尾（canceling → canceled；无版本守卫——控制序在服务层）。
func (s *Store) FinishCancel(ctx context.Context, missionID, reason, occurredAt string) (mission.Mission, bool, error) {
	return s.transitionMissionIf(ctx, missionID, 0, mission.StatusCanceled, reason, occurredAt,
		func(_ int, status, _ string) bool {
			return status == mission.StatusCanceling
		})
}

// CreateMission 无门建单（域内直建契约路径——owner 是归属事实，任意生命周期
// 可指派；来源撞键语义不变）。
func (s *Store) CreateMission(ctx context.Context, m mission.Mission, src *mission.RoomSource) (mission.Mission, error) {
	return s.CreateMissionGated(ctx, m, src, nil)
}

// CreateMissionGated 带事务门建单（0.6.13 审校 #1/#2 + 复审 #3/#5/#7）：门在
// **同一写事务**内核对——slot 意图五核（实例在册 / active 任职 == 预期 /
// 负责人 active / 授权准入**含 workspace_access** / 模板唯一性锚）+
// RequireOwnerActive 身份门（预检后退役/暂停不得落入新承诺）。不符 →
// ErrSlotIntentViolated / ErrOwnerGateViolated 整体回滚（网关映射 route_stale）。
// SlotInstanceID 随投影落列（执行装配按实例固定 Binding——同人双任职不串用）。
func (s *Store) CreateMissionGated(ctx context.Context, m mission.Mission, src *mission.RoomSource, gate *mission.CreationGate) (mission.Mission, error) {
	raw, err := json.Marshal(m.Content)
	if err != nil {
		return mission.Mission{}, fmt.Errorf("sqlite: mission content marshal: %w", err)
	}
	if gate != nil && gate.Slot != nil {
		m.SlotInstanceID = gate.Slot.InstanceID // 创建事实（直呼存储面同服务语义）
	}
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		if err := verifyCreationGateTx(ctx, q, gate, m.OwnerAgentID); err != nil {
			return err
		}
		if src != nil {
			var existing string
			err := q.QueryRowContext(ctx,
				`SELECT mission_id FROM mission_sources WHERE room_id = ? AND task_id = ?`, src.RoomID, src.TaskID).Scan(&existing)
			if err == nil {
				return fmt.Errorf("%w: (%s, %s) 已映射 %s", mission.ErrSourceAlreadyMapped, src.RoomID, src.TaskID, existing)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: source probe: %w", err)
			}
			if _, err := q.ExecContext(ctx,
				`INSERT INTO mission_sources (room_id, task_id, mission_id, created_at) VALUES (?, ?, ?, ?)`,
				src.RoomID, src.TaskID, m.MissionID, m.CreatedAt); err != nil {
				return fmt.Errorf("sqlite: insert mission source: %w", err)
			}
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO missions (`+missionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.MissionID, m.ContractVersion, m.Status, m.OwnerAgentID, m.Requester,
			m.Objective, string(raw), m.CreatedAt, m.UpdatedAt, m.WaitingReason, m.ReviewPackageID, m.ReviewRound, m.PendingOwnerAgentID, m.PendingOfferID, m.SlotInstanceID); err != nil {
			return fmt.Errorf("sqlite: create mission: %w", err)
		}
		if err := insertMissionVersion(ctx, q, m, string(raw)); err != nil {
			return err
		}
		return appendMissionEvent(ctx, q, m.MissionID, mission.EventCreated, map[string]string{
			"contract_version": strconv.Itoa(m.ContractVersion), "status": m.Status,
		}, m.CreatedAt)
	})
	if err != nil {
		return mission.Mission{}, err
	}
	if src != nil {
		rs := *src
		m.RoomSource = &rs
	}
	return m, nil
}

// verifyCreationGateTx 建单事务门（0.6.13 审校 #2/#7 + 复审 #3/#5/#7，RFC
// §4.5/§4.6）。nil 门直通（域内直建契约路径）。
//
// slot 意图五核：实例在册 → active 任职 == 预期 Agent（换绑漂移拒）→ 负责人
// 身份 active（暂停身份不建单）→ 授权准入（任职钉版职责要求 vs 授权档案
// **完整快照含 workspace_access**——复审 #5 漏读会把合法 read_write 当空值
// 误拒；与 GrantByAgent 同构扫描 + EvaluateReadiness 单源）→ 模板唯一性锚
// （复审 #7：SpecID 非空时该 scope 实例集须仍恰为解析实例——模板长出兄弟
// 实例即歧义漂移拒建；显式实例路径不受兄弟实例数限制）。
// RequireOwnerActive 身份门（复审 #3）：显式 owner 网关交办在建单时刻仍
// active——预检后退役不得落入新承诺。读取故障向上传播（不吞——宁拒建单）。
func verifyCreationGateTx(ctx context.Context, q identityExec, gate *mission.CreationGate, ownerAgentID string) error {
	if gate == nil {
		return nil
	}
	if gate.RequireOwnerActive && ownerAgentID != "" {
		if err := verifyOwnerActiveTx(ctx, q, ownerAgentID); err != nil {
			return err
		}
	}
	slotIntent := gate.Slot
	if slotIntent == nil {
		return nil
	}
	var specID string
	err := q.QueryRowContext(ctx,
		`SELECT slot_spec_id FROM slot_instances WHERE slot_instance_id = ?`, slotIntent.InstanceID).Scan(&specID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 实例不在册（%s）", mission.ErrSlotIntentViolated, slotIntent.InstanceID)
	} else if err != nil {
		return fmt.Errorf("sqlite: slot intent instance probe: %w", err)
	}
	if slotIntent.SpecID != "" && specID != slotIntent.SpecID {
		return fmt.Errorf("%w: 实例谱系漂移（预期模板 %s，实例属 %s）", mission.ErrSlotIntentViolated, slotIntent.SpecID, specID)
	}
	var bAgent string
	var bVer int
	err = q.QueryRowContext(ctx,
		`SELECT agent_id, slot_spec_version FROM slot_bindings WHERE slot_instance_id = ? AND status = 'active'`,
		slotIntent.InstanceID).Scan(&bAgent, &bVer)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 实例无 active 任职（%s）", mission.ErrSlotIntentViolated, slotIntent.InstanceID)
	} else if err != nil {
		return fmt.Errorf("sqlite: slot intent binding probe: %w", err)
	}
	if bAgent != slotIntent.AgentID {
		return fmt.Errorf("%w: 任职已换人（预期 %s，现 active %s）", mission.ErrSlotIntentViolated, slotIntent.AgentID, bAgent)
	}
	if err := verifyOwnerActiveTx(ctx, q, slotIntent.AgentID); err != nil {
		return err
	}
	// 模板唯一性锚（复审 #7）：模板引用解析时的"恰一实例"在建单时刻须仍成立
	//（identityExec 无游标面——计数 + 唯一行两次点查等价）。
	if slotIntent.SpecID != "" {
		var cnt int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM slot_instances WHERE slot_spec_id = ?`, slotIntent.SpecID).Scan(&cnt); err != nil {
			return fmt.Errorf("sqlite: slot intent spec instances probe: %w", err)
		}
		if cnt != 1 {
			return fmt.Errorf("%w: 模板实例集已漂移（%s 现有 %d 实例——歧义须显式指实例）", mission.ErrSlotIntentViolated, slotIntent.SpecID, cnt)
		}
		var only string
		if err := q.QueryRowContext(ctx,
			`SELECT slot_instance_id FROM slot_instances WHERE slot_spec_id = ?`, slotIntent.SpecID).Scan(&only); err != nil {
			return fmt.Errorf("sqlite: slot intent spec instance probe: %w", err)
		}
		if only != slotIntent.InstanceID {
			return fmt.Errorf("%w: 模板唯一实例已换（预期 %s，现 %s）", mission.ErrSlotIntentViolated, slotIntent.InstanceID, only)
		}
	}
	var content string
	err = q.QueryRowContext(ctx,
		`SELECT content_json FROM slot_specs WHERE slot_spec_id = ? AND version = ?`, specID, bVer).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 职责谱系缺失（%s v%d）", mission.ErrSlotIntentViolated, specID, bVer)
	} else if err != nil {
		return fmt.Errorf("sqlite: slot intent spec probe: %w", err)
	}
	var specContent slot.SpecContent
	if err := json.Unmarshal([]byte(content), &specContent); err != nil {
		return fmt.Errorf("sqlite: slot intent spec unmarshal: %w", err)
	}
	// 授权档案完整快照（复审 #5：含 workspace_access——漏列使合法 read_write
	// 被当空值拒）。与 GrantByAgent 同构扫描。
	var grant slot.AgentGrant
	var skills, tools, contexts, caps, wsAccess string
	err = q.QueryRowContext(ctx,
		`SELECT skills, tools, context, runtime_capabilities, workspace_access FROM slot_agent_grants WHERE agent_id = ?`,
		slotIntent.AgentID).Scan(&skills, &tools, &contexts, &caps, &wsAccess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: slot intent grant probe: %w", err)
	}
	if err == nil {
		grant.Runtime.WorkspaceAccess = wsAccess
		for _, u := range []struct {
			raw string
			out any
		}{{skills, &grant.Skills}, {tools, &grant.Tools}, {contexts, &grant.Context}, {caps, &grant.Runtime.Capabilities}} {
			if err := json.Unmarshal([]byte(u.raw), u.out); err != nil {
				return fmt.Errorf("sqlite: slot intent grant unmarshal: %w", err)
			}
		}
	}
	if rep := slot.EvaluateReadiness(specContent, grant); !rep.Ready {
		return fmt.Errorf("%w: 授权准入不满足（缺 %s）", mission.ErrSlotIntentViolated, strings.Join(rep.Missing, "、"))
	}
	return nil
}

// verifyBindingGrantTx 任职授权事务内核对（三审 #3：建 Run 时钉版模板要求
// vs 授权档案——与建单门/GrantByAgent 同源完整快照含 workspace_access）。
func verifyBindingGrantTx(ctx context.Context, q identityExec, instanceID string, specVersion int, agentID string) error {
	var specID string
	err := q.QueryRowContext(ctx,
		`SELECT slot_spec_id FROM slot_instances WHERE slot_instance_id = ?`, instanceID).Scan(&specID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：任职实例不在册（%s）", missionrun.ErrOwnerNotActive, instanceID)
	} else if err != nil {
		return fmt.Errorf("sqlite: run grant instance probe: %w", err)
	}
	var content string
	err = q.QueryRowContext(ctx,
		`SELECT content_json FROM slot_specs WHERE slot_spec_id = ? AND version = ?`, specID, specVersion).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：职责谱系缺失（%s v%d）", missionrun.ErrOwnerNotActive, specID, specVersion)
	} else if err != nil {
		return fmt.Errorf("sqlite: run grant spec probe: %w", err)
	}
	var specContent slot.SpecContent
	if err := json.Unmarshal([]byte(content), &specContent); err != nil {
		return fmt.Errorf("sqlite: run grant spec unmarshal: %w", err)
	}
	var grant slot.AgentGrant
	var skills, tools, contexts, caps, wsAccess string
	err = q.QueryRowContext(ctx,
		`SELECT skills, tools, context, runtime_capabilities, workspace_access FROM slot_agent_grants WHERE agent_id = ?`,
		agentID).Scan(&skills, &tools, &contexts, &caps, &wsAccess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: run grant probe: %w", err)
	}
	if err == nil {
		grant.Runtime.WorkspaceAccess = wsAccess
		for _, u := range []struct {
			raw string
			out any
		}{{skills, &grant.Skills}, {tools, &grant.Tools}, {contexts, &grant.Context}, {caps, &grant.Runtime.Capabilities}} {
			if err := json.Unmarshal([]byte(u.raw), u.out); err != nil {
				return fmt.Errorf("sqlite: run grant unmarshal: %w", err)
			}
		}
	}
	if rep := slot.EvaluateReadiness(specContent, grant); !rep.Ready {
		return fmt.Errorf("%w：任职授权不满足（缺 %s）——撤权后旧清单不得执行", missionrun.ErrOwnerNotActive, strings.Join(rep.Missing, "、"))
	}
	return nil
}

// grantByAgentTx 事务内授权档案读取（GrantByAgent 同构——池/事务连接不混用）。
func (s *Store) grantByAgentTx(ctx context.Context, q identityExec, agentID string) (slot.AgentGrant, bool, error) {
	var g slot.AgentGrant
	var skills, tools, contexts, caps, ws string
	err := q.QueryRowContext(ctx,
		`SELECT skills, tools, context, runtime_capabilities, workspace_access FROM slot_agent_grants WHERE agent_id = ?`,
		agentID).Scan(&skills, &tools, &contexts, &caps, &ws)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.AgentGrant{}, false, nil
	}
	if err != nil {
		return slot.AgentGrant{}, false, fmt.Errorf("sqlite: run grant fp probe: %w", err)
	}
	g.Runtime.WorkspaceAccess = ws
	for _, u := range []struct {
		raw string
		out any
	}{{skills, &g.Skills}, {tools, &g.Tools}, {contexts, &g.Context}, {caps, &g.Runtime.Capabilities}} {
		if err := json.Unmarshal([]byte(u.raw), u.out); err != nil {
			return slot.AgentGrant{}, false, fmt.Errorf("sqlite: run grant fp unmarshal: %w", err)
		}
	}
	return g, true, nil
}

// verifyOwnerActiveTx 身份门事务内核对（复审 #3/#4 共用）：在册且 active。
func verifyOwnerActiveTx(ctx context.Context, q identityExec, agentID string) error {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE agent_id = ?`, agentID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 负责人不在册（%s）", mission.ErrOwnerGateViolated, agentID)
	} else if err != nil {
		return fmt.Errorf("sqlite: owner gate probe: %w", err)
	}
	if status != mission.AgentStatusActive {
		return fmt.Errorf("%w: 负责人非 active（%s：%s）", mission.ErrOwnerGateViolated, agentID, status)
	}
	return nil
}

// MissionIDBySource 来源映射查询；未映射 ok=false。
func (s *Store) MissionIDBySource(ctx context.Context, roomID, taskID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT mission_id FROM mission_sources WHERE room_id = ? AND task_id = ?`, roomID, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: mission by source: %w", err)
	}
	return id, true, nil
}

// sourceOf 来源回填（详情与版本行共用）；deleted_at 为来源墓碑（ADR-0033 / 0.6.10）。
func (s *Store) sourceOf(ctx context.Context, missionID string) *mission.RoomSource {
	var roomID, taskID string
	var deletedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id, task_id, deleted_at FROM mission_sources WHERE mission_id = ?`, missionID).Scan(&roomID, &taskID, &deletedAt)
	if err != nil {
		return nil
	}
	src := &mission.RoomSource{RoomID: roomID, TaskID: taskID}
	if deletedAt.Valid {
		src.DeletedAt = deletedAt.String
	}
	return src
}

// MissionByID 单查契约；未知 ok=false。
func (s *Store) MissionByID(ctx context.Context, missionID string) (mission.Mission, bool, error) {
	m, err := scanMission(s.db.QueryRowContext(ctx,
		`SELECT `+missionCols+` FROM missions WHERE mission_id = ?`, missionID))
	if errors.Is(err, sql.ErrNoRows) {
		return mission.Mission{}, false, nil
	}
	if err != nil {
		return mission.Mission{}, false, fmt.Errorf("sqlite: mission by id: %w", err)
	}
	m.RoomSource = s.sourceOf(ctx, missionID)
	return m, true, nil
}

// List 契约摘要（创建序）。
func (s *Store) ListMissions(ctx context.Context) ([]mission.Summary, error) {
	// 0.6.13 审校 #6：摘要携房间来源（任务卡续接识别）——LEFT JOIN 唯一映射。
	rows, err := s.db.QueryContext(ctx, `
SELECT m.mission_id, m.contract_version, m.status, m.owner_agent_id, m.objective, m.created_at, m.updated_at,
       COALESCE(src.room_id, ''), COALESCE(src.task_id, ''), src.deleted_at
  FROM missions m LEFT JOIN mission_sources src ON src.mission_id = m.mission_id
 ORDER BY m.rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: mission list: %w", err)
	}
	defer rows.Close()
	out := []mission.Summary{}
	for rows.Next() {
		var sum mission.Summary
		var roomID, taskID string
		var deleted sql.NullString
		if err := rows.Scan(&sum.MissionID, &sum.ContractVersion, &sum.Status,
			&sum.OwnerAgentID, &sum.Objective, &sum.CreatedAt, &sum.UpdatedAt, &roomID, &taskID, &deleted); err != nil {
			return nil, fmt.Errorf("sqlite: scan mission summary: %w", err)
		}
		if roomID != "" {
			// 复审 #13：摘要与详情同一墓碑事实（删房后两入口状态一致）。
			sum.RoomSource = &mission.RoomSource{RoomID: roomID, TaskID: taskID, DeletedAt: deleted.String}
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// ListMissionsByStatus 契约摘要（创建序，按状态过滤——0.6.6 审校 P2：
// 重启恢复面枚举 canceling 兑现收尾用）。
func (s *Store) ListMissionsByStatus(ctx context.Context, status string) ([]mission.Summary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT mission_id, contract_version, status, owner_agent_id, objective, created_at, updated_at
  FROM missions WHERE status = ? ORDER BY rowid`, status)
	if err != nil {
		return nil, fmt.Errorf("sqlite: mission list by status: %w", err)
	}
	defer rows.Close()
	out := []mission.Summary{}
	for rows.Next() {
		var sum mission.Summary
		if err := rows.Scan(&sum.MissionID, &sum.ContractVersion, &sum.Status,
			&sum.OwnerAgentID, &sum.Objective, &sum.CreatedAt, &sum.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan mission summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// Versions 不可变版本行升序；未知 ok=false。
func (s *Store) MissionVersions(ctx context.Context, missionID string) ([]mission.Mission, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT mission_id, version, status, owner_agent_id, requester, content, created_at
  FROM mission_contract_versions WHERE mission_id = ? ORDER BY version`, missionID)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: mission versions: %w", err)
	}
	defer rows.Close()
	out := []mission.Mission{}
	for rows.Next() {
		var m mission.Mission
		var content string
		if err := rows.Scan(&m.MissionID, &m.ContractVersion, &m.Status,
			&m.OwnerAgentID, &m.Requester, &content, &m.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("sqlite: scan mission version: %w", err)
		}
		if err := json.Unmarshal([]byte(content), &m.Content); err != nil {
			return nil, false, fmt.Errorf("sqlite: mission version unmarshal: %w", err)
		}
		m.UpdatedAt = m.CreatedAt // 版本行时间对齐该版本
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	if rs := s.sourceOf(ctx, missionID); rs != nil {
		for i := range out {
			out[i].RoomSource = rs
		}
	}
	return out, true, nil
}

// TransitionStatus 条件状态投影（0.6.5 / ADR-0028）：状态 == from 才更新
// + status_changed 事件（单事务）；不符/未知/终态幂等无操作 changed=false。
func (s *Store) TransitionStatus(ctx context.Context, missionID, from, to string, event mission.Event) (bool, error) {
	changed := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		changed = false
		var current string
		err := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, missionID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlite: transition probe: %w", err)
		}
		if current != from || current == mission.StatusCanceled {
			return nil
		}
		// 0.6.7：出 waiting 即清等待原因（to = waiting 时保留——调用方序已知）
		setSQL := `UPDATE missions SET status = ?, updated_at = ?, waiting_reason = '' WHERE mission_id = ?`
		if to == mission.StatusWaiting {
			setSQL = `UPDATE missions SET status = ?, updated_at = ? WHERE mission_id = ?`
		}
		res, err := q.ExecContext(ctx, setSQL, to, event.OccurredAt, missionID)
		if err != nil {
			return fmt.Errorf("sqlite: transition update: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: transition event: %w", err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// MarkWaiting 条件进 waiting + 等待原因（0.6.7 / ADR-0030）：当前状态 == from
// 才置 waiting、waiting_reason=reason + 事件（单事务）；不符 = 无操作。
func (s *Store) MarkWaiting(ctx context.Context, missionID, from, reason string, event mission.Event) (bool, error) {
	changed := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		changed = false
		var current string
		err := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, missionID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlite: waiting probe: %w", err)
		}
		if current != from || current == mission.StatusCanceled {
			return nil
		}
		res, err := q.ExecContext(ctx,
			`UPDATE missions SET status = ?, waiting_reason = ?, updated_at = ? WHERE mission_id = ?`,
			mission.StatusWaiting, reason, event.OccurredAt, missionID)
		if err != nil {
			return fmt.Errorf("sqlite: waiting update: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: waiting event: %w", err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// ---- 交付域（RFC-0018 §10.2 / ADR-0031，0.6.8；v21 表）----

const deliveryCols = `package_id, mission_id, run_id, attempt, generation, contract_version, artifact_refs, acceptance_mapping, verification_evidence, open_items, risks, action_receipts, suggested_memory, notes, submitted_at`

func scanPackage(row interface{ Scan(...any) error }) (delivery.Package, error) {
	var p delivery.Package
	var refs, mapping, open, risks, receipts, memory string
	if err := row.Scan(&p.PackageID, &p.MissionID, &p.RunID, &p.Attempt, &p.Generation, &p.ContractVersion,
		&refs, &mapping, &p.VerificationEvidence, &open, &risks, &receipts, &memory, &p.Notes, &p.SubmittedAt); err != nil {
		return delivery.Package{}, err
	}
	for name, dst := range map[string]*[]string{"artifact_refs": &p.ArtifactRefs, "open_items": &p.OpenItems,
		"risks": &p.Risks, "action_receipts": &p.ActionReceipts, "suggested_memory": &p.SuggestedMemory} {
		src := map[string]string{"artifact_refs": refs, "open_items": open, "risks": risks,
			"action_receipts": receipts, "suggested_memory": memory}[name]
		if err := json.Unmarshal([]byte(src), dst); err != nil {
			return delivery.Package{}, fmt.Errorf("sqlite: package %s unmarshal: %w", name, err)
		}
		if *dst == nil {
			*dst = []string{}
		}
	}
	if err := json.Unmarshal([]byte(mapping), &p.AcceptanceMapping); err != nil {
		return delivery.Package{}, fmt.Errorf("sqlite: package acceptance_mapping unmarshal: %w", err)
	}
	if p.AcceptanceMapping == nil {
		p.AcceptanceMapping = []delivery.MappingEntry{}
	}
	return p, nil
}

func jsonList(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlite: package list marshal: %w", err)
	}
	return string(b), nil
}

// CreatePackage 单事务（0.6.8 审校 #1/#2）：复核 missions 行（契约版本 ==
// p.ContractVersion 且非 canceled——端口读与写之间的并发修订/取消在此拒绝）→
// 写包 → 写来源 Run 流回执；任一步失败整体回滚（无"包落了回执丢"）。
func (s *Store) CreatePackage(ctx context.Context, p delivery.Package, receipt delivery.Receipt) error {
	refs, err := jsonList(p.ArtifactRefs)
	if err != nil {
		return err
	}
	mapping, err := jsonList(p.AcceptanceMapping)
	if err != nil {
		return err
	}
	open, err := jsonList(p.OpenItems)
	if err != nil {
		return err
	}
	risks, err := jsonList(p.Risks)
	if err != nil {
		return err
	}
	receipts, err := jsonList(p.ActionReceipts)
	if err != nil {
		return err
	}
	memory, err := jsonList(p.SuggestedMemory)
	if err != nil {
		return err
	}
	receiptPayload, err := runEventPayloadJSON(receipt.Payload)
	if err != nil {
		return err
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		// 事务内权威守卫（审校 #1）
		var version int
		var status string
		err := q.QueryRowContext(ctx,
			`SELECT contract_version, status FROM missions WHERE mission_id = ?`, p.MissionID).Scan(&version, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return delivery.ErrMissionNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: delivery guard probe: %w", err)
		}
		if status == mission.StatusCanceled {
			return fmt.Errorf("%w: 不再受理交付", delivery.ErrMissionCanceled)
		}
		if status == mission.StatusReviewing {
			return fmt.Errorf("%w: 受审包已固定——退回后再补证", delivery.ErrMissionReviewing)
		}
		if status == mission.StatusAccepted {
			return fmt.Errorf("%w: 终态", delivery.ErrMissionAccepted)
		}
		if version != p.ContractVersion {
			return fmt.Errorf("%w: 期望 v%d，当前 v%d", delivery.ErrVersionConflict, p.ContractVersion, version)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO delivery_packages (`+deliveryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.PackageID, p.MissionID, p.RunID, p.Attempt, p.Generation, p.ContractVersion,
			refs, mapping, p.VerificationEvidence, open, risks, receipts, memory, p.Notes, p.SubmittedAt); err != nil {
			return fmt.Errorf("sqlite: insert delivery package: %w", err)
		}
		// 回执同事务（审校 #2）
		if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			receipt.RunID, receipt.EventType, receiptPayload, p.SubmittedAt); err != nil {
			return fmt.Errorf("sqlite: insert delivery receipt: %w", err)
		}
		return nil
	})
}

// ListPackagesByMission 按真实提交时间倒序（0.6.8 审校 #3：submitted_at 为
// RFC3339Nano 小数位可变，字符串序错位——先按 rowid 倒序取出，再按解析时间
// 稳定排序；同刻保持插入倒序）。
func (s *Store) ListPackagesByMission(ctx context.Context, missionID string) ([]delivery.Package, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+`
  FROM delivery_packages WHERE mission_id = ? ORDER BY rowid DESC`, missionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list delivery packages: %w", err)
	}
	defer rows.Close()
	out := []delivery.Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan delivery package: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return delivery.LaterThan(out[i].SubmittedAt, out[j].SubmittedAt)
	})
	return out, nil
}

// PackageByID 单查；未知 ok=false。
func (s *Store) PackageByID(ctx context.Context, packageID string) (delivery.Package, bool, error) {
	p, err := scanPackage(s.db.QueryRowContext(ctx,
		`SELECT `+deliveryCols+` FROM delivery_packages WHERE package_id = ?`, packageID))
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.Package{}, false, nil
	}
	if err != nil {
		return delivery.Package{}, false, fmt.Errorf("sqlite: package by id: %w", err)
	}
	return p, true, nil
}

// RequestReopen 决策重开请求（0.6.11 / ADR-0034）：单事务——CAS 版本（**任意
// 状态**：含 accepted 终态——新证据推翻已验收的决定正是重开典型场景）+
// reopen_requested 事件；不改状态与版本。版本不符 ok=false。
func (s *Store) ReopenRequest(ctx context.Context, missionID string, expectedVersion int, event mission.Event) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var version int
		err := q.QueryRowContext(ctx,
			`SELECT contract_version FROM missions WHERE mission_id = ?`, missionID).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return mission.ErrMissionNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: reopen probe: %w", err)
		}
		if version != expectedVersion {
			return nil
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: reopen event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	out, _, err := s.MissionByID(ctx, missionID)
	return out, true, err
}

// ---- 责任交接（0.6.12 / ADR-0035；v29 pending_owner_agent_id）----

// handoverProbe 交接事务内探测（版本 / 状态 / 归属 / 在途承接人与发起标识）。
func handoverProbe(ctx context.Context, q identityExec, missionID string) (version int, status, owner, pending, offer string, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT contract_version, status, owner_agent_id, pending_owner_agent_id, pending_offer_id FROM missions WHERE mission_id = ?`,
		missionID).Scan(&version, &status, &owner, &pending, &offer)
	if errors.Is(err, sql.ErrNoRows) {
		err = mission.ErrMissionNotFound
	} else if err != nil {
		err = fmt.Errorf("sqlite: handover probe: %w", err)
	}
	return
}

// HandoverOffer 发起交接：版本 + 状态族（HandoverOfferable 单源）+ 无在途 →
// pending 置位 + handover_offered 事件（from_owner 注入**事务内核对后的实际
// 归属**——复审 #2：服务层读后 owner 被并发翻转的交错不进事件），单事务；
// owner / 版本不变。
func (s *Store) HandoverOffer(ctx context.Context, missionID string, expectedVersion int, newOwner, offerID string, event mission.Event) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		version, status, owner, pending, _, err := handoverProbe(ctx, q, missionID)
		if err != nil {
			return err
		}
		if version != expectedVersion || !mission.HandoverOfferable(status) || pending != "" {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE missions SET pending_owner_agent_id = ?, pending_offer_id = ?, updated_at = ? WHERE mission_id = ?`,
			newOwner, offerID, event.OccurredAt, missionID); err != nil {
			return fmt.Errorf("sqlite: handover offer: %w", err)
		}
		event.Payload = mission.InjectHandoverFacts(event.Payload, owner)
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: handover offer event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	out, _, err := s.MissionByID(ctx, missionID)
	return out, true, err
}

// HandoverAccept 承接（承接回执）：版本 + 发起标识锚（审校 #1——撤回改派后的
// 旧 accept 拒）+ 状态族 + **承接人身份事务内核对**（审校 #3a——服务层先读后
// 承接人被退役的交错在此拦：retired/retiring/suspended 不接新承诺）→ owner =
// pending、pending 清空、**draft 按事务内现行 content + 新 owner 重判**
// （复审 #2：HandoverDraftTarget 单源——修订被冻结窗挡住，现行 content 即权威；
// 并发把状态带到 draft（如 paused→resume 重判）时完整任务不再被事务前快照放回
// paused）、其余状态保持（事务内以现行为准）+ handover_accepted 事件（from_owner /
// status_at_accept 注入**事务内核对后的实际值**）+ 状态变化时 status_changed
// reason=handover，单事务。
func (s *Store) HandoverAccept(ctx context.Context, missionID string, expectedVersion int, expectOffer string, event mission.Event) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		version, status, owner, pending, offer, err := handoverProbe(ctx, q, missionID)
		if err != nil {
			return err
		}
		if version != expectedVersion || offer == "" || offer != expectOffer || !mission.HandoverOfferable(status) {
			return nil
		}
		// 审校 #3a：身份事务内核对（同库跨表直查——RequestReview 复核 runs 同例）。
		var tstatus string
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM agents WHERE agent_id = ?`, pending).Scan(&tstatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // 承接人已不存在（offer 后被删——测试面可达）
			}
			return fmt.Errorf("sqlite: handover target probe: %w", err)
		}
		if tstatus != mission.AgentStatusActive {
			return nil // 承接人已非 active（retired/retiring/suspended）——不接新承诺
		}
		newStatus := status
		if status == mission.StatusDraft {
			// 复审 #2：事务内按**现行 content** 重判（服务层/事务前快照不参与）。
			var rawContent string
			if err := q.QueryRowContext(ctx,
				`SELECT content FROM missions WHERE mission_id = ?`, missionID).Scan(&rawContent); err != nil {
				return fmt.Errorf("sqlite: handover accept content: %w", err)
			}
			var c mission.Content
			if err := json.Unmarshal([]byte(rawContent), &c); err != nil {
				return fmt.Errorf("sqlite: handover accept content unmarshal: %w", err)
			}
			newStatus = mission.HandoverDraftTarget(pending, c)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE missions SET owner_agent_id = ?, pending_owner_agent_id = '', pending_offer_id = '', status = ?, updated_at = ? WHERE mission_id = ?`,
			pending, newStatus, event.OccurredAt, missionID); err != nil {
			return fmt.Errorf("sqlite: handover accept: %w", err)
		}
		// 审校 #4 + 复审 #2：回执记**事务内核对后的实际归属与状态**（预填仅占位）。
		event.Payload = mission.InjectHandoverFacts(event.Payload, owner)
		event.Payload = mission.InjectHandoverStatus(event.Payload, newStatus)
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: handover accept event: %w", err)
		}
		if newStatus != status {
			if err := appendMissionEvent(ctx, q, missionID, mission.EventStatusChanged, map[string]string{
				"from": status, "to": newStatus, "reason": "handover",
			}, event.OccurredAt); err != nil {
				return err
			}
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	out, _, err := s.MissionByID(ctx, missionID)
	return out, true, err
}

// HandoverWithdraw 婉拒/撤回共用：版本 + 发起标识锚（审校 #1）→ pending 清空 +
// 事件（declined | revoked 由 event 携带），单事务；任何状态可了结在途交接。
func (s *Store) HandoverWithdraw(ctx context.Context, missionID string, expectedVersion int, expectOffer string, event mission.Event) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		version, _, _, _, offer, err := handoverProbe(ctx, q, missionID)
		if err != nil {
			return err
		}
		if version != expectedVersion || offer == "" || offer != expectOffer {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE missions SET pending_owner_agent_id = '', pending_offer_id = '', updated_at = ? WHERE mission_id = ?`,
			event.OccurredAt, missionID); err != nil {
			return fmt.Errorf("sqlite: handover withdraw: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: handover withdraw event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	out, _, err := s.MissionByID(ctx, missionID)
	return out, true, err
}

// RequestReview 条件送验（0.6.9 / ADR-0032；v0.2 #2/#3）：单事务——版本 + ready
// 守卫 → **事务内复核无在途/未处置 Run**（ErrUnsettledRuns）→ reviewing +
// review_package_id + review_round 单调递增 + 事件。
func (s *Store) RequestReview(ctx context.Context, missionID string, expectedVersion int, packageID string, event mission.Event) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var version int
		var status, pending string
		err := q.QueryRowContext(ctx,
			`SELECT contract_version, status, pending_owner_agent_id FROM missions WHERE mission_id = ?`, missionID).Scan(&version, &status, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return mission.ErrMissionNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: review probe: %w", err)
		}
		// 0.6.12 / ADR-0035 §3：在途交接冻结窗（事务内核对）。
		if version != expectedVersion || status != mission.StatusReady || pending != "" {
			return nil
		}
		// 审校 #2：送验事务内复核未决 Run（在途 / 未处置 unknown）
		var unsettled int
		if err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE mission_id = ? AND
  (state IN ('pending','running') OR (state = 'unknown' AND resolution = ''))`,
			missionID).Scan(&unsettled); err != nil {
			return fmt.Errorf("sqlite: review unsettled probe: %w", err)
		}
		if unsettled > 0 {
			return fmt.Errorf("%w：%d 条未决", mission.ErrUnsettledRuns, unsettled)
		}
		if _, err := q.ExecContext(ctx, `
UPDATE missions SET status = ?, review_package_id = ?, review_round = review_round + 1, updated_at = ?
 WHERE mission_id = ?`,
			mission.StatusReviewing, packageID, event.OccurredAt, missionID); err != nil {
			return fmt.Errorf("sqlite: review update: %w", err)
		}
		// 0.6.10 审校 #1/#5：事务内把**实际**轮次与版本写入送验事件载荷
		//（UPDATE 后回读——同包重审也递增，不用调用方预读值）。
		var newRound int
		if err := q.QueryRowContext(ctx,
			`SELECT review_round FROM missions WHERE mission_id = ?`, missionID).Scan(&newRound); err != nil {
			return fmt.Errorf("sqlite: review round readback: %w", err)
		}
		event.Payload = mission.InjectReviewFacts(event.Payload, version, newRound)
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: review event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	return s.MissionByID(ctx, missionID)
}

// RecordReview 条件裁定（0.6.9；v0.2 #3 轮次守卫 + 复审 R10 包核对）：单事务——
// 版本 + reviewing + **轮次相符** + **受审包相符**（调用方预读的锚——预填未来
// 轮次与"退回 A 送验 B"交错时，锚已换为 B → 拒绝，事件不可能记作 A）四守卫 →
// to（ready 清锚 / accepted 保留锚；轮次单调不复位）+ 事件（按核对后的实际锚）。
// 复审 R1：接受（to=accepted）前复查未决 Run——未核验的执行不能随任务被接受。
func (s *Store) RecordReview(ctx context.Context, missionID string, expectedVersion, expectedRound int, to string, event mission.Event, expectedPackage string) (mission.Mission, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var version, round int
		var status, anchor string
		err := q.QueryRowContext(ctx,
			`SELECT contract_version, status, review_round, review_package_id FROM missions WHERE mission_id = ?`,
			missionID).Scan(&version, &status, &round, &anchor)
		if errors.Is(err, sql.ErrNoRows) {
			return mission.ErrMissionNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: record review probe: %w", err)
		}
		if version != expectedVersion || status != mission.StatusReviewing || round != expectedRound ||
			(expectedPackage != "" && anchor != expectedPackage) {
			return nil
		}
		if to == mission.StatusAccepted {
			var unsettled int
			if err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE mission_id = ? AND
  (state IN ('pending','running') OR (state = 'unknown' AND resolution = ''))`,
				missionID).Scan(&unsettled); err != nil {
				return fmt.Errorf("sqlite: accept unsettled probe: %w", err)
			}
			if unsettled > 0 {
				return fmt.Errorf("%w：%d 条未决", mission.ErrUnsettledRuns, unsettled)
			}
		}
		if to == mission.StatusReady {
			if _, err := q.ExecContext(ctx, `
UPDATE missions SET status = ?, review_package_id = '', updated_at = ? WHERE mission_id = ?`,
				to, event.OccurredAt, missionID); err != nil {
				return fmt.Errorf("sqlite: record review update: %w", err)
			}
		} else {
			if _, err := q.ExecContext(ctx, `
UPDATE missions SET status = ?, updated_at = ? WHERE mission_id = ?`,
				to, event.OccurredAt, missionID); err != nil {
				return fmt.Errorf("sqlite: record review update: %w", err)
			}
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO mission_events (mission_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			missionID, event.EventType, string(event.Payload), event.OccurredAt); err != nil {
			return fmt.Errorf("sqlite: record review event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return mission.Mission{}, ok, err
	}
	return s.MissionByID(ctx, missionID)
}

// ---- 网关首切片域（RFC-0018 §11.2/§11.2.1 / ADR-0024，0.6.1；v14 表）----

// SaveDecision 决策落库。
func (s *Store) SaveDecision(ctx context.Context, d dispatch.Decision) error {
	targets, err := json.Marshal(d.TargetRefs)
	if err != nil {
		return fmt.Errorf("sqlite: decision targets marshal: %w", err)
	}
	req, err := json.Marshal(d.Request)
	if err != nil {
		return fmt.Errorf("sqlite: decision request marshal: %w", err)
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO dispatch_decisions (route_id, decision_version, request_digest, status, selection_source, reason_code, reason_summary, target_refs, request, resolved_agent_id, resolved_instance_id, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.RouteID, d.DecisionVersion, d.RequestDigest, d.Status, d.SelectionSource,
			d.ReasonCode, d.ReasonSummary, string(targets), string(req),
			d.ResolvedAgentID, d.ResolvedInstanceID, d.ExpiresAt, d.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: save decision: %w", err)
		}
		return nil
	})
}

// DecisionByID 单查决策；未知 ok=false。
func (s *Store) DecisionByID(ctx context.Context, routeID string) (dispatch.Decision, bool, error) {
	var d dispatch.Decision
	var targets, req string
	err := s.db.QueryRowContext(ctx, `
SELECT route_id, decision_version, request_digest, status, selection_source, reason_code, reason_summary, target_refs, request, resolved_agent_id, resolved_instance_id, expires_at, created_at
  FROM dispatch_decisions WHERE route_id = ?`, routeID).
		Scan(&d.RouteID, &d.DecisionVersion, &d.RequestDigest, &d.Status, &d.SelectionSource,
			&d.ReasonCode, &d.ReasonSummary, &targets, &req, &d.ResolvedAgentID, &d.ResolvedInstanceID, &d.ExpiresAt, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Decision{}, false, nil
	}
	if err != nil {
		return dispatch.Decision{}, false, fmt.Errorf("sqlite: decision by id: %w", err)
	}
	if err := json.Unmarshal([]byte(targets), &d.TargetRefs); err != nil {
		return dispatch.Decision{}, false, fmt.Errorf("sqlite: decision targets unmarshal: %w", err)
	}
	if err := json.Unmarshal([]byte(req), &d.Request); err != nil {
		return dispatch.Decision{}, false, fmt.Errorf("sqlite: decision request unmarshal: %w", err)
	}
	return d, true, nil
}

// scanReceipt 单行回执扫描（池连接与事务连接共用；ErrNoRows → found=false）。
func scanReceipt(row interface{ Scan(...any) error }) (dispatch.Receipt, bool, error) {
	var r dispatch.Receipt
	var targets, returns string
	err := row.Scan(&r.ReceiptID, &r.CallerRef, &r.Scope, &r.IdempotencyKey, &r.RequestID, &r.RequestDigest,
		&r.RouteID, &r.DecisionVersion, &r.DomainCommandID, &targets, &returns, &r.Reason, &r.CommittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Receipt{}, false, nil
	}
	if err != nil {
		return dispatch.Receipt{}, false, err
	}
	if err := json.Unmarshal([]byte(targets), &r.TargetRefs); err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt targets unmarshal: %w", err)
	}
	if err := json.Unmarshal([]byte(returns), &r.ReturnRefs); err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt returns unmarshal: %w", err)
	}
	return r, true, nil
}

// ClaimKey 原子认领幂等键（审校 P1）：pending 回执行（domain_command_id
// 空）经 (caller, scope, key) 唯一约束一次落库——异摘要冲突在写入前退出
// （零领域副作用）；同摘要已完结 → finalized 重放；pending → 重驱动恢复。
// 事务内读取经同一事务连接（池上限 4 × 持连接等锁死锁教训）。
func (s *Store) ClaimKey(ctx context.Context, r dispatch.Receipt) (dispatch.Receipt, bool, bool, error) {
	targets, err := json.Marshal(r.TargetRefs)
	if err != nil {
		return dispatch.Receipt{}, false, false, fmt.Errorf("sqlite: receipt targets marshal: %w", err)
	}
	var existing dispatch.Receipt
	conflict, finalized := false, false
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		row := q.QueryRowContext(ctx, `
SELECT receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at
  FROM dispatch_receipts WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
			r.CallerRef, r.Scope, r.IdempotencyKey)
		ex, found, err := scanReceipt(row)
		if err != nil {
			return fmt.Errorf("sqlite: claim probe: %w", err)
		}
		if found {
			existing = ex
			if ex.RequestDigest != r.RequestDigest {
				conflict = true
			} else {
				finalized = ex.DomainCommandID != ""
			}
			return nil
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, '[]', ?, '')`,
			r.ReceiptID, r.CallerRef, r.Scope, r.IdempotencyKey, r.RequestID, r.RequestDigest,
			r.RouteID, r.DecisionVersion, string(targets), r.Reason); err != nil {
			return fmt.Errorf("sqlite: claim key: %w", err)
		}
		return nil
	})
	if err != nil {
		return dispatch.Receipt{}, false, false, err
	}
	return existing, conflict, finalized, nil
}

// FinalizeReceipt 完结回执（幂等 UPDATE：填 domain_command_id/committed_at/
// return_refs——认领者创建成功后调用）。
func (s *Store) FinalizeReceipt(ctx context.Context, r dispatch.Receipt) error {
	returns, err := json.Marshal(r.ReturnRefs)
	if err != nil {
		return fmt.Errorf("sqlite: receipt returns marshal: %w", err)
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		targets, err := json.Marshal(r.TargetRefs)
		if err != nil {
			return fmt.Errorf("sqlite: receipt targets marshal: %w", err)
		}
		// 复审 #8：route_id 随完结写真值（pending 认领行的 rd_* 是首认领决策——
		// 换绑拒绝后换路由重投成功的实际决策须落行，重放与按键查询读到同一
		// 份）。upsert 自愈：并发孪生一败释放删行后另一者完结时 UPDATE 落空
		// → 以完结行重建（domain_command_id 非空，公开查询即见正确回执）。
		if _, err := q.ExecContext(ctx, `
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(caller_ref, scope, idempotency_key) DO UPDATE SET
  request_id = excluded.request_id, route_id = excluded.route_id,
  domain_command_id = excluded.domain_command_id, target_refs = excluded.target_refs,
  return_refs = excluded.return_refs, reason = excluded.reason, committed_at = excluded.committed_at`,
			r.ReceiptID, r.CallerRef, r.Scope, r.IdempotencyKey, r.RequestID, r.RequestDigest,
			r.RouteID, r.DecisionVersion, r.DomainCommandID, string(targets), string(returns), r.Reason, r.CommittedAt); err != nil {
			return fmt.Errorf("sqlite: finalize receipt: %w", err)
		}
		return nil
	})
}

// ClaimByKey 认领行查询（含 pending——三审 #6 处置入口只读面）。
func (s *Store) ClaimByKey(ctx context.Context, callerRef, scope, key string) (dispatch.Receipt, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT receipt_id FROM dispatch_receipts WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
		callerRef, scope, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Receipt{}, false, nil
	}
	if err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: claim by key: %w", err)
	}
	return s.receiptByID(ctx, id)
}

// ReleaseClaim 遗留认领处置（四审 #4/#8）：**单一写事务**内判定资格并删除
// ——只有 RowsAffected=1 才算释放；零行按事务内真值分辨（finalized /
// not_found / digest_mismatch / not_releasable）。资格：pending + 同摘要 +
// route_id 空（update 类认领——create 认领 route 非空，在途建单不可经公开
// 入口释放，防同键建两单串回执）。
func (s *Store) ReleaseClaim(ctx context.Context, callerRef, scope, key, digest string) (string, error) {
	outcome := ""
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var ex, route, dg, domain string
		err := q.QueryRowContext(ctx, `
SELECT receipt_id, request_digest, route_id, domain_command_id
  FROM dispatch_receipts WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
			callerRef, scope, key).Scan(&ex, &dg, &route, &domain)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = "not_found"
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlite: release claim probe: %w", err)
		}
		if domain != "" {
			outcome = "finalized"
			return nil
		}
		if dg != digest {
			outcome = "digest_mismatch"
			return nil
		}
		if route != "" {
			outcome = "not_releasable" // create 认领（在途建单）——公开处置不可及
			return nil
		}
		res, err := q.ExecContext(ctx, `
DELETE FROM dispatch_receipts
 WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?
   AND request_digest = ? AND domain_command_id = '' AND route_id = ''`,
			callerRef, scope, key, digest)
		if err != nil {
			return fmt.Errorf("sqlite: release claim delete: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			outcome = "released"
			return nil
		}
		// 判定与删除之间被并发完结/消费——事务内重读分辨
		var d2, rt2 string
		if err := q.QueryRowContext(ctx, `
SELECT domain_command_id, route_id FROM dispatch_receipts
 WHERE caller_ref = ? AND scope = ? AND idempotency_key = ?`,
			callerRef, scope, key).Scan(&d2, &rt2); errors.Is(err, sql.ErrNoRows) {
			outcome = "not_found"
			return nil
		} else if err != nil {
			return fmt.Errorf("sqlite: release claim reprobe: %w", err)
		}
		if d2 != "" {
			outcome = "finalized"
		} else if rt2 != "" {
			outcome = "not_releasable"
		} else {
			outcome = "digest_mismatch"
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// ReleaseKey 回滚未生效命令对键的认领（0.6.13 审校 #11：分步退化面的拒绝
// 补偿——只删同摘要 pending 行，不碰已完结回执；原子面经事务回滚天然满足）。
func (s *Store) ReleaseKey(ctx context.Context, callerRef, scope, key, digest string) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
DELETE FROM dispatch_receipts
 WHERE caller_ref = ? AND scope = ? AND idempotency_key = ? AND request_digest = ? AND domain_command_id = ''`,
			callerRef, scope, key, digest); err != nil {
			return fmt.Errorf("sqlite: release key: %w", err)
		}
		return nil
	})
}

// ReceiptByKey 幂等键查**已提交**回执（0.6.13 审校 #11：pending 认领是事务
// 内部事实，公开查询不可见——防"虚假已提交回执"）；未完结 ok=false。
func (s *Store) ReceiptByKey(ctx context.Context, callerRef, scope, key string) (dispatch.Receipt, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT receipt_id FROM dispatch_receipts WHERE caller_ref = ? AND scope = ? AND idempotency_key = ? AND domain_command_id != ''`,
		callerRef, scope, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Receipt{}, false, nil
	}
	if err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt by key: %w", err)
	}
	return s.receiptByID(ctx, id)
}

func (s *Store) receiptByID(ctx context.Context, receiptID string) (dispatch.Receipt, bool, error) {
	var r dispatch.Receipt
	var targets, returns string
	err := s.db.QueryRowContext(ctx, `
SELECT receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at
  FROM dispatch_receipts WHERE receipt_id = ?`, receiptID).
		Scan(&r.ReceiptID, &r.CallerRef, &r.Scope, &r.IdempotencyKey, &r.RequestID, &r.RequestDigest,
			&r.RouteID, &r.DecisionVersion, &r.DomainCommandID, &targets, &returns, &r.Reason, &r.CommittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Receipt{}, false, nil
	}
	if err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt by id: %w", err)
	}
	if err := json.Unmarshal([]byte(targets), &r.TargetRefs); err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt targets unmarshal: %w", err)
	}
	if err := json.Unmarshal([]byte(returns), &r.ReturnRefs); err != nil {
		return dispatch.Receipt{}, false, fmt.Errorf("sqlite: receipt returns unmarshal: %w", err)
	}
	return r, true, nil
}

// Events 事件流（seq 升序）；未知 ok=false。
func (s *Store) MissionEvents(ctx context.Context, missionID string) ([]mission.Event, bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM missions WHERE mission_id = ?)`, missionID).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("sqlite: mission exists: %w", err)
	}
	if !exists {
		return nil, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, mission_id, event_type, payload, occurred_at
  FROM mission_events WHERE mission_id = ? ORDER BY seq`, missionID)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: mission events: %w", err)
	}
	defer rows.Close()
	out := []mission.Event{}
	for rows.Next() {
		var ev mission.Event
		var payload string
		if err := rows.Scan(&ev.Seq, &ev.MissionID, &ev.EventType, &payload, &ev.OccurredAt); err != nil {
			return nil, false, fmt.Errorf("sqlite: scan mission event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, true, rows.Err()
}

// ---- 职责实例域（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3；v10 表，端口实现）----

// CreateInstance 建职责实例（knowledge_namespace UNIQUE 兜底生成器碰撞）。
func (s *Store) CreateInstance(ctx context.Context, inst slot.SlotInstance) error {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_instances (slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			inst.SlotInstanceID, inst.SlotSpecID, inst.Scope, inst.Title,
			inst.KnowledgeNamespace, inst.CreatedFromVersion, inst.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: create slot instance: %w", err)
		}
		return nil
	})
	return err
}

// ListInstances 实例全量（创建序）。
func (s *Store) ListInstances(ctx context.Context) ([]slot.SlotInstance, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at
  FROM slot_instances ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list slot instances: %w", err)
	}
	defer rows.Close()
	out := []slot.SlotInstance{}
	for rows.Next() {
		var in slot.SlotInstance
		if err := rows.Scan(&in.SlotInstanceID, &in.SlotSpecID, &in.Scope, &in.Title,
			&in.KnowledgeNamespace, &in.CreatedFromVersion, &in.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan slot instance: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// InstanceByID 单查实例；未知 ok=false。
func (s *Store) InstanceByID(ctx context.Context, slotInstanceID string) (slot.SlotInstance, bool, error) {
	var in slot.SlotInstance
	err := s.db.QueryRowContext(ctx, `
SELECT slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at
  FROM slot_instances WHERE slot_instance_id = ?`, slotInstanceID).
		Scan(&in.SlotInstanceID, &in.SlotSpecID, &in.Scope, &in.Title,
			&in.KnowledgeNamespace, &in.CreatedFromVersion, &in.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.SlotInstance{}, false, nil
	}
	if err != nil {
		return slot.SlotInstance{}, false, fmt.Errorf("sqlite: slot instance by id: %w", err)
	}
	return in, true, nil
}

// LatestByLineage 各谱系最新版聚合（实例列表的现势连接单源）。
func (s *Store) LatestByLineage(ctx context.Context) (map[string]slot.InstanceSpecNow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_spec_id, name, version FROM slot_specs
 WHERE version = (SELECT MAX(version) FROM slot_specs t WHERE t.slot_spec_id = slot_specs.slot_spec_id)`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: slot latest by lineage: %w", err)
	}
	defer rows.Close()
	out := map[string]slot.InstanceSpecNow{}
	for rows.Next() {
		var id, name string
		var version int
		if err := rows.Scan(&id, &name, &version); err != nil {
			return nil, fmt.Errorf("sqlite: scan latest by lineage: %w", err)
		}
		out[id] = slot.InstanceSpecNow{Name: name, LatestVersion: version}
	}
	return out, rows.Err()
}

// ---- 任职域（RFC-0018 §3.2/§5.2 / ADR-0021，0.5.4；v11 表，端口实现）----

const bindingCols = `binding_id, agent_id, slot_instance_id, slot_spec_version, scope, status, handoff_note, created_at, updated_at`

func scanBinding(row interface{ Scan(...any) error }) (slot.Binding, error) {
	var b slot.Binding
	if err := row.Scan(&b.BindingID, &b.AgentID, &b.SlotInstanceID, &b.SlotSpecVersion,
		&b.Scope, &b.Status, &b.HandoffNote, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return slot.Binding{}, err
	}
	return b, nil
}

func appendBindingEvent(ctx context.Context, q identityExec, bindingID, eventType string, payload map[string]string, occurredAt string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlite: binding event payload: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
INSERT INTO binding_events (binding_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
		bindingID, eventType, string(raw), occurredAt); err != nil {
		return fmt.Errorf("sqlite: append binding %s: %w", eventType, err)
	}
	return nil
}

// CreateBinding 建任职（单事务，审校 P2#3）：行直接落终态 target，created 与
// preparing→target 两事件同一事务一次提交——中断/失败零残留。
func (s *Store) CreateBinding(ctx context.Context, b slot.Binding, target string, targetPayload map[string]string) (slot.Binding, error) {
	b.Status = target
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_bindings (`+bindingCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.BindingID, b.AgentID, b.SlotInstanceID, b.SlotSpecVersion,
			b.Scope, b.Status, b.HandoffNote, b.CreatedAt, b.UpdatedAt); err != nil {
			return fmt.Errorf("sqlite: create binding: %w", err)
		}
		if err := appendBindingEvent(ctx, q, b.BindingID, slot.EventCreated, map[string]string{
			"agent_id": b.AgentID, "slot_instance_id": b.SlotInstanceID,
			"slot_spec_version": strconv.Itoa(b.SlotSpecVersion),
		}, b.CreatedAt); err != nil {
			return err
		}
		payload := map[string]string{"from": slot.BindingPreparing, "to": target}
		for k, v := range targetPayload {
			payload[k] = v
		}
		return appendBindingEvent(ctx, q, b.BindingID, slot.EventStatusChanged, payload, b.CreatedAt)
	})
	if err != nil {
		return slot.Binding{}, err
	}
	return b, nil
}

// RetryBindingPreparation 单事务重试（审校 P2#3）：blocked 校验 → 两段事件 →
// 终态投影一次提交；非 blocked ok=false。
func (s *Store) RetryBindingPreparation(ctx context.Context, bindingID, target string, targetPayload map[string]string, occurredAt string) (slot.Binding, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var status string
		err := q.QueryRowContext(ctx,
			`SELECT status FROM slot_bindings WHERE binding_id = ?`, bindingID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return slot.ErrBindingNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: retry probe: %w", err)
		}
		if status != slot.BindingBlocked {
			return nil
		}
		if err := appendBindingEvent(ctx, q, bindingID, slot.EventStatusChanged,
			map[string]string{"from": slot.BindingBlocked, "to": slot.BindingPreparing}, occurredAt); err != nil {
			return err
		}
		payload := map[string]string{"from": slot.BindingPreparing, "to": target}
		for k, v := range targetPayload {
			payload[k] = v
		}
		if err := appendBindingEvent(ctx, q, bindingID, slot.EventStatusChanged, payload, occurredAt); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE slot_bindings SET status = ?, updated_at = ? WHERE binding_id = ?`, target, occurredAt, bindingID); err != nil {
			return fmt.Errorf("sqlite: retry project: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return slot.Binding{}, false, err
	}
	if !ok {
		return slot.Binding{}, false, nil
	}
	got, found, err := s.BindingByID(ctx, bindingID)
	if err != nil || !found {
		return slot.Binding{}, false, err
	}
	return got, true, nil
}

// BindingByID 单查任职；未知 ok=false。
func (s *Store) BindingByID(ctx context.Context, bindingID string) (slot.Binding, bool, error) {
	var b slot.Binding
	err := s.db.QueryRowContext(ctx,
		`SELECT `+bindingCols+` FROM slot_bindings WHERE binding_id = ?`, bindingID).
		Scan(&b.BindingID, &b.AgentID, &b.SlotInstanceID, &b.SlotSpecVersion,
			&b.Scope, &b.Status, &b.HandoffNote, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.Binding{}, false, nil
	}
	if err != nil {
		return slot.Binding{}, false, fmt.Errorf("sqlite: binding by id: %w", err)
	}
	return b, true, nil
}

// ListBindings 任职列表（创建序）；instanceID 非空时按实例过滤。
func (s *Store) ListBindings(ctx context.Context, instanceID string) ([]slot.Binding, error) {
	query := `SELECT ` + bindingCols + ` FROM slot_bindings`
	args := []any{}
	if instanceID != "" {
		query += ` WHERE slot_instance_id = ?`
		args = append(args, instanceID)
	}
	query += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list bindings: %w", err)
	}
	defer rows.Close()
	out := []slot.Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TransitionBinding 事务转移（读在 BEGIN IMMEDIATE 临界区内）；激活撞单
// active 部分唯一索引 → ErrActiveBindingExists。
func (s *Store) TransitionBinding(ctx context.Context, bindingID string, allowedFrom []string, to string, extras map[string]string, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx,
			`SELECT status FROM slot_bindings WHERE binding_id = ?`, bindingID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return slot.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: binding probe: %w", err)
		}
		if !allowedFromHas(allowedFrom, from) {
			return nil
		}
		update := `UPDATE slot_bindings SET status = ?, updated_at = ?`
		args := []any{to, occurredAt}
		if note, has := extras["handoff_note"]; has {
			// 审校 P2#4：交接备注与状态/事件同事务入投影（ready 直释等路径）
			update += `, handoff_note = ?`
			args = append(args, note)
		}
		update += ` WHERE binding_id = ?`
		args = append(args, bindingID)
		if _, err := q.ExecContext(ctx, update, args...); err != nil {
			if to == slot.BindingActive && strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return slot.ErrActiveBindingExists // 单 active 部分唯一索引（AT-13）
			}
			return fmt.Errorf("sqlite: binding project: %w", err)
		}
		payload := map[string]string{"from": from, "to": to}
		for k, v := range extras {
			payload[k] = v
		}
		if err := appendBindingEvent(ctx, q, bindingID, slot.EventStatusChanged, payload, occurredAt); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// ReleaseBinding 原子释放：draining/released 两段事件 + 终态投影（含交接
// 备注）同一事务一次提交；ok=false = 非 active。
func (s *Store) ReleaseBinding(ctx context.Context, bindingID string, handoffNote, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx,
			`SELECT status FROM slot_bindings WHERE binding_id = ?`, bindingID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return slot.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: release probe: %w", err)
		}
		if from != slot.BindingActive {
			return nil
		}
		if err := appendBindingEvent(ctx, q, bindingID, slot.EventStatusChanged,
			map[string]string{"from": from, "to": slot.BindingDraining, "handoff_note": handoffNote}, occurredAt); err != nil {
			return err
		}
		if err := appendBindingEvent(ctx, q, bindingID, slot.EventStatusChanged,
			map[string]string{"from": slot.BindingDraining, "to": slot.BindingReleased}, occurredAt); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE slot_bindings SET status = ?, handoff_note = ?, updated_at = ? WHERE binding_id = ?`,
			slot.BindingReleased, handoffNote, occurredAt, bindingID); err != nil {
			return fmt.Errorf("sqlite: release project: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// BindingEvents 任职事件流（seq 升序）。
func (s *Store) BindingEvents(ctx context.Context, bindingID string) ([]slot.BindingEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, binding_id, event_type, payload, occurred_at FROM binding_events WHERE binding_id = ? ORDER BY seq`, bindingID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: binding events: %w", err)
	}
	defer rows.Close()
	out := []slot.BindingEvent{}
	for rows.Next() {
		var ev slot.BindingEvent
		var payload string
		if err := rows.Scan(&ev.Seq, &ev.BindingID, &ev.EventType, &payload, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan binding event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ---- 执行上下文清单域（RFC-0018 §7.3/§6 / ADR-0025，0.6.2；v15 表）----

// execctxScanColumns 清单行统一读取列。
const execctxScanColumns = `manifest_id, mission_id, contract_version, binding_id, agent_id, state, reason_code, digest, policy_version, layers, watermarks, total_bytes, created_at`

// scanExecctxManifest 清单行 → Manifest（层/水位 JSON 反序列化）。
func scanExecctxManifest(row interface{ Scan(...any) error }) (execctx.Manifest, error) {
	var m execctx.Manifest
	var layers, watermarks string
	if err := row.Scan(&m.ManifestID, &m.MissionID, &m.ContractVersion, &m.BindingID, &m.AgentID,
		&m.State, &m.ReasonCode, &m.Digest, &m.PolicyVersion, &layers, &watermarks,
		&m.TotalBytes, &m.CreatedAt); err != nil {
		return execctx.Manifest{}, err
	}
	if err := json.Unmarshal([]byte(layers), &m.Layers); err != nil {
		return execctx.Manifest{}, fmt.Errorf("sqlite: manifest layers unmarshal: %w", err)
	}
	if err := json.Unmarshal([]byte(watermarks), &m.Watermarks); err != nil {
		return execctx.Manifest{}, fmt.Errorf("sqlite: manifest watermarks unmarshal: %w", err)
	}
	return m, nil
}

// scanExecctxReceipt 回执行 → Receipt（账目 JSON 反序列化）。
func scanExecctxReceipt(receiptID, manifestID, digest, policy, accounting string, total int, createdAt string) (execctx.Receipt, error) {
	r := execctx.Receipt{
		ReceiptID: receiptID, ManifestID: manifestID, Digest: digest,
		PolicyVersion: policy, TotalBytes: total, CreatedAt: createdAt,
	}
	if err := json.Unmarshal([]byte(accounting), &r.Accounting); err != nil {
		return execctx.Receipt{}, fmt.Errorf("sqlite: receipt accounting unmarshal: %w", err)
	}
	return r, nil
}

// execctxReceiptByManifestID 按清单取回执；不存在 ok=false（连接来源由
// 调用方决定——写临界区内经 q，读路径经池）。
func (s *Store) execctxReceiptByManifestID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, manifestID string) (execctx.Receipt, bool, error) {
	var receiptID, digest, policy, accounting, createdAt string
	var total int
	err := q.QueryRowContext(ctx, `
SELECT receipt_id, digest, policy_version, accounting, total_bytes, created_at
  FROM exec_context_receipts WHERE manifest_id = ?`, manifestID).
		Scan(&receiptID, &digest, &policy, &accounting, &total, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return execctx.Receipt{}, false, nil
	}
	if err != nil {
		return execctx.Receipt{}, false, fmt.Errorf("sqlite: receipt by manifest: %w", err)
	}
	r, err := scanExecctxReceipt(receiptID, manifestID, digest, policy, accounting, total, createdAt)
	if err != nil {
		return execctx.Receipt{}, false, err
	}
	return r, true, nil
}

// CreateManifest 清单与回执同事务落库；(mission_id, digest) 撞键
// ErrManifestExists（先查后插同 BEGIN IMMEDIATE 临界区——并发孪生恰一
// 落库，败者可凭错误转重放）。
func (s *Store) CreateManifest(ctx context.Context, m execctx.Manifest, r execctx.Receipt) error {
	layers, err := json.Marshal(m.Layers)
	if err != nil {
		return fmt.Errorf("sqlite: manifest layers marshal: %w", err)
	}
	watermarks, err := json.Marshal(m.Watermarks)
	if err != nil {
		return fmt.Errorf("sqlite: manifest watermarks marshal: %w", err)
	}
	accounting, err := json.Marshal(r.Accounting)
	if err != nil {
		return fmt.Errorf("sqlite: receipt accounting marshal: %w", err)
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		// 复审 R5：新建清单与回执的同一事务内核对终态——预读 reviewing 后接受
		// 落地的迟到装配不再落库（既有清单/回执查询照常；mission 行不存在 =
		// 测试播种形态放行，生产任务行恒存在）。
		var mstate string
		if merr := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, m.MissionID).Scan(&mstate); merr == nil {
			if mstate == mission.StatusCanceled {
				return execctx.ErrMissionCanceled
			}
			if mstate == mission.StatusAccepted {
				return execctx.ErrMissionAccepted
			}
		} else if merr != nil && !errors.Is(merr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: manifest mission probe: %w", merr)
		}
		var existing string
		err := q.QueryRowContext(ctx,
			`SELECT manifest_id FROM exec_context_manifests WHERE mission_id = ? AND digest = ?`,
			m.MissionID, m.Digest).Scan(&existing)
		if err == nil {
			return fmt.Errorf("%w: (%s, %s) 已存 %s", execctx.ErrManifestExists, m.MissionID, m.Digest, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: manifest digest probe: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO exec_context_manifests (manifest_id, mission_id, contract_version, binding_id, agent_id, state, reason_code, digest, policy_version, layers, watermarks, total_bytes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ManifestID, m.MissionID, m.ContractVersion, m.BindingID, m.AgentID,
			m.State, m.ReasonCode, m.Digest, m.PolicyVersion, string(layers),
			string(watermarks), m.TotalBytes, m.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: insert manifest: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO exec_context_receipts (receipt_id, manifest_id, digest, policy_version, accounting, total_bytes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ReceiptID, r.ManifestID, r.Digest, r.PolicyVersion,
			string(accounting), r.TotalBytes, r.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: insert receipt: %w", err)
		}
		return nil
	})
}

// ManifestByDigest 按 (mission_id, digest) 取清单与回执；不存在 ok=false。
func (s *Store) ManifestByDigest(ctx context.Context, missionID, digest string) (execctx.Manifest, execctx.Receipt, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+execctxScanColumns+`
  FROM exec_context_manifests WHERE mission_id = ? AND digest = ?`, missionID, digest)
	m, err := scanExecctxManifest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return execctx.Manifest{}, execctx.Receipt{}, false, nil
	}
	if err != nil {
		return execctx.Manifest{}, execctx.Receipt{}, false, fmt.Errorf("sqlite: manifest by digest: %w", err)
	}
	r, ok, err := s.execctxReceiptByManifestID(ctx, s.db, m.ManifestID)
	if err != nil || !ok {
		return execctx.Manifest{}, execctx.Receipt{}, false, err
	}
	return m, r, true, nil
}

// ManifestByID 单查（含回执）；不存在 ok=false。
func (s *Store) ManifestByID(ctx context.Context, manifestID string) (execctx.Manifest, execctx.Receipt, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+execctxScanColumns+`
  FROM exec_context_manifests WHERE manifest_id = ?`, manifestID)
	m, err := scanExecctxManifest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return execctx.Manifest{}, execctx.Receipt{}, false, nil
	}
	if err != nil {
		return execctx.Manifest{}, execctx.Receipt{}, false, fmt.Errorf("sqlite: manifest by id: %w", err)
	}
	r, ok, err := s.execctxReceiptByManifestID(ctx, s.db, m.ManifestID)
	if err != nil || !ok {
		return execctx.Manifest{}, execctx.Receipt{}, false, err
	}
	return m, r, true, nil
}

// ManifestsByMission 装配历史（创建序倒序——rowid 即插入序）。
func (s *Store) ManifestsByMission(ctx context.Context, missionID string) ([]execctx.Manifest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+execctxScanColumns+`
  FROM exec_context_manifests WHERE mission_id = ? ORDER BY rowid DESC`, missionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: manifests by mission: %w", err)
	}
	defer rows.Close()
	out := []execctx.Manifest{}
	for rows.Next() {
		m, err := scanExecctxManifest(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: manifests scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- 本地运行时域（RFC-0018 §3/§6 / ADR-0026，0.6.3；v16 表）----

const runtimeScanColumns = `runtime_id, adapter, host, distro, path, channel, version, login, state, environment_id, last_health_at, last_health_note, discovered_at, updated_at`

func scanRuntimeRow(row interface{ Scan(...any) error }) (mosaicruntime.Runtime, error) {
	var r mosaicruntime.Runtime
	if err := row.Scan(&r.RuntimeID, &r.Adapter, &r.Host, &r.Distro, &r.Path, &r.Channel,
		&r.Version, &r.Login, &r.State, &r.EnvironmentID, &r.LastHealthAt,
		&r.LastHealthNote, &r.DiscoveredAt, &r.UpdatedAt); err != nil {
		return mosaicruntime.Runtime{}, err
	}
	return r, nil
}

func runtimeEventPayloadJSON(payload map[string]any) (string, error) {
	if payload == nil {
		return "{}", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("sqlite: runtime event payload marshal: %w", err)
	}
	return string(b), nil
}

// SyncRuntimes 批量单事务：行按 (adapter,host,distro,path) upsert——新行
// INSERT，既有行只刷新镜像事实（channel/version/login/updated_at——状态/
// 绑定/发现时间是本域事实不被覆盖）。discovered 事件在事务内只为真正
// 插入的行生成（并发首次同步的落败方 upsert 不产生孤儿事件——审校 P2#6：
// 服务层并发构造的新行 ID 未入库，落败方不得留下引用不存在 ID 的事件）。
func (s *Store) SyncRuntimes(ctx context.Context, now string, rows []mosaicruntime.Runtime) (int, int, error) {
	created, refreshed := 0, 0
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		created, refreshed = 0, 0
		for _, r := range rows {
			// 先查存在性再 upsert（与 mission_sources 同型）：INSERT…ON
			// CONFLICT DO UPDATE 的 RowsAffected 不能区分插入/更新。
			var existing string
			perr := q.QueryRowContext(ctx,
				`SELECT runtime_id FROM runtimes WHERE adapter = ? AND host = ? AND distro = ? AND path = ?`,
				r.Adapter, r.Host, r.Distro, r.Path).Scan(&existing)
			if perr != nil && !errors.Is(perr, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: runtime existence probe: %w", perr)
			}
			isNew := perr != nil
			if isNew {
				created++
			} else {
				refreshed++
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO runtimes (runtime_id, adapter, host, distro, path, channel, version, login, state, environment_id, last_health_at, last_health_note, discovered_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (adapter, host, distro, path) DO UPDATE SET
  channel = excluded.channel, version = excluded.version, login = excluded.login,
  updated_at = excluded.updated_at`,
				r.RuntimeID, r.Adapter, r.Host, r.Distro, r.Path, r.Channel,
				r.Version, r.Login, r.State, r.EnvironmentID, r.LastHealthAt,
				r.LastHealthNote, r.DiscoveredAt, r.UpdatedAt); err != nil {
				return fmt.Errorf("sqlite: upsert runtime: %w", err)
			}
			if isNew {
				payload, err := runtimeEventPayloadJSON(map[string]any{"version": r.Version, "login": r.Login})
				if err != nil {
					return err
				}
				if _, err := q.ExecContext(ctx, `
INSERT INTO runtime_events (runtime_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
					r.RuntimeID, mosaicruntime.EventDiscovered, payload, now); err != nil {
					return fmt.Errorf("sqlite: insert runtime event: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return created, refreshed, nil
}

// UpdateRuntime 全行替换 + 事件同事务。
func (s *Store) UpdateRuntime(ctx context.Context, r mosaicruntime.Runtime, events []mosaicruntime.Event) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
UPDATE runtimes SET adapter=?, host=?, distro=?, path=?, channel=?, version=?, login=?, state=?, environment_id=?, last_health_at=?, last_health_note=?, updated_at=?
 WHERE runtime_id = ?`,
			r.Adapter, r.Host, r.Distro, r.Path, r.Channel, r.Version, r.Login,
			r.State, r.EnvironmentID, r.LastHealthAt, r.LastHealthNote, r.UpdatedAt, r.RuntimeID)
		if err != nil {
			return fmt.Errorf("sqlite: update runtime: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("sqlite: update runtime: 行不存在 %s", r.RuntimeID)
		}
		for _, e := range events {
			payload, err := runtimeEventPayloadJSON(e.Payload)
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO runtime_events (runtime_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
				e.RuntimeID, e.EventType, payload, e.OccurredAt); err != nil {
				return fmt.Errorf("sqlite: insert runtime event: %w", err)
			}
		}
		return nil
	})
}

// RuntimeByID 单查；未知 ok=false。
func (s *Store) RuntimeByID(ctx context.Context, runtimeID string) (mosaicruntime.Runtime, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runtimeScanColumns+`
  FROM runtimes WHERE runtime_id = ?`, runtimeID)
	r, err := scanRuntimeRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return mosaicruntime.Runtime{}, false, nil
	}
	if err != nil {
		return mosaicruntime.Runtime{}, false, fmt.Errorf("sqlite: runtime by id: %w", err)
	}
	return r, true, nil
}

// ListRuntimes 全量（rowid 序 = 首次发现序）。
func (s *Store) ListRuntimes(ctx context.Context) ([]mosaicruntime.Runtime, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runtimeScanColumns+`
  FROM runtimes ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list runtimes: %w", err)
	}
	defer rows.Close()
	out := []mosaicruntime.Runtime{}
	for rows.Next() {
		r, err := scanRuntimeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: runtime scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuntimeEvents 事件流（seq 升序；ok 恒 true——存在性由调用方先行核对）。
func (s *Store) RuntimeEvents(ctx context.Context, runtimeID string) ([]mosaicruntime.Event, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, runtime_id, event_type, payload, occurred_at
  FROM runtime_events WHERE runtime_id = ? ORDER BY seq`, runtimeID)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: runtime events: %w", err)
	}
	defer rows.Close()
	out := []mosaicruntime.Event{}
	for rows.Next() {
		var e mosaicruntime.Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.RuntimeID, &e.EventType, &payload, &e.OccurredAt); err != nil {
			return nil, false, fmt.Errorf("sqlite: runtime event scan: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, false, fmt.Errorf("sqlite: runtime event payload unmarshal: %w", err)
		}
		out = append(out, e)
	}
	return out, true, rows.Err()
}

// CreateEnvironment 建环境；目录/规范工作区身份（workspace_key——
// 审校 P2#2 别名排他）撞键 ErrEnvironmentDirTaken。
func (s *Store) CreateEnvironment(ctx context.Context, e mosaicruntime.Environment) error {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO environments (environment_id, name, workspace_dir, workspace_key, is_default, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.EnvironmentID, e.Name, e.WorkspaceDir, e.WorkspaceKey, boolInt(e.IsDefault), e.CreatedAt, e.UpdatedAt); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w：%s", mosaicruntime.ErrEnvironmentDirTaken, e.WorkspaceDir)
			}
			return fmt.Errorf("sqlite: insert environment: %w", err)
		}
		return nil
	})
	return err
}

// EnvironmentByID 单查；未知 ok=false。
func (s *Store) EnvironmentByID(ctx context.Context, environmentID string) (mosaicruntime.Environment, bool, error) {
	var e mosaicruntime.Environment
	var isDefault int
	err := s.db.QueryRowContext(ctx, `
SELECT environment_id, name, workspace_dir, workspace_key, is_default, created_at, updated_at
  FROM environments WHERE environment_id = ?`, environmentID).
		Scan(&e.EnvironmentID, &e.Name, &e.WorkspaceDir, &e.WorkspaceKey, &isDefault, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mosaicruntime.Environment{}, false, nil
	}
	if err != nil {
		return mosaicruntime.Environment{}, false, fmt.Errorf("sqlite: environment by id: %w", err)
	}
	e.IsDefault = isDefault == 1
	return e, true, nil
}

// ListEnvironments 环境列表（创建序；默认在前）。
func (s *Store) ListEnvironments(ctx context.Context) ([]mosaicruntime.Environment, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT environment_id, name, workspace_dir, workspace_key, is_default, created_at, updated_at
  FROM environments ORDER BY is_default DESC, rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list environments: %w", err)
	}
	defer rows.Close()
	out := []mosaicruntime.Environment{}
	for rows.Next() {
		var e mosaicruntime.Environment
		var isDefault int
		if err := rows.Scan(&e.EnvironmentID, &e.Name, &e.WorkspaceDir, &e.WorkspaceKey, &isDefault, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: environment scan: %w", err)
		}
		e.IsDefault = isDefault == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// DefaultEnvironment 默认环境（部分唯一索引保证至多一）。
func (s *Store) DefaultEnvironment(ctx context.Context) (mosaicruntime.Environment, bool, error) {
	var e mosaicruntime.Environment
	var isDefault int
	err := s.db.QueryRowContext(ctx, `
SELECT environment_id, name, workspace_dir, workspace_key, is_default, created_at, updated_at
  FROM environments WHERE is_default = 1`).
		Scan(&e.EnvironmentID, &e.Name, &e.WorkspaceDir, &e.WorkspaceKey, &isDefault, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mosaicruntime.Environment{}, false, nil
	}
	if err != nil {
		return mosaicruntime.Environment{}, false, fmt.Errorf("sqlite: default environment: %w", err)
	}
	e.IsDefault = isDefault == 1
	return e, true, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- 租约面（RFC-0018 §5.2/§10.3.1 / ADR-0027，0.6.4；v17 表）----

const leaseScanColumns = `lease_id, runtime_id, environment_id, workspace_ref, mission_id, generation, fence_token, state, resource_limits, acquired_at, expires_at, released_at, updated_at`

func scanLeaseRow(row interface{ Scan(...any) error }) (mosaicruntime.Lease, error) {
	var l mosaicruntime.Lease
	var limits string
	if err := row.Scan(&l.LeaseID, &l.RuntimeID, &l.EnvironmentID, &l.WorkspaceRef,
		&l.MissionID, &l.Generation, &l.FenceToken, &l.State, &limits,
		&l.AcquiredAt, &l.ExpiresAt, &l.ReleasedAt, &l.UpdatedAt); err != nil {
		return mosaicruntime.Lease{}, err
	}
	if err := json.Unmarshal([]byte(limits), &l.ResourceLimits); err != nil {
		return mosaicruntime.Lease{}, fmt.Errorf("sqlite: lease limits unmarshal: %w", err)
	}
	if l.ResourceLimits == nil {
		l.ResourceLimits = map[string]int{}
	}
	return l, nil
}

// AcquireLease 单事务：互斥判定（活跃租约两查——运行时容量；工作区独占
// 按规范身份 workspace_key 经 JOIN 计——同一物理目录的路径别名不得绕过，
// 审校 P2#2；BEGIN IMMEDIATE 临界区内并发恰一胜者，库级部分唯一索引
// 兜底）→ 代次 = 当代 MAX+1 → 插入。
func (s *Store) AcquireLease(ctx context.Context, lease mosaicruntime.Lease) (mosaicruntime.Lease, error) {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		// 复审 R8：写事务内核对占用主体终态——预读 reviewing 后接受落地的迟到
		// 领取不再落库（终态不占排他资源，另一 ready 任务不被阻塞）。
		var mstate string
		if merr := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, lease.MissionID).Scan(&mstate); merr == nil {
			if mstate == "canceled" {
				return mosaicruntime.ErrMissionCanceled
			}
			if mstate == "accepted" {
				return mosaicruntime.ErrMissionAccepted
			}
		} else if !errors.Is(merr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: lease mission probe: %w", merr)
		}
		var conflictRuntime, conflictEnv string
		err := q.QueryRowContext(ctx,
			`SELECT lease_id FROM runtime_leases WHERE state = 'active' AND runtime_id = ?`,
			lease.RuntimeID).Scan(&conflictRuntime)
		if err == nil {
			return fmt.Errorf("%w：%s（%s）", mosaicruntime.ErrLeaseConflictRuntime, lease.RuntimeID, conflictRuntime)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: lease runtime probe: %w", err)
		}
		// 工作区独占按 workspace_key（空键历史行不参与——服务层同口径）。
		// 两步而非标量子查询：子查询空集得 NULL，等式静默落空（陷阱）。
		var acqKey string
		kerr := q.QueryRowContext(ctx,
			`SELECT workspace_key FROM environments WHERE environment_id = ?`, lease.EnvironmentID).Scan(&acqKey)
		if kerr != nil && !errors.Is(kerr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: lease env key probe: %w", kerr)
		}
		if kerr == nil && acqKey != "" {
			err = q.QueryRowContext(ctx, `
SELECT l.lease_id FROM runtime_leases l
JOIN environments e ON e.environment_id = l.environment_id
WHERE l.state = 'active' AND e.workspace_key = ?`, acqKey).Scan(&conflictEnv)
			if err == nil {
				return fmt.Errorf("%w：%s（%s）", mosaicruntime.ErrLeaseConflictWorkspace, lease.EnvironmentID, conflictEnv)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: lease env probe: %w", err)
			}
		}
		var gen sql.NullInt64
		if err := q.QueryRowContext(ctx,
			`SELECT MAX(generation) FROM runtime_leases WHERE runtime_id = ?`, lease.RuntimeID).Scan(&gen); err != nil {
			return fmt.Errorf("sqlite: lease generation probe: %w", err)
		}
		lease.Generation = int(gen.Int64) + 1
		limits, err := json.Marshal(lease.ResourceLimits)
		if err != nil {
			return fmt.Errorf("sqlite: lease limits marshal: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO runtime_leases (lease_id, runtime_id, environment_id, workspace_ref, mission_id, generation, fence_token, state, resource_limits, acquired_at, expires_at, released_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			lease.LeaseID, lease.RuntimeID, lease.EnvironmentID, lease.WorkspaceRef, lease.MissionID,
			lease.Generation, lease.FenceToken, lease.State, string(limits),
			lease.AcquiredAt, lease.ExpiresAt, lease.ReleasedAt, lease.UpdatedAt); err != nil {
			if isUniqueViolation(err) {
				// 并发兜底：临界区外插入被部分唯一索引拦下——按列归类
				//（环境索引命中 = 工作区独占；否则容量互斥）
				if strings.Contains(err.Error(), "environment_id") {
					return fmt.Errorf("%w：%s 并发领取冲突（工作区）", mosaicruntime.ErrLeaseConflictWorkspace, lease.EnvironmentID)
				}
				return fmt.Errorf("%w：%s 并发领取冲突", mosaicruntime.ErrLeaseConflictRuntime, lease.RuntimeID)
			}
			return fmt.Errorf("sqlite: insert lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return mosaicruntime.Lease{}, err
	}
	return lease, nil
}

// UpdateLease 全行替换（测试播种/惰性直改；续租/释放走条件化的
// RenewLease/ReleaseLease——审校 P1 竞态防护）。
func (s *Store) UpdateLease(ctx context.Context, lease mosaicruntime.Lease) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		limits, err := json.Marshal(lease.ResourceLimits)
		if err != nil {
			return fmt.Errorf("sqlite: lease limits marshal: %w", err)
		}
		res, err := q.ExecContext(ctx, `
UPDATE runtime_leases SET environment_id=?, workspace_ref=?, mission_id=?, generation=?, fence_token=?, state=?, resource_limits=?, expires_at=?, released_at=?, updated_at=?
 WHERE lease_id = ?`,
			lease.EnvironmentID, lease.WorkspaceRef, lease.MissionID, lease.Generation,
			lease.FenceToken, lease.State, string(limits),
			lease.ExpiresAt, lease.ReleasedAt, lease.UpdatedAt, lease.LeaseID)
		if err != nil {
			return fmt.Errorf("sqlite: update lease: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w：%s", mosaicruntime.ErrLeaseNotFound, lease.LeaseID)
		}
		return nil
	})
}

// RenewLease 原子续租（审校 P1）：UPDATE 只改续租字段且以 state='active'
// 为条件——并发到期先落则零行，ErrLeaseInactive（不复活 active、不改原
// 令牌）；成功后在同临界区回读返回。
func (s *Store) RenewLease(ctx context.Context, leaseID, newExpiresAt, now string) (mosaicruntime.Lease, error) {
	var out mosaicruntime.Lease
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
UPDATE runtime_leases SET expires_at = ?, updated_at = ?
 WHERE lease_id = ? AND state = 'active'`, newExpiresAt, now, leaseID)
		if err != nil {
			return fmt.Errorf("sqlite: renew lease: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w：%s（续租时已非活跃）", mosaicruntime.ErrLeaseInactive, leaseID)
		}
		row := q.QueryRowContext(ctx, `SELECT `+leaseScanColumns+`
  FROM runtime_leases WHERE lease_id = ?`, leaseID)
		out, err = scanLeaseRow(row)
		if err != nil {
			return fmt.Errorf("sqlite: renew lease re-read: %w", err)
		}
		return nil
	})
	if err != nil {
		return mosaicruntime.Lease{}, err
	}
	return out, nil
}

// ReleaseLease 原子释放（同型防护）：只改终态字段，条件 state='active'。
func (s *Store) ReleaseLease(ctx context.Context, leaseID, now string) (mosaicruntime.Lease, error) {
	var out mosaicruntime.Lease
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
UPDATE runtime_leases SET state = 'released', released_at = ?, updated_at = ?
 WHERE lease_id = ? AND state = 'active'`, now, now, leaseID)
		if err != nil {
			return fmt.Errorf("sqlite: release lease: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w：%s（释放时已非活跃）", mosaicruntime.ErrLeaseInactive, leaseID)
		}
		row := q.QueryRowContext(ctx, `SELECT `+leaseScanColumns+`
  FROM runtime_leases WHERE lease_id = ?`, leaseID)
		out, err = scanLeaseRow(row)
		if err != nil {
			return fmt.Errorf("sqlite: release lease re-read: %w", err)
		}
		return nil
	})
	if err != nil {
		return mosaicruntime.Lease{}, err
	}
	return out, nil
}

// LeaseByID 单查；未知 ok=false。
func (s *Store) LeaseByID(ctx context.Context, leaseID string) (mosaicruntime.Lease, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+leaseScanColumns+`
  FROM runtime_leases WHERE lease_id = ?`, leaseID)
	l, err := scanLeaseRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return mosaicruntime.Lease{}, false, nil
	}
	if err != nil {
		return mosaicruntime.Lease{}, false, fmt.Errorf("sqlite: lease by id: %w", err)
	}
	return l, true, nil
}

// ListLeases 租约历史（state 过滤可 all；runtimeID 可空；领取序倒序——
// acquired_at 降序，同刻 lease_id 降序兜底）。
func (s *Store) ListLeases(ctx context.Context, state, runtimeID string) ([]mosaicruntime.Lease, error) {
	qry := `SELECT ` + leaseScanColumns + ` FROM runtime_leases WHERE 1=1`
	args := []any{}
	if state != "" && state != mosaicruntime.LeaseStateAll {
		qry += ` AND state = ?`
		args = append(args, state)
	}
	if runtimeID != "" {
		qry += ` AND runtime_id = ?`
		args = append(args, runtimeID)
	}
	qry += ` ORDER BY acquired_at DESC, lease_id DESC`
	rows, err := s.db.QueryContext(ctx, qry, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list leases: %w", err)
	}
	defer rows.Close()
	out := []mosaicruntime.Lease{}
	for rows.Next() {
		l, err := scanLeaseRow(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: lease scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MaxLeaseGeneration 运行时当代代次（无租约 0）。
func (s *Store) MaxLeaseGeneration(ctx context.Context, runtimeID string) (int, error) {
	var gen sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(generation) FROM runtime_leases WHERE runtime_id = ?`, runtimeID).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("sqlite: max lease generation: %w", err)
	}
	return int(gen.Int64), nil
}

// ExpireOverdue 惰性到期：expires_at ≤ now 的活跃租约转 expired。
// 单语句原子判定+更新（审校 P2#3）：比较与转移在同一条 UPDATE 内——
// 无"扫描旧截止时间→期间续租提交→仍被置 expired"的窗口；时间比较用
// SQLite datetime()（ISO8601 变长小数可解析，毫秒截断对 ≥60s 租期无碍）。
func (s *Store) ExpireOverdue(ctx context.Context, now string) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
UPDATE runtime_leases SET state = 'expired', updated_at = ?
 WHERE state = 'active' AND datetime(expires_at) <= datetime(?)`, now, now); err != nil {
			return fmt.Errorf("sqlite: expire overdue: %w", err)
		}
		return nil
	})
}

// ---- Mission 执行域（RFC-0018 §6/§10.2 / ADR-0028，0.6.5；v19 表）----

const runScanColumns = `run_id, mission_id, attempt, lease_id, manifest_id, runtime_id, binding_id, agent_id, state, error_text, result_block, fence_token, generation, requested_at, started_at, finished_at, updated_at, deadline, resolution, resolution_note, resolved_at`

func scanRunRow(row interface{ Scan(...any) error }) (missionrun.Run, error) {
	var r missionrun.Run
	if err := row.Scan(&r.RunID, &r.MissionID, &r.Attempt, &r.LeaseID, &r.ManifestID,
		&r.RuntimeID, &r.BindingID, &r.AgentID, &r.State, &r.ErrorText, &r.ResultBlock,
		&r.FenceToken, &r.Generation, &r.RequestedAt, &r.StartedAt, &r.FinishedAt, &r.UpdatedAt, &r.Deadline,
		&r.Resolution, &r.ResolutionNote, &r.ResolvedAt); err != nil {
		return missionrun.Run{}, err
	}
	return r, nil
}

func runEventPayloadJSON(payload map[string]any) (string, error) {
	if payload == nil {
		return "{}", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("sqlite: run event payload marshal: %w", err)
	}
	return string(b), nil
}

// CreateRun 建行（部分唯一索引兜底每 Mission 至多一未终态——撞键
// ErrRunExists）+ attempt_requested 事件同事务。复审 R1：写事务内核对任务
// 仍是 ready——调度读列表与落 Run 之间送验/暂停/取消落地时，过期调度在此
// 拒绝（ErrMissionNotReady，调用方释放租约）。
func (s *Store) CreateRun(ctx context.Context, r missionrun.Run, maxRuns, manifestContractVersion int, grantFingerprint string) (missionrun.Run, error) {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var existing string
		perr := q.QueryRowContext(ctx,
			`SELECT run_id FROM runs WHERE mission_id = ? AND state IN ('pending', 'running')`,
			r.MissionID).Scan(&existing)
		if perr == nil {
			return fmt.Errorf("%w：%s（%s 在途）", missionrun.ErrRunExists, r.MissionID, existing)
		}
		if !errors.Is(perr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: run unfinished probe: %w", perr)
		}
		// 复审 #4：执行者门建行事务内核对——负责人身份 active（暂停/退役不
		// 自动解除任职；身份行缺席 = 测试播种放行）+ 装配所钉任职仍 active
		// 且未换人（换绑后旧 Binding 不得启动）。不符 → ErrOwnerNotActive，
		// 任务停留 ready。
		if r.AgentID != "" {
			var status string
			if err := q.QueryRowContext(ctx,
				`SELECT status FROM agents WHERE agent_id = ?`, r.AgentID).Scan(&status); err == nil {
				if status != mission.AgentStatusActive {
					return fmt.Errorf("%w：负责人非 active（%s：%s）", missionrun.ErrOwnerNotActive, r.AgentID, status)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: run owner gate probe: %w", err)
			}
		}
		if r.BindingID != "" {
			var bStatus, bAgent, bInstance string
			var bVer int
			if err := q.QueryRowContext(ctx,
				`SELECT status, agent_id, slot_instance_id, slot_spec_version FROM slot_bindings WHERE binding_id = ?`, r.BindingID).
				Scan(&bStatus, &bAgent, &bInstance, &bVer); err == nil {
				if bStatus != "active" || bAgent != r.AgentID {
					return fmt.Errorf("%w：所钉任职失效/换人（bd %s：%s/%s）", missionrun.ErrOwnerNotActive, r.BindingID, bStatus, bAgent)
				}
				// 三审 #3：任职授权在建行事务内核对——清单装配后撤回必需工具/工
				// 作区授权，旧清单不得送达执行器（RFC §11.1 / AT-16 实际执行时
				// 授权仍有效）。复用建单门同源判定（钉版模板要求 vs 授权档案）。
				if err := verifyBindingGrantTx(ctx, q, bInstance, bVer, r.AgentID); err != nil {
					return err
				}
				// 四审 #3：授权快照指纹比对（RFC §11.4 撤权立即阻止新交付）——
				// 可选上下文/工具撤销不改变必需判定，但清单内容（房间原文/工具
				// 清单）已按装配时授权固化；指纹漂移即拒领，重新装配按剩余授权
				// 降级。授权行缺席 = 测试播种容差跳过。
				if grantFingerprint != "" {
					if g, gok, gerr := s.grantByAgentTx(ctx, q, r.AgentID); gerr != nil {
						return gerr
					} else if gok && execctx.GrantFingerprint(g) != grantFingerprint {
						return fmt.Errorf("%w：清单授权快照已漂移（bd %s）——释放租约重新装配", missionrun.ErrGrantDrift, r.BindingID)
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: run binding gate probe: %w", err)
			}
		}
		// 三审 #4：归属核对——交接承接不递增契约版本，清单装配与建行之间的
		// owner 翻转须在此拦（旧负责人的清单不得领取新 Run）。
		{
			var curOwner string
			if err := q.QueryRowContext(ctx,
				`SELECT owner_agent_id FROM missions WHERE mission_id = ?`, r.MissionID).Scan(&curOwner); err == nil {
				if curOwner != r.AgentID {
					return fmt.Errorf("%w：任务归属已交接（执行者 %s，当前负责人 %s）——释放租约丢弃旧装配", missionrun.ErrOwnerNotActive, r.AgentID, curOwner)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: run owner probe: %w", err)
			}
		}
		// 0.6.13 审校 #14 + 复审 #10：次数上限与版本漂移核对在建行事务内
		//（权威面——外层预检与建行分离时并发孪生可双过；计数查询故障中止
		// 不建）。现行契约**在事务内读取**：max_runs 以现行内容为准（RefOf
		// 后修订收紧即时生效）；manifestContractVersion>0 时核对漂移（漂移
		// 释放租约重新装配——预算与清单不得混用两个版本）。任务行缺席 =
		// 测试播种放行（回落调用方上限）。
		authMaxRuns := maxRuns
		if manifestContractVersion > 0 {
			var curVersion int
			var content string
			merr := q.QueryRowContext(ctx,
				`SELECT contract_version, content FROM missions WHERE mission_id = ?`, r.MissionID).
				Scan(&curVersion, &content)
			if merr == nil {
				if curVersion != manifestContractVersion {
					return fmt.Errorf("%w：清单钉 v%d，现行 v%d（%s）", missionrun.ErrContractDrift, manifestContractVersion, curVersion, r.MissionID)
				}
				var cur struct {
					Budget struct {
						MaxRuns int `json:"max_runs"`
					} `json:"budget"`
				}
				if err := json.Unmarshal([]byte(content), &cur); err == nil && cur.Budget.MaxRuns > 0 {
					authMaxRuns = cur.Budget.MaxRuns
				}
			} else if !errors.Is(merr, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: run mission probe: %w", merr)
			}
		}
		if authMaxRuns > 0 {
			var used int
			if err := q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM runs WHERE mission_id = ?`, r.MissionID).Scan(&used); err != nil {
				return fmt.Errorf("sqlite: run budget probe: %w", err)
			}
			if used >= authMaxRuns {
				return fmt.Errorf("%w（%s：%d/%d）", missionrun.ErrBudgetExhausted, r.MissionID, used, authMaxRuns)
			}
		}
		var maxAtt sql.NullInt64
		if err := q.QueryRowContext(ctx,
			`SELECT MAX(attempt) FROM runs WHERE mission_id = ?`, r.MissionID).Scan(&maxAtt); err != nil {
			return fmt.Errorf("sqlite: run attempt probe: %w", err)
		}
		r.Attempt = int(maxAtt.Int64) + 1
		// 复审 R1：任务须仍是 ready | running（过期调度拒绝——送验/暂停/取消/
		// waiting/accepted 落地后不得再起 Run；running 容纳直插播种与恢复形态；
		// mission 行不存在 = 测试播种放行，生产任务行恒存在）
		var mstate string
		if merr := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, r.MissionID).Scan(&mstate); merr == nil && mstate != "ready" && mstate != "running" {
			return fmt.Errorf("%w（当前 %s）——过期调度拒绝", missionrun.ErrMissionNotReady, mstate)
		} else if merr != nil && !errors.Is(merr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: run mission probe: %w", merr)
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO runs (run_id, mission_id, attempt, lease_id, manifest_id, runtime_id, binding_id, agent_id, state, error_text, result_block, fence_token, generation, requested_at, started_at, finished_at, updated_at, deadline, resolution, resolution_note, resolved_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RunID, r.MissionID, r.Attempt, r.LeaseID, r.ManifestID, r.RuntimeID,
			r.BindingID, r.AgentID, r.State, r.ErrorText, r.ResultBlock,
			r.FenceToken, r.Generation, r.RequestedAt, r.StartedAt, r.FinishedAt, r.UpdatedAt, r.Deadline,
			r.Resolution, r.ResolutionNote, r.ResolvedAt); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w：%s", missionrun.ErrRunExists, r.MissionID)
			}
			return fmt.Errorf("sqlite: insert run: %w", err)
		}
		payload, err := runEventPayloadJSON(map[string]any{
			"attempt": r.Attempt, "lease_id": r.LeaseID, "manifest_id": r.ManifestID})
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			r.RunID, missionrun.EventAttemptRequested, payload, r.RequestedAt); err != nil {
			return fmt.Errorf("sqlite: insert run event: %w", err)
		}
		return nil
	})
	if err != nil {
		return missionrun.Run{}, err
	}
	return r, nil
}

// UpdateRunTerminal 终态写入 + **写锁后**停止裁决（四审 #6 + 五审 #4/#5 +
// 既有 P1）：identityWriteTx 取得写锁后在事务内核对——
//
//	a) 行上 deadline 已过（nowFn 锁后取值——等待写锁期间期限越过）；
//	b) Mission 持久停止意图（paused/canceling——人工暂停/取消已生效，存储层
//	   可读的事实，不依赖进程内注册表时序）。
//
// 命中即改按 canceled 落终态（cause=wall_clock_exceeded | pause | canceling），
// **替换**（非追加）原拟提交终态事件——唯一一组 stop_requested + canceled +
// late_result（preview ≤400 rune、携 generation、published=false）；拟发布结果
// 不落 ResultBlock。返回实际落库 Run（调用方据其生成 Mission 投影原因）。
func (s *Store) UpdateRunTerminal(ctx context.Context, r missionrun.Run, events []missionrun.Event, nowFn func() string) (missionrun.Run, error) {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		overrideCause := ""
		if r.Deadline != "" {
			nowRFC := nowFn() // 锁后取值（等待窗口内的期限越过在此判定）
			nowT, perr := time.Parse(time.RFC3339Nano, nowRFC)
			dl, derr := time.Parse(time.RFC3339Nano, r.Deadline)
			if perr == nil && derr == nil && !nowT.Before(dl) {
				overrideCause = missionrun.CauseWallClockExceeded
			}
		}
		if overrideCause == "" && (r.State == missionrun.StateCompleted || r.State == missionrun.StateFailed) {
			// 六审 #2：Run **自身**的持久停止事实（信号送达时写入——resume 不
			// 清除）：暂停→立即恢复的窗口后旧 Run 不得以完成发布正文。
			var sigPayload string
			if err := q.QueryRowContext(ctx,
				`SELECT payload FROM run_events WHERE run_id = ? AND event_type = 'stop_requested' ORDER BY seq DESC LIMIT 1`,
				r.RunID).Scan(&sigPayload); err == nil {
				var parsed struct {
					Cause string `json:"cause"`
				}
				if json.Unmarshal([]byte(sigPayload), &parsed) == nil && parsed.Cause != "" {
					overrideCause = parsed.Cause
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: terminal stop signal probe: %w", err)
			}
		}
		if overrideCause == "" && (r.State == missionrun.StateCompleted || r.State == missionrun.StateFailed) {
			// 人工停止意图（持久化在 mission 行——五审既有 P1：信号已送达但
			// select 抢占失败/锁等待期送达的窗口由此收口）。六审 #3：状态码映射
			// 为公开动作码（paused→pause / canceling→cancel——ADR-0029 约定，
			// 与 SignalStopByMission/finalizeStopped 同组常量）。
			var mStatus string
			if err := q.QueryRowContext(ctx,
				`SELECT status FROM missions WHERE mission_id = ?`, r.MissionID).Scan(&mStatus); err == nil {
				switch mStatus {
				case "paused":
					overrideCause = "pause"
				case "canceling":
					overrideCause = "cancel"
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("sqlite: terminal mission probe: %w", err)
			}
		}
		if overrideCause != "" && (r.State == missionrun.StateCompleted || r.State == missionrun.StateFailed) {
			outcome, preview := "ok", r.ResultBlock
			if r.State == missionrun.StateFailed {
				outcome, preview = "error", r.ErrorText
			}
			r.State = missionrun.StateCanceled
			r.ErrorText = "停止意图在终态写入时判定（" + overrideCause + "）——迟到结果只审计"
			r.ResultBlock = ""
			nowRFC := nowFn()
			r.FinishedAt, r.UpdatedAt = nowRFC, nowRFC
			// 五审 #4/#5 + 七审 #10：替换原拟提交终态事件——改判后的唯一事件组
			//（迟到审计同规范：preview ≤400 rune、携 generation）。stop_requested
			// 恰一条：暂停/取消事务已持久标记时不再追加。
			arbEvents := []missionrun.Event{
				{RunID: r.RunID, EventType: missionrun.EventCanceled, OccurredAt: nowRFC,
					Payload: map[string]any{"cause": overrideCause}},
				{RunID: r.RunID, EventType: missionrun.EventLateResult, OccurredAt: nowRFC,
					Payload: map[string]any{"outcome": outcome, "preview": truncateRunPreview(preview, 400),
						"generation": r.Generation, "published": false, "phase": "terminal_write"}},
			}
			if !hasStopMarkerTx(ctx, q, r.RunID) {
				arbEvents = append([]missionrun.Event{{
					RunID: r.RunID, EventType: missionrun.EventStopRequested, OccurredAt: nowRFC,
					Payload: map[string]any{"cause": overrideCause, "arbitrated_at_write": true},
				}}, arbEvents...)
			}
			events = arbEvents
		}
		if _, err := q.ExecContext(ctx, `
UPDATE runs SET state = ?, error_text = ?, result_block = ?, finished_at = ?, updated_at = ?, deadline = ?
 WHERE run_id = ?`,
			r.State, r.ErrorText, r.ResultBlock, r.FinishedAt, r.UpdatedAt, r.Deadline, r.RunID); err != nil {
			return fmt.Errorf("sqlite: update run terminal: %w", err)
		}
		for _, e := range events {
			payload, err := runEventPayloadJSON(e.Payload)
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
				e.RunID, e.EventType, payload, e.OccurredAt); err != nil {
				return fmt.Errorf("sqlite: insert run event: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return missionrun.Run{}, err
	}
	return r, nil
}

// VerifyGrantFingerprint 交付前授权复核（六审基线：Run 建行后、输入交付前
// 撤权——旧清单不得送达执行器）。指纹为空/授权行缺席（测试容差）恒过；
// 不符 ErrGrantDrift（调用方不交付并重新装配）。
func (s *Store) VerifyGrantFingerprint(ctx context.Context, agentID, bindingID, fingerprint string) error {
	if fingerprint == "" {
		return nil
	}
	return s.identityWriteTx(ctx, func(q identityExec) error {
		g, gok, err := s.grantByAgentTx(ctx, q, agentID)
		if err != nil {
			return err
		}
		if !gok {
			return nil
		}
		if execctx.GrantFingerprint(g) != fingerprint {
			return fmt.Errorf("%w：交付前复核（bd %s）——重新装配", missionrun.ErrGrantDrift, bindingID)
		}
		return nil
	})
}

// hasStopMarkerTx 该 Run 已有持久 stop_requested（七审 #10：终态组去重依据）。
func hasStopMarkerTx(ctx context.Context, q identityExec, runID string) bool {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_events WHERE run_id = ? AND event_type = 'stop_requested'`, runID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// truncateRunPreview 迟到结果预览截断（rune 计 ≤n——中文不半截；五审 #4：
// 与 run 域 truncateS 同规范）。
func truncateRunPreview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ResolveRun 条件处置（0.6.7 / ADR-0030）：state='unknown' AND resolution=” 才落
// 三列 + resolved 事件（单事务）；ok=false = 非 unknown/已处置（服务方重读分辨
// 幂等）；未知 Run → ErrRunNotFound。
func (s *Store) ResolveRun(ctx context.Context, runID, resolution, note, resolvedAt string) (missionrun.Run, bool, error) {
	ok := false
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		var exists int
		var missionID string
		if err := q.QueryRowContext(ctx, `SELECT 1, mission_id FROM runs WHERE run_id = ?`, runID).Scan(&exists, &missionID); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w：%s", missionrun.ErrRunNotFound, runID)
		} else if err != nil {
			return fmt.Errorf("sqlite: resolve probe: %w", err)
		}
		// 复审 R7：事务内终态核对
		var mstate string
		if merr := q.QueryRowContext(ctx,
			`SELECT status FROM missions WHERE mission_id = ?`, missionID).Scan(&mstate); merr == nil {
			if mstate == "accepted" || mstate == "canceled" {
				return fmt.Errorf("%w（Mission %s）", missionrun.ErrMissionTerminal, mstate)
			}
		} else if !errors.Is(merr, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: resolve mission probe: %w", merr)
		}
		res, err := q.ExecContext(ctx, `
UPDATE runs SET resolution = ?, resolution_note = ?, resolved_at = ?, updated_at = ?
 WHERE run_id = ? AND state = ? AND resolution = ''`,
			resolution, note, resolvedAt, resolvedAt, runID, missionrun.StateUnknown)
		if err != nil {
			return fmt.Errorf("sqlite: resolve run: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		payload, err := runEventPayloadJSON(map[string]any{"outcome": resolution, "note": note})
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
			runID, missionrun.EventResolved, payload, resolvedAt); err != nil {
			return fmt.Errorf("sqlite: insert run event: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return missionrun.Run{}, ok, err
	}
	got, found, err := s.RunByID(ctx, runID)
	if err != nil || !found {
		return missionrun.Run{}, false, err
	}
	return got, true, nil
}

// AppendRunEvents 事件追加（不重写行——终态行只增审计）。
func (s *Store) AppendRunEvents(ctx context.Context, runID string, events []missionrun.Event) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE run_id = ?`, runID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w：%s", missionrun.ErrRunNotFound, runID)
		} else if err != nil {
			return fmt.Errorf("sqlite: append probe: %w", err)
		}
		for _, e := range events {
			payload, err := runEventPayloadJSON(e.Payload)
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
				runID, e.EventType, payload, e.OccurredAt); err != nil {
				return fmt.Errorf("sqlite: insert run event: %w", err)
			}
		}
		return nil
	})
}

// ListUnresolvedUnknown 未处置 unknown Run（诊断计数——RFC §11.5）。
// ListExpiredRunning 墙钟过期 running Run（0.6.13 / ADR-0036 §3；审校 #15：
// 期限按**时刻**比较——RFC3339Nano 小数位数不定，字典序 .11Z < .1Z 会漏
// 已过期行/选中未来行；解析失败的病态行保守跳过不误停）。
func (s *Store) ListExpiredRunning(ctx context.Context, now string) ([]missionrun.Run, error) {
	nowT, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list expired running (now): %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runScanColumns+` FROM runs
 WHERE state = 'running' AND deadline != ''`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list expired running: %w", err)
	}
	defer rows.Close()
	var out []missionrun.Run
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		dT, perr := time.Parse(time.RFC3339Nano, r.Deadline)
		if perr != nil || !dT.Before(nowT) {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListUnresolvedUnknown(ctx context.Context) ([]missionrun.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runScanColumns+`
  FROM runs WHERE state = ? AND resolution = '' ORDER BY rowid`, missionrun.StateUnknown)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unresolved unknown: %w", err)
	}
	defer rows.Close()
	out := []missionrun.Run{}
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan unresolved unknown: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRun 全行替换 + 事件同事务。
func (s *Store) UpdateRun(ctx context.Context, r missionrun.Run, events []missionrun.Event) error {
	return s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
UPDATE runs SET mission_id=?, attempt=?, lease_id=?, manifest_id=?, runtime_id=?, binding_id=?, agent_id=?, state=?, error_text=?, result_block=?, fence_token=?, generation=?, requested_at=?, started_at=?, finished_at=?, updated_at=?, deadline=?, resolution=?, resolution_note=?, resolved_at=?
 WHERE run_id = ?`,
			r.MissionID, r.Attempt, r.LeaseID, r.ManifestID, r.RuntimeID, r.BindingID,
			r.AgentID, r.State, r.ErrorText, r.ResultBlock, r.FenceToken, r.Generation,
			r.RequestedAt, r.StartedAt, r.FinishedAt, r.UpdatedAt, r.Deadline,
			r.Resolution, r.ResolutionNote, r.ResolvedAt, r.RunID)
		if err != nil {
			return fmt.Errorf("sqlite: update run: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w：%s", missionrun.ErrRunNotFound, r.RunID)
		}
		for _, e := range events {
			payload, err := runEventPayloadJSON(e.Payload)
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO run_events (run_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
				e.RunID, e.EventType, payload, e.OccurredAt); err != nil {
				return fmt.Errorf("sqlite: insert run event: %w", err)
			}
		}
		return nil
	})
}

// ListLegacyResidues 旧版（0.6.5/0.6.6）崩溃残留终态行（failed + error_text
// 前缀——升级收敛用，0.6.7 发布前补齐）。
func (s *Store) ListLegacyResidues(ctx context.Context, failedPrefix string) ([]missionrun.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runScanColumns+`
  FROM runs WHERE state = ? AND error_text LIKE ?`, missionrun.StateFailed, failedPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("sqlite: list legacy residues: %w", err)
	}
	defer rows.Close()
	out := []missionrun.Run{}
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunByID 单查。
func (s *Store) RunByID(ctx context.Context, runID string) (missionrun.Run, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runScanColumns+`
  FROM runs WHERE run_id = ?`, runID)
	r, err := scanRunRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return missionrun.Run{}, false, nil
	}
	if err != nil {
		return missionrun.Run{}, false, fmt.Errorf("sqlite: run by id: %w", err)
	}
	return r, true, nil
}

// ListRunsByMission attempt 降序（新在前）。
func (s *Store) ListRunsByMission(ctx context.Context, missionID string) ([]missionrun.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runScanColumns+`
  FROM runs WHERE mission_id = ? ORDER BY attempt DESC`, missionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list runs: %w", err)
	}
	defer rows.Close()
	out := []missionrun.Run{}
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: run scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUnfinished 未终态行（恢复面）。
func (s *Store) ListUnfinished(ctx context.Context) ([]missionrun.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runScanColumns+`
  FROM runs WHERE state IN ('pending', 'running') ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unfinished runs: %w", err)
	}
	defer rows.Close()
	out := []missionrun.Run{}
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: run scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunEvents 事件流（seq 升序）。
func (s *Store) RunEvents(ctx context.Context, runID string) ([]missionrun.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, run_id, event_type, payload, occurred_at
  FROM run_events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: run events: %w", err)
	}
	defer rows.Close()
	out := []missionrun.Event{}
	for rows.Next() {
		var e missionrun.Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.RunID, &e.EventType, &payload, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("sqlite: run event scan: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, fmt.Errorf("sqlite: run event payload unmarshal: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NextAttempt 下一 attempt（无 Run → 1）。
func (s *Store) NextAttempt(ctx context.Context, missionID string) (int, error) {
	var maxAtt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(attempt) FROM runs WHERE mission_id = ?`, missionID).Scan(&maxAtt)
	if err != nil {
		return 0, fmt.Errorf("sqlite: next attempt: %w", err)
	}
	return int(maxAtt.Int64) + 1, nil
}
