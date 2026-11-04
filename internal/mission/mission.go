// Package mission Mission 契约域（RFC-0018 §3.1/§3.2/§6.1 / ADR-0023，0.6.0）。
// Mission 是可验收的一次交办——独立事实源，归 Workspace，不挂 Room。
// 契约带版本（不可变版本行，修订即追加）；修订/取消经 expected_version
// CAS（AT-13 并发冲突明确拒绝）；契约状态子集 draft/ready/canceled。
//
// 0.6.6 / ADR-0029 增执行控制态：paused（阻止新调度）与 canceling（停止
// 中间态）——控制命令不递增 contract_version（执行投影，非契约修订）。
package mission

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 契约状态（§3.2 的 0.6.0 子集：执行/验收态随 0.6.5/0.6.9 扩展）。
const (
	StatusDraft    = "draft"    // 契约不完备（验收/来源/交付物缺失或 unassigned）
	StatusReady    = "ready"    // 完备可受领（执行随 0.6.5 起）
	StatusCanceled = "canceled" // 唯一终态；不可修订、不被迟到回调翻转
	// StatusRunning 执行中投影（0.6.5 / ADR-0028）：领取租约创建 Run 后进入；
	// Run 终态回 ready（可再执行）。非契约生命周期状态——不经 Revise/Cancel
	// 的完备性判定，由 run 域经 MarkRunning/MarkReadyFromRunning 投影。
	StatusRunning = "running"
	// StatusPaused 执行控制态（0.6.6 / ADR-0029）：ready | running → paused
	// （阻止新调度——ListReady 只取 ready）；在途 Run 由控制面停止（Run →
	// canceled）；paused → ready（resume——调度器自动拾取）。对 RFC §3.2
	// 的一处增列：ready 可暂停（持久调度器周期派发 ready——"暂停先阻止
	// 新调度"要求 ready 可暂停）。
	StatusPaused = "paused"
	// StatusCanceling 取消中间态（0.6.6 / ADR-0029）：非终态 → canceling
	// （停止意图持久化先行）→ 在途 Run 停止后 → canceled。可长期停留
	// （执行器不响应取消时——"待停止"可见，可续驱，重启恢复兜底）。
	StatusCanceling = "canceling"
	// StatusReviewing 验收中（0.6.9 / ADR-0032；RFC §3.2 running → reviewing →
	// accepted）：request_review 后的验收门——受审包固定（review_package_id）、
	// 契约冻结（revise/pause/submit 拒绝）、不被调度；review_mission 接受
	// （accepted 终态）或退回（ready——可再执行/再交付/再送验）。取消两段照常。
	StatusReviewing = "reviewing"
	// StatusAccepted 已验收（0.6.9）：Mission 首个"完成"终态——修订/取消/暂停/
	// 送验/再交付/执行均拒绝；review_package_id 保留以追溯"按哪个包验收"。
	StatusAccepted = "accepted"
	// StatusWaiting 等待态（0.6.7 / ADR-0030；RFC §3.2 running ↔ waiting）：
	// running → waiting(reason)——本版唯一原因 reconciliation（重启恢复判
	// 为 unknown 的 Run 待核验处置）。出调度列直至处置（waiting → ready|
	// draft 完备性重判）；暂停保留等待原因、恢复据此回 waiting（不绕过
	// 禁止盲目重跑）；取消两段照常；修订保持投影。
	StatusWaiting = "waiting"
)

// WaitingReconciliation 等待原因：有 unknown Run 待核验处置（run 域同名常量
// 同义——域间不互相 import）。
const WaitingReconciliation = "reconciliation"

// 事件类型（mission_events 流）。
const (
	EventCreated         = "created"
	EventContractRevised = "contract_revised"
	EventStatusChanged   = "status_changed"
	// EventReopenRequested 决策重开请求（0.6.11 / ADR-0034）：CAS 版本、任意状态
	// 可追加——不改 Mission 状态、不翻转终态；事实随事件留痕（append-only）。
	EventReopenRequested = "reopen_requested"
	// 责任交接四事实（0.6.12 / ADR-0035）：offered 发起（pending 置位、owner 不变）
	// / accepted 承接回执（owner 翻转）/ declined 婉拒 / revoked 撤回——均不递增
	// contract_version（归属是控制面事实），append-only（交接史不可改写）。
	EventHandoverOffered  = "handover_offered"
	EventHandoverAccepted = "handover_accepted"
	EventHandoverDeclined = "handover_declined"
	EventHandoverRevoked  = "handover_revoked"
)

// AgentStatusActive 承接人须处的身份生命周期状态（身份域同名值——域间不互相
// import；suspended / retiring / retired 不接新承诺，RFC §3.2"retiring 停止接新
// 任务"）。
const AgentStatusActive = "active"

// RequesterOwner 本地单用户请求方（多入口请求方随网关/委派扩展）。
const RequesterOwner = "owner"

// 领域错误。
var (
	// ErrInvalidMission 契约内容不合法（校验单源见 validate）。
	ErrInvalidMission = errors.New("mission: 契约不合法")
	// ErrMissionNotFound Mission 不存在。
	ErrMissionNotFound = errors.New("mission: Mission 不存在")
	// ErrAgentNotFound 负责人 Agent 不存在。
	ErrAgentNotFound = errors.New("mission: Agent 不存在")
	// ErrVersionConflict expected_version 不匹配（CAS 败者——AT-13 可见拒绝，
	// 携回当前版本重试，不静默合并）。
	ErrVersionConflict = errors.New("mission: 契约版本冲突")
	// ErrInvalidTransition 终态拒修订/重复取消。
	ErrInvalidTransition = errors.New("mission: 非法契约转移")
	// ErrInvalidReview 裁定不合法（0.6.9：decision 枚举 / returned 缺 reason /
	// reason 与逐项备注超限）。
	ErrInvalidReview = errors.New("mission: 裁定不合法")
	// ErrReviewNotAuthorized 验收权限不足（0.6.9：契约 reviewer 非空时裁定人
	// 须与之相符——契约一致性校验，非独立身份认证；空 = 请求人自验）。
	ErrReviewNotAuthorized = errors.New("mission: 验收权限不足")
	// ErrUnsettledRuns 存在在途或未处置 unknown 的 Run（0.6.9 审校 #2：送验
	// 事务内复核——待核验完成再放行）。
	ErrUnsettledRuns       = errors.New("mission: 存在未决 Run")
	ErrSourceAlreadyMapped = errors.New("mission: 房间来源已被交办")
	// ErrSlotIntentViolated 职责派发意图复核失败（0.6.13 审校 #2/#7：建单
	// 事务内核对——实例不在册 / active 任职已换人 / 负责人身份非 active /
	// 授权准入不满足；网关映射 route_stale 重新解析，不得用陈旧任职建单）。
	ErrSlotIntentViolated = errors.New("mission: 职责派发意图复核失败（实例/任职/负责人已变化）")
	// ErrOwnerGateViolated 负责人身份门复核失败（复审 #3：建单事务内 owner 非
	// active——预检后退役/暂停不得落入新承诺；网关映射 route_stale 重新解析）。
	ErrOwnerGateViolated = errors.New("mission: 负责人身份门复核失败（非 active）")
	// ErrAgentStatusUnavailable 身份状态查询端口未装配（0.6.12 / ADR-0035：交接
	// 是责任转移的受控通道，承接人须 active——宁拒不放行）。
	ErrAgentStatusUnavailable = errors.New("mission: 未装配身份状态查询（承接人校验不可用）")
	// ErrOwnerMismatch 修订归属锚不符（0.6.12 审校 #2）：责任承接翻转 owner 后
	// （contract_version 不递增），携旧 owner 的修订被拒——旧表单不得无感知撤销
	// 已完成的责任转移；携带方所见 owner 重试。
	ErrOwnerMismatch = errors.New("mission: 归属锚不符（任务负责人已变化）")
)

// Budget 预算（§6.1：并发、总 Run 数、墙钟、重试与委派深度）——0.6.0 为
// 契约声明事实，运行时强制随 0.6.5 Scheduler 落。
type Budget struct {
	Concurrency         int `json:"concurrency"`
	MaxRuns             int `json:"max_runs"`
	MaxWallClockSeconds int `json:"max_wall_clock_seconds"`
	MaxRetries          int `json:"max_retries"`
	MaxDelegationDepth  int `json:"max_delegation_depth"`
}

// DefaultBudget 安全缺省：单 Agent、不重试、不委派、墙钟一天、总 Run 32。
func DefaultBudget() Budget {
	return Budget{Concurrency: 1, MaxRuns: 32, MaxWallClockSeconds: 86400, MaxRetries: 0, MaxDelegationDepth: 0}
}

// DelegationPolicy 委派策略（同 SlotSpec 形状，本地定义不跨包依赖）；
// 缺省 = 不允许委派（安全缺省）。
type DelegationPolicy struct {
	Allowed  []string `json:"allowed"`
	MaxDepth int      `json:"max_depth"`
}

// Content 契约内容（§6.1 的 0.6.0 子集；decision_ref / context_manifest_ref
// 随 0.6.1/0.6.2 增列——对象未存在不落空壳字段）。
type Content struct {
	Objective          string           `json:"objective"`
	Scope              string           `json:"scope"`
	Constraints        []string         `json:"constraints"`
	AcceptanceCriteria []string         `json:"acceptance_criteria"`
	DeliverableSpecs   []string         `json:"deliverable_specs"`
	SourceRefs         []string         `json:"source_refs"`
	ReturnTargets      []string         `json:"return_targets"`
	Budget             Budget           `json:"budget"`
	Delegation         DelegationPolicy `json:"delegation"`
	Reviewer           string           `json:"reviewer"`
}

// Mission 契约投影行（schema: MissionView——内容字段经嵌入平铺）。
type Mission struct {
	MissionID       string `json:"mission_id"`
	ContractVersion int    `json:"contract_version"`
	Status          string `json:"status"`
	WaitingReason   string `json:"waiting_reason"`    // 等待原因（0.6.7；waiting 态一等事实，暂停期间保留；非 waiting 空）
	ReviewPackageID string `json:"review_package_id"` // 受审/已验收包（0.6.9：request_review 写入；accepted 保留、returned 清空）
	ReviewRound     int    `json:"review_round"`      // 送验轮次（0.6.9 审校 #3：每次 request_review 递增——裁定须核对轮次，防旧裁定落到下一轮；非 reviewing/accepted 为 0）
	OwnerAgentID    string `json:"owner_agent_id"`    // 空 = 显式 unassigned（§3.3）
	// PendingOwnerAgentID 在途交接承接人（0.6.12 / ADR-0035）：handover 发起置位、
	// 承接/婉拒/撤回清空；空 = 无在途交接。在途期间 owner 不变（责任不因单方意愿
	// 转移）；Revise / RequestReview / MarkCanceling 冻结（先了结交接）。
	PendingOwnerAgentID string `json:"pending_owner_agent_id"`
	// PendingOfferID 在途交接发起标识（0.6.12 审校 #1：承接/婉拒/撤回请求绑定此锚
	// ——撤回改派后 contract_version 不递增，旧请求须被拒而非处理新交接）。
	PendingOfferID string `json:"pending_offer_id"`
	// SlotInstanceID 职责派发意图（0.6.13 审校 #1：slot 寻址派发的创建事实
	// ——执行装配按该实例固定 Binding（同人双任职不串用），空 = 显式/无职责。
	// 建单时经 CreateMissionSlot 事务内核对（审校 #2：任职/身份/授权漂移拒建）。
	SlotInstanceID string      `json:"slot_instance_id"`
	Requester      string      `json:"requester"`
	RoomSource     *RoomSource `json:"room_source"` // 房间交办来源（非房间交办 null；创建事实）
	Content
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Summary 契约摘要（schema: MissionSummary）。
type Summary struct {
	MissionID       string `json:"mission_id"`
	ContractVersion int    `json:"contract_version"`
	Status          string `json:"status"`
	OwnerAgentID    string `json:"owner_agent_id"`
	Objective       string `json:"objective"`
	// RoomSource 房间交办来源（0.6.13 审校 #6：任务卡续接识别——已映射任务卡
	// 经 /v1/dispatch/update 追加要求；非房间交办 nil）。
	RoomSource *RoomSource `json:"room_source,omitempty"`
	CreatedAt  string      `json:"created_at"`
	UpdatedAt  string      `json:"updated_at"`
}

// Event mission_events 流事件（schema: MissionEvent）。
type Event struct {
	Seq        int64           `json:"seq"`
	MissionID  string          `json:"mission_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	OccurredAt string          `json:"occurred_at"`
}

// AgentLookup 身份域查询端口（owner 是责任归属事实——任意生命周期可指派，
// 行为能力由执行/交接时点校验；创建/修订只需存在性）。
type AgentLookup interface {
	// AgentExists Agent 是否存在。
	AgentExists(ctx context.Context, agentID string) (bool, error)
	// AgentStatus Agent 当前生命周期状态（0.6.12 / ADR-0035：交接受控通道的承接
	// 人须 active——offer 与 accept 两点复核）；未知 ok=false。
	AgentStatus(ctx context.Context, agentID string) (status string, ok bool, err error)
}

// SourceLookup 房间域查询端口（ADR-0024 / 0.6.1）：房间任务卡存在性——
// app 装配注入适配（EventReader 分页读全量事件 → room.TasksOf 纯函数重建）。
type SourceLookup interface {
	// RoomTaskExists 房间任务卡是否存在于当前投影。
	RoomTaskExists(ctx context.Context, roomID, taskID string) (bool, error)
}

// RoomSource 房间交办来源（ADR-0024 / 0.6.1）：(room_id, task_id) 全局唯一
// 映射——映射为创建事实，修订不可改（换来源 = 新交办）。DeletedAt（ADR-0033 /
// 0.6.10）为来源墓碑：房间删除时刻，空串 = 房间仍在——返回目标失效不重跑
// 任务（AT-20），结果与映射保留。
type RoomSource struct {
	RoomID    string `json:"room_id"`
	TaskID    string `json:"task_id"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

// SlotIntent 职责派发意图（0.6.13 审校 #1/#2 / ADR-0036 §2：slot 寻址决策的
// 建单入参——解析所得实例 + 预期任职 Agent。AgentID 必须等于 owner（寻址
// 结果即负责人）；存储事务内四核：实例在册 / active 任职 == AgentID /
// agents.status == active / 授权准入满足（复用 slot 准入门）。不符 →
// ErrSlotIntentViolated（回滚零副作用）。
type SlotIntent struct {
	InstanceID string
	AgentID    string
	// SpecID 原始模板引用（复审 #7：空 = 显式实例路径；非空 = 模板解析路径
	// ——建单事务内须复核该 scope 实例集**仍恰为** InstanceID 一者，模板长出
	// 兄弟实例即歧义漂移，拒绝建单；显式实例路径不受兄弟实例数限制）。
	SpecID string
}

// CreationGate 网关建单事务门（复审 #3/#7）：Slot 职责意图四核（首轮 #1/#2/#7）
// + RequireOwnerActive——网关交办（显式 owner 或寻址所得）要求负责人在建单
// **事务内**仍 active（预检后、建单前退役/暂停不得落入新承诺；域内直建契约
// 不设此门——owner 是归属事实，任意生命周期可指派，行为能力由执行时点校验）。
type CreationGate struct {
	Slot               *SlotIntent
	RequireOwnerActive bool
}

// SameContent 契约内容等值（归一化后逐字段；#11 遗留恢复判定：事务内现行
// 内容与命令载荷等值 + 版本恰为 expected+1 即该命令的修订确已提交）。
func SameContent(a, b Content) bool {
	na, nb := normalizeContent(a), normalizeContent(b)
	ab, bb := func() []byte {
		x, _ := json.Marshal(na)
		return x
	}(), func() []byte {
		y, _ := json.Marshal(nb)
		return y
	}()
	return string(ab) == string(bb)
}

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
// 一切写路径单事务 [校验→投影→版本行→事件]（BEGIN IMMEDIATE 全库约定）。
type Store interface {
	// CreateMission 投影 v1 + 不可变版本行 + created 事件（+ 可选房间来源
	// 映射），单事务；来源撞键 → ErrSourceAlreadyMapped（携已映射 mission_id）。
	CreateMission(ctx context.Context, m Mission, src *RoomSource) (Mission, error)
	// CreateMissionGated 带事务门的建单（0.6.13 审校 #1/#2 + 复审 #3/#7）：
	// SlotInstanceID 随投影落列；gate 的职责意图四核（实例/任职/身份/授权）
	// 与模板唯一性锚、RequireOwnerActive 身份门在**同一写事务**内核对——
	// 换绑/暂停/退役/撤权/模板长出兄弟实例不得插进复核与建单之间；不符 →
	// ErrSlotIntentViolated / ErrOwnerGateViolated 整体回滚。SQLite 实装；
	// memstore 记录意图（无 slot/身份域，仅域级自洽核对）。
	CreateMissionGated(ctx context.Context, m Mission, src *RoomSource, gate *CreationGate) (Mission, error)
	// MissionIDBySource 来源映射查询；未映射 ok=false。
	MissionIDBySource(ctx context.Context, roomID, taskID string) (string, bool, error)
	// Revise CAS 修订：contract_version 匹配、**当前 owner == expectedOwner**
	//（0.6.12 审校 #2 归属锚——承接翻转 owner 不递增版本，旧表单修订不得无感知
	// 撤销责任转移）且非终态才落新版本行 + 投影更新 + contract_revised（及完备性
	// 变化时 status_changed）事件，单事务；ok=false = 版本不匹配/归属锚不符/已终态
	//（服务方重读区分）。
	ReviseMission(ctx context.Context, missionID string, expectedVersion int, expectedOwner string, next Mission) (Mission, bool, error)
	// PauseMission CAS 暂停（0.6.6 / ADR-0029；0.6.7 += waiting——等待原因
	// 保留）：contract_version 匹配且 status ∈ (ready, running, waiting) →
	// paused + status_changed（from/to/reason）
	// 事件，单事务；ok=false = 版本不匹配/状态不合法/未知（服务方重读区分；
	// 幂等由服务层先读实现——存储面只认 ready/running）。
	PauseMission(ctx context.Context, missionID string, expectedVersion int, reason, occurredAt string) (Mission, bool, error)
	// ResumeMission CAS 恢复：contract_version 匹配且 status = paused →
	// target（ready | draft——服务层按当前契约完备性判定，0.6.6 审校 P2：
	// 暂停期间修订删验收后恢复不得直通 ready）+ status_changed 事件，单事务；
	// ok=false 同上。
	ResumeMission(ctx context.Context, missionID string, expectedVersion int, target, occurredAt string) (Mission, bool, error)
	// MarkCanceling CAS 停止意图：contract_version 匹配且非终态 → canceling +
	// status_changed（含 reason）事件，单事务；ok=false = 版本不匹配/终态/
	// 未知（canceling 续驱幂等由服务层先读实现）。
	MarkCanceling(ctx context.Context, missionID string, expectedVersion int, reason, occurredAt string) (Mission, bool, error)
	// FinishCancel 收尾：canceling → canceled + status_changed 事件，单事务
	// （无版本守卫——控制序由服务层持有）；ok=false = 非 canceling/未知。
	FinishCancel(ctx context.Context, missionID, reason, occurredAt string) (Mission, bool, error)
	// MissionByID 单查契约；未知 ok=false。
	MissionByID(ctx context.Context, missionID string) (Mission, bool, error)
	// List 契约摘要（创建序）。
	ListMissions(ctx context.Context) ([]Summary, error)
	// ListMissionsByStatus 契约摘要（创建序，按状态过滤——0.6.6 审校 P2：
	// 重启恢复面枚举 canceling 兑现收尾用）。
	ListMissionsByStatus(ctx context.Context, status string) ([]Summary, error)
	// Versions 不可变版本行升序（每行含该版本完备性事实）；未知 ok=false。
	MissionVersions(ctx context.Context, missionID string) ([]Mission, bool, error)
	// Events 事件流（seq 升序）；未知 ok=false。
	MissionEvents(ctx context.Context, missionID string) ([]Event, bool, error)
	// ReopenRequest 决策重开请求（0.6.11 / ADR-0034）：CAS 版本（任意状态）+
	// reopen_requested 事件，单事务；版本不符 ok=false。不改状态与版本。
	ReopenRequest(ctx context.Context, missionID string, expectedVersion int, event Event) (Mission, bool, error)
	// TransitionStatus 条件状态投影（0.6.5 / ADR-0028）：当前状态 == from
	// 才更新为 to + 事件（单事务）；from 不符/Mission 未知/终态 = 无操作
	// 成功（幂等——调用方序已知）。canceled 拒转。0.6.7：to != waiting 时
	// 清 waiting_reason（出 waiting 即清原因）。
	TransitionStatus(ctx context.Context, missionID, from, to string, event Event) (changed bool, err error)
	// MarkWaiting 条件进 waiting（0.6.7 / ADR-0030）：当前状态 == from 才置
	// waiting + waiting_reason=reason + 事件（单事务）；不符 = 无操作。
	MarkWaiting(ctx context.Context, missionID, from, reason string, event Event) (changed bool, err error)
	// RequestReview 条件送验（0.6.9 / ADR-0032；v0.2 #3：轮次递增）：contract_version
	// 匹配且 status == ready → reviewing + review_package_id + review_round+1 +
	// 事件，单事务；ok=false = 版本不匹配/非 ready。**送验事务内**复核无在途/
	// 未处置 Run（ErrUnsettledRuns——审校 #2）。
	RequestReview(ctx context.Context, missionID string, expectedVersion int, packageID string, event Event) (Mission, bool, error)
	// RecordReview 条件裁定（0.6.9；v0.2 #3 轮次守卫 + 复审 R10 包核对）：
	// contract_version 匹配、status == reviewing、review_round == expectedRound
	// **且受审包 == expectedPackage（调用方预读锚）** → to（accepted 保留锚 /
	// ready 清锚）+ 事件（按核对后的实际锚——预填未来轮次与换包交错时锚已换
	// → 拒绝，事件不可能记作旧包）；accepted 前事务内复查未决 Run（复审 R1）。
	RecordReview(ctx context.Context, missionID string, expectedVersion, expectedRound int, to string, event Event, expectedPackage string) (Mission, bool, error)
	// HandoverOffer 发起交接（0.6.12 / ADR-0035）：contract_version 匹配、状态 ∈
	// draft/ready/running/waiting/paused、无在途交接 → pending_owner = newOwner +
	// pending_offer_id = offerID + handover_offered 事件，单事务；ok=false = 版本
	// 不符/状态族外/已有在途（服务方重读分辨）。owner 与版本不变。
	HandoverOffer(ctx context.Context, missionID string, expectedVersion int, newOwner, offerID string, event Event) (Mission, bool, error)
	// HandoverAccept 承接（承接回执）：contract_version 匹配、在途发起标识 ==
	// expectOffer（锚——审校 #1：撤回改派后旧 accept 拒绝）且非空、状态族同上 →
	// owner = pending、pending 清空、**draft 按事务内现行 content + 新 owner 重判**
	//（HandoverDraftTarget 单源——复审 #2：事务前快照不参与事实判定）、其余状态
	// 保持（执行控制投影事务内以现行为准）+ handover_accepted 事件（回执
	// from_owner / status_at_accept 由**事务内核对后的实际值**注入）+ 状态变化时
	// status_changed reason=handover，单事务；ok=false 同上。
	HandoverAccept(ctx context.Context, missionID string, expectedVersion int, expectOffer string, event Event) (Mission, bool, error)
	// HandoverWithdraw 婉拒/撤回共用：contract_version 匹配、在途发起标识 ==
	// expectOffer 且非空 → pending 清空 + 事件（declined | revoked 由 event 携带），
	// 单事务；任何状态可了结在途交接（不应存在卡死路径）。
	HandoverWithdraw(ctx context.Context, missionID string, expectedVersion int, expectOffer string, event Event) (Mission, bool, error)
}

// Service 契约服务：校验单源 + 完备性判定单源 + CAS 编排。
type Service struct {
	store        Store
	now          func() string
	newMissionID func() string
	newOfferID   func() string // 交接发起标识（0.6.12 审校 #1：hdo_ 前缀——承接/婉拒/撤回的锚）
	agentLookup  AgentLookup   // nil = owner 存在性校验不可用（创建/修订拒带 owner）
	sourceLookup SourceLookup  // nil = 房间来源校验不可用（创建拒带 room_source）
}

// Option 装配选项（测试注入确定性时钟/ID 生成器）。
type Option func(*Service)

// WithNow 注入时钟（缺省 RFC3339Nano UTC）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewMissionID 注入 ID 生成器（缺省：mi_ + 时间有序 + 随机后缀）。
func WithNewMissionID(f func() string) Option { return func(s *Service) { s.newMissionID = f } }

// WithNewOfferID 注入交接发起标识生成器（缺省：hdo_ + 时间有序 + 随机后缀；
// 0.6.12 审校 #1——同一时刻至多一条在途交接，id 在发起时生成并随事件回链）。
func WithNewOfferID(f func() string) Option { return func(s *Service) { s.newOfferID = f } }

// WithAgentLookup 注入身份域查询端口。
func WithAgentLookup(l AgentLookup) Option { return func(s *Service) { s.agentLookup = l } }

// WithSourceLookup 注入房间域来源查询端口（ADR-0024 / 0.6.1）。
func WithSourceLookup(l SourceLookup) Option { return func(s *Service) { s.sourceLookup = l } }

// New 构造服务。
func New(store Store, opts ...Option) *Service {
	s := &Service{
		store:        store,
		now:          func() string { return nowUTC() },
		newMissionID: newMissionID,
		newOfferID:   newOfferID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newMissionID 缺省：mi_ + 时间有序毫秒（36 进制）+ 随机后缀。
func newMissionID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "mi_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// newOfferID 缺省：hdo_ + 时间有序毫秒（36 进制）+ 随机后缀。
func newOfferID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "hdo_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Create 建契约（= create_mission，§11.2）：校验 → owner/来源存在性 →
// 完备性判定（draft/ready）→ 投影 v1 + 版本行 + created 事件（+ 可选来源
// 映射）单事务。CreateWithSlot 的无职责路径（显式/缺省 owner）。
func (s *Service) Create(ctx context.Context, ownerAgentID string, c Content, src *RoomSource) (Mission, error) {
	return s.CreateGated(ctx, ownerAgentID, c, src, nil)
}

// CreateGated 带事务门建契约（0.6.13 审校 #1/#2 + 复审 #3/#7：网关派发
// 路径）——门条件经存储事务内核对后随创建事实落 SlotInstanceID。
func (s *Service) CreateGated(ctx context.Context, ownerAgentID string, c Content, src *RoomSource, gate *CreationGate) (Mission, error) {
	if err := ValidateContent(c); err != nil {
		return Mission{}, err
	}
	if gate != nil && gate.Slot != nil {
		slot := gate.Slot
		// 域级前置：意图自洽（预期任职 = 负责人；寻址结果即 owner）。任职/
		// 身份/授权/模板唯一性的时点核对在存储写事务内（漂移即回滚）。
		if slot.InstanceID == "" || slot.AgentID == "" || slot.AgentID != strings.TrimSpace(ownerAgentID) {
			return Mission{}, fmt.Errorf("%w: 意图自洽性（instance=%q agent=%q owner=%q）", ErrSlotIntentViolated, slot.InstanceID, slot.AgentID, ownerAgentID)
		}
	}
	if ownerAgentID != "" {
		if err := s.checkOwner(ctx, ownerAgentID); err != nil {
			return Mission{}, err
		}
	}
	if src != nil {
		if err := s.checkSource(ctx, src); err != nil {
			return Mission{}, err
		}
	}
	now := s.now()
	m := Mission{
		MissionID:       s.newMissionID(),
		ContractVersion: 1,
		Status:          StatusDraft,
		OwnerAgentID:    strings.TrimSpace(ownerAgentID),
		Requester:       RequesterOwner,
		Content:         normalizeContent(c),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if statusOf(m) == StatusReady {
		m.Status = StatusReady
	}
	var out Mission
	var err error
	if gate != nil && gate.Slot != nil {
		m.SlotInstanceID = gate.Slot.InstanceID
		out, err = s.store.CreateMissionGated(ctx, m, src, gate)
	} else if gate != nil && gate.RequireOwnerActive {
		out, err = s.store.CreateMissionGated(ctx, m, src, gate)
	} else {
		out, err = s.store.CreateMission(ctx, m, src)
	}
	if err != nil {
		return Mission{}, err
	}
	if src != nil {
		roomSrc := *src
		out.RoomSource = &roomSrc
	}
	return out, nil
}

// checkSource 房间来源校验（ADR-0024）：task_id 形如 tsk_*；房间任务卡须
// 存在于当前投影（SourceLookup 未装配时带来源的请求不可用——宁拒不放行）。
func (s *Service) checkSource(ctx context.Context, src *RoomSource) error {
	if !taskIDPattern.MatchString(src.TaskID) || src.RoomID == "" || len(src.RoomID) > 64 {
		return fmt.Errorf("%w: room_source 须为有效房间与 tsk_ 任务卡", ErrInvalidMission)
	}
	if s.sourceLookup == nil {
		return fmt.Errorf("mission: 未装配 SourceLookup（房间来源校验不可用）")
	}
	ok, err := s.sourceLookup.RoomTaskExists(ctx, src.RoomID, src.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 房间任务卡不存在（%s / %s）", ErrInvalidMission, src.RoomID, src.TaskID)
	}
	return nil
}

// InjectReviewFacts 送验事件载荷补事实字段（0.6.10 审校 #1/#5）：存储事务内
// 核对后的**实际**契约版本与送验轮次——回房投影按事件还原当时契约与当轮，
// 不再读 missions 当前行（v1 退回后修订 v2 的重放不得把旧包记到 v2）。
// 旧事件（无该字段）由投影按事件流重建，两路同一结果。
func InjectReviewFacts(payload []byte, contractVersion, reviewRound int) []byte {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		m = map[string]any{}
	}
	m["contract_version"] = contractVersion
	m["review_round"] = reviewRound
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// InjectHandoverFacts 交接事件载荷注入**事务内核对后的实际归属**（复审 #2：服务
// 层读后 owner 被并发翻转的交错不得进事件——from_owner 记落地时点的真实负责人，
// 与行内 owner_agent_id 同源）。
func InjectHandoverFacts(payload []byte, actualOwner string) []byte {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		m = map[string]any{}
	}
	m["from_owner"] = actualOwner
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// HandoverDraftTarget draft 承接后的完备性重判单源（复审 #2：存储事务内按**现行
// content** 与新 owner 计算——statusOf 同一判定；服务层快照不参与）。齐备 →
// ready（交接同时是存量 unassigned 任务的指派通道），否则保持 draft。
func HandoverDraftTarget(ownerAgentID string, c Content) string {
	return statusOf(Mission{OwnerAgentID: ownerAgentID, Content: c})
}

// InjectHandoverStatus 承接回执载荷注入**事务内核对后的实际任务状态**（0.6.12
// 审校 #4——InjectReviewFacts 同例）：服务层预填的 status_at_accept 只是占位，
// 存储事务内 UPDATE 后回读实际状态再落事件——并发暂停/恢复交错不进回执。
func InjectHandoverStatus(payload []byte, actualStatus string) []byte {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		m = map[string]any{}
	}
	m["status_at_accept"] = actualStatus
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// blankTrim 判空去首尾（Unicode White_Space ∪ U+FEFF——与 SPA blankTrim 同集，
// 0.6.9 审校 #1 / 复审 R6 口径）。
func blankTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
}

// ReopenRequest 决策重开请求（0.6.11 / ADR-0034）：CAS 版本（任意状态——含
// accepted 终态：新证据推翻已验收的决定正是重开典型场景）+ reopen_requested
// 事件；Mission 状态与版本不变。形状在域口校验（decision_ref / 证据 / 受影响
// 项 / reason 上限），版本不符 ErrVersionConflict。
func (s *Service) ReopenRequest(ctx context.Context, missionID string, expectedVersion int,
	requestID, decisionRef string, evidence, affected []string, reason string) (Mission, error) {
	decisionRef = blankTrim(decisionRef)
	if n := utf8.RuneCountInString(decisionRef); n < 1 || n > 256 {
		return Mission{}, fmt.Errorf("%w: decision_ref 须 1..256 字", ErrInvalidMission)
	}
	if len(evidence) < 1 || len(evidence) > 8 {
		return Mission{}, fmt.Errorf("%w: evidence_refs 须 1..8 条", ErrInvalidMission)
	}
	for i, e := range evidence {
		if n := utf8.RuneCountInString(blankTrim(e)); n < 1 || n > 256 {
			return Mission{}, fmt.Errorf("%w: evidence_refs[%d] 须 1..256 字", ErrInvalidMission, i)
		}
	}
	if len(affected) > 16 {
		return Mission{}, fmt.Errorf("%w: affected_mission_ids ≤16", ErrInvalidMission)
	}
	if utf8.RuneCountInString(reason) > 280 {
		return Mission{}, fmt.Errorf("%w: reason ≤280 字", ErrInvalidMission)
	}
	payload, err := json.Marshal(map[string]any{
		"request_id": requestID, "decision_ref": decisionRef,
		"evidence_refs": evidence, "affected_mission_ids": affected,
		"reason": reason,
		// 0.6.10 审校 #1 同例：CAS 后的实际契约版本随事实写入（回房投影不读当前行）
		"contract_version": expectedVersion,
	})
	if err != nil {
		return Mission{}, fmt.Errorf("mission: 重开事件构造失败：%w", err)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	out, ok2, err := s.store.ReopenRequest(ctx, missionID, expectedVersion, Event{
		MissionID: missionID, EventType: EventReopenRequested, Payload: payload, OccurredAt: s.now(),
	})
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	// 二审 #2：事务已提交——后续读取（如 MissionByID 断线）失败不丢事实：
	// 返回零值 Mission（调用方 reopen 不依赖此值，只看 err==nil 即继续暂停）。
	return out, nil
}

// ---- 责任交接（0.6.12 / ADR-0035；RFC-0018 §3.3/§4.5/§8.1，AT-03）----

// HandoverFact 交接事实读面（事件流组装；Kind=accepted 即承接回执）。
type HandoverFact struct {
	Kind             string `json:"kind"`     // offered | accepted | declined | revoked
	OfferID          string `json:"offer_id"` // 发起标识回链（同一发起的 offered→accepted/declined/revoked 同 id）
	FromOwnerAgentID string `json:"from_owner_agent_id"`
	ToOwnerAgentID   string `json:"to_owner_agent_id"`
	Notes            string `json:"notes,omitempty"`            // offered
	Note             string `json:"note,omitempty"`             // accepted
	Reason           string `json:"reason,omitempty"`           // declined / revoked
	StatusAtAccept   string `json:"status_at_accept,omitempty"` // accepted：B 接手时的任务状态
	ContractVersion  int    `json:"contract_version"`
	OccurredAt       string `json:"occurred_at"`
}

// HandoverOfferable 可发起/承接交接的状态族：draft / ready / running / waiting /
// paused。拒绝：accepted / canceled（终态——责任已随裁定/取消了结）、reviewing
// （验收基线冻结，ADR-0032 先例）、canceling（停止瞬态——先收尾）。导出供存储
// 事务内核对共用同一单源。
func HandoverOfferable(status string) bool {
	switch status {
	case StatusDraft, StatusReady, StatusRunning, StatusWaiting, StatusPaused:
		return true
	}
	return false
}

// checkHandoverTarget 承接人校验：存在（ErrAgentNotFound）且 active
// （ErrInvalidTransition——suspended / retiring / retired 不接新承诺）；端口未装配
// ErrAgentStatusUnavailable（宁拒不放行）。与 checkOwner（存在性即可——契约
// 事实记录）区分：交接是责任转移的受控通道，承接方必须能履职。
func (s *Service) checkHandoverTarget(ctx context.Context, agentID string) error {
	if s.agentLookup == nil {
		return ErrAgentStatusUnavailable
	}
	st, ok, err := s.agentLookup.AgentStatus(ctx, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAgentNotFound
	}
	if st != AgentStatusActive {
		return fmt.Errorf("%w: 承接人 %s 非 active（%s）——不接新承诺", ErrInvalidTransition, agentID, st)
	}
	return nil
}

// HandoverOffer 发起交接（交接第一段）：CAS 版本 → 状态族 → 在途唯一 → 承接人
// ≠ 当前 owner → 承接人存在且 active → pending 置位 + handover_offered 事件。
// owner 不变（责任不因单方意愿转移）、contract_version 不递增（控制面事实）。
func (s *Service) HandoverOffer(ctx context.Context, missionID string, expectedVersion int, newOwner, notes string) (Mission, error) {
	newOwner = strings.TrimSpace(newOwner)
	if n := utf8.RuneCountInString(newOwner); n < 1 || n > 128 {
		return Mission{}, fmt.Errorf("%w: new_owner_agent_id 须 1..128 字", ErrInvalidMission)
	}
	if utf8.RuneCountInString(notes) > 2000 {
		return Mission{}, fmt.Errorf("%w: notes ≤2000 字", ErrInvalidMission)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	if cur.ContractVersion != expectedVersion {
		return Mission{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	if !HandoverOfferable(cur.Status) {
		return Mission{}, fmt.Errorf("%w: %s 不可交接", ErrInvalidTransition, cur.Status)
	}
	if cur.PendingOwnerAgentID != "" {
		return Mission{}, fmt.Errorf("%w: 已有在途交接（→ %s），先承接/婉拒/撤回", ErrInvalidTransition, cur.PendingOwnerAgentID)
	}
	if newOwner == cur.OwnerAgentID {
		return Mission{}, fmt.Errorf("%w: 承接人即当前负责人", ErrInvalidTransition)
	}
	if err := s.checkHandoverTarget(ctx, newOwner); err != nil {
		return Mission{}, err
	}
	offerID := s.newOfferID()
	payload, err := json.Marshal(map[string]any{
		"offer_id":   offerID,
		"from_owner": cur.OwnerAgentID, "to_owner": newOwner, "notes": notes,
		"contract_version": expectedVersion, // CAS 后的实际版本随事实留痕
	})
	if err != nil {
		return Mission{}, fmt.Errorf("mission: 交接事件构造失败：%w", err)
	}
	out, ok2, err := s.store.HandoverOffer(ctx, missionID, expectedVersion, newOwner, offerID, Event{
		MissionID: missionID, EventType: EventHandoverOffered, Payload: payload, OccurredAt: s.now(),
	})
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
}

// HandoverAccept 承接（交接第二段——承接回执）：CAS 版本 → 有在途 → 状态族
// （offer 后若进 reviewing / canceling / 终态则拒绝——先了结任务状态）→ 承接人仍
// active（复核）→ owner 翻转 + pending 清空 + 完备性重判（unassigned draft 齐备
// → ready 入调度列；running / waiting / paused 执行控制投影保持）+
// handover_accepted 事件（from / to / contract_version / status_at_accept / note）。
func (s *Service) HandoverAccept(ctx context.Context, missionID string, expectedVersion int, offerID, note string) (Mission, error) {
	if utf8.RuneCountInString(note) > 280 {
		return Mission{}, fmt.Errorf("%w: note ≤280 字", ErrInvalidMission)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	if cur.ContractVersion != expectedVersion {
		return Mission{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	if cur.PendingOwnerAgentID == "" {
		return Mission{}, fmt.Errorf("%w: 无在途交接", ErrInvalidTransition)
	}
	// 审校 #1：请求绑定所见发起——撤回改派后（版本不递增）旧承接不得处理新交接。
	if offerID == "" || cur.PendingOfferID != offerID {
		return Mission{}, fmt.Errorf("%w: 在途交接已变化（所见发起 %q 已不在途——请刷新）", ErrInvalidTransition, offerID)
	}
	if !HandoverOfferable(cur.Status) {
		return Mission{}, fmt.Errorf("%w: %s 不可承接（先了结任务状态）", ErrInvalidTransition, cur.Status)
	}
	if err := s.checkHandoverTarget(ctx, cur.PendingOwnerAgentID); err != nil {
		return Mission{}, err
	}
	// 复审 #2：完备性重判与回执事实全部在存储事务内按现行值生成——draft 重判由
	// 事务内按现行 content + 新 owner 自算（HandoverDraftTarget 单源）；回执
	// from_owner / status_at_accept 由事务内注入（服务层预填仅占位）。
	payload, err := json.Marshal(map[string]any{
		"offer_id":   offerID,
		"from_owner": cur.OwnerAgentID, "to_owner": cur.PendingOwnerAgentID,
		"contract_version": expectedVersion, "status_at_accept": cur.Status, "note": note,
	})
	if err != nil {
		return Mission{}, fmt.Errorf("mission: 承接回执构造失败：%w", err)
	}
	out, ok2, err := s.store.HandoverAccept(ctx, missionID, expectedVersion, offerID, Event{
		MissionID: missionID, EventType: EventHandoverAccepted, Payload: payload, OccurredAt: s.now(),
	})
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
}

// HandoverDecline 婉拒（承接人显式不接——"显式无人承接"留痕，AT-03）：pending
// 清空 + handover_declined 事件；owner 不变。
func (s *Service) HandoverDecline(ctx context.Context, missionID string, expectedVersion int, offerID, reason string) (Mission, error) {
	return s.handoverWithdraw(ctx, missionID, expectedVersion, offerID, reason, EventHandoverDeclined)
}

// HandoverRevoke 撤回（发起人了结在途交接——改派前先撤回，在途唯一）：pending
// 清空 + handover_revoked 事件；owner 不变。
func (s *Service) HandoverRevoke(ctx context.Context, missionID string, expectedVersion int, offerID, reason string) (Mission, error) {
	return s.handoverWithdraw(ctx, missionID, expectedVersion, offerID, reason, EventHandoverRevoked)
}

// handoverWithdraw 婉拒/撤回共用：任何状态可了结在途交接（不应存在卡死路径）。
func (s *Service) handoverWithdraw(ctx context.Context, missionID string, expectedVersion int, offerID, reason, eventType string) (Mission, error) {
	if utf8.RuneCountInString(reason) > 280 {
		return Mission{}, fmt.Errorf("%w: reason ≤280 字", ErrInvalidMission)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	if cur.ContractVersion != expectedVersion {
		return Mission{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	if cur.PendingOwnerAgentID == "" {
		return Mission{}, fmt.Errorf("%w: 无在途交接", ErrInvalidTransition)
	}
	// 审校 #1：婉拒/撤回同样绑定所见发起。
	if offerID == "" || cur.PendingOfferID != offerID {
		return Mission{}, fmt.Errorf("%w: 在途交接已变化（所见发起 %q 已不在途——请刷新）", ErrInvalidTransition, offerID)
	}
	payload, err := json.Marshal(map[string]any{
		"offer_id":   offerID,
		"from_owner": cur.OwnerAgentID, "to_owner": cur.PendingOwnerAgentID,
		"reason": reason, "contract_version": expectedVersion,
	})
	if err != nil {
		return Mission{}, fmt.Errorf("mission: 交接了结事件构造失败：%w", err)
	}
	out, ok2, err := s.store.HandoverWithdraw(ctx, missionID, expectedVersion, offerID, Event{
		MissionID: missionID, EventType: eventType, Payload: payload, OccurredAt: s.now(),
	})
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
}

// HandoverHistory 交接史读面：四类 handover_* 事件按 seq 升序组装；未知任务
// ok=false。
func (s *Service) HandoverHistory(ctx context.Context, missionID string) ([]HandoverFact, bool, error) {
	evs, ok, err := s.store.MissionEvents(ctx, missionID)
	if err != nil || !ok {
		return nil, ok, err
	}
	out := []HandoverFact{}
	for _, e := range evs {
		var kind string
		switch e.EventType {
		case EventHandoverOffered:
			kind = "offered"
		case EventHandoverAccepted:
			kind = "accepted"
		case EventHandoverDeclined:
			kind = "declined"
		case EventHandoverRevoked:
			kind = "revoked"
		default:
			continue
		}
		var p struct {
			OfferID         string `json:"offer_id"`
			FromOwner       string `json:"from_owner"`
			ToOwner         string `json:"to_owner"`
			Notes           string `json:"notes"`
			Note            string `json:"note"`
			Reason          string `json:"reason"`
			StatusAtAccept  string `json:"status_at_accept"`
			ContractVersion int    `json:"contract_version"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		out = append(out, HandoverFact{
			Kind: kind, OfferID: p.OfferID, FromOwnerAgentID: p.FromOwner, ToOwnerAgentID: p.ToOwner,
			Notes: p.Notes, Note: p.Note, Reason: p.Reason, StatusAtAccept: p.StatusAtAccept,
			ContractVersion: p.ContractVersion, OccurredAt: e.OccurredAt,
		})
	}
	return out, true, nil
}

// Revise 修订契约（= revise_mission）：全量替换 + 版本追加；expected_version
// CAS——败者 ErrVersionConflict（可见重试）；**expectedOwner 归属锚**（0.6.12 审校
// #2——与当前 owner 不符 ErrOwnerMismatch：承接翻转 owner 不递增版本，携旧 owner
// 的修订被拒，不得无感知撤销已完成的责任转移；空串 = 预期 unassigned）；终态拒
// 修订；完备性重判（draft ↔ ready 双向，事件留痕）；执行控制投影保持（running/
// paused/canceling 中修订不改执行状态——ADR-0029 决策 5）。
func (s *Service) Revise(ctx context.Context, missionID string, expectedVersion int, expectedOwner, ownerAgentID string, c Content) (Mission, error) {
	if err := ValidateContent(c); err != nil {
		return Mission{}, err
	}
	if ownerAgentID != "" {
		if err := s.checkOwner(ctx, ownerAgentID); err != nil {
			return Mission{}, err
		}
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	// 守卫与 next 构造单源（0.6.13 审校 #3：PrepareRevision——与网关原子续接
	// 面共用；执行控制投影保持的权威核对仍在存储事务内，0.6.6 审校 P1）。
	next, err := PrepareRevision(cur, expectedOwner, ownerAgentID, c, s.now())
	if err != nil {
		return Mission{}, err
	}
	out, ok2, err := s.store.ReviseMission(ctx, missionID, expectedVersion, expectedOwner, next)
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrOwnerOrTransition(ctx, missionID, expectedVersion, expectedOwner)
	}
	return out, nil
}

// PrepareRevision 修订的纯域构造（0.6.13 审校 #3：Service.Revise 与网关原子
// 续接面（sqlite UpdateMissionAtomic）**单源**——守卫与 next 构造不得两处漂移）：
// 契约校验 → 归属锚 → 终态/验收冻结/在途交接冻结守卫 → next（版本+1 / 归一化 /
// 完备性重判 draft↔ready / 执行控制投影保持——running/paused/canceling/waiting
// 中修订不回写调度列）。CAS 与事务内权威核对在存储面。
func PrepareRevision(cur Mission, expectedOwner, ownerAgentID string, c Content, now string) (Mission, error) {
	if err := ValidateContent(c); err != nil {
		return Mission{}, err
	}
	if cur.OwnerAgentID != expectedOwner {
		return Mission{}, fmt.Errorf("%w: 期望负责人 %q，当前 %q（承接可能已翻转归属——请刷新）",
			ErrOwnerMismatch, expectedOwner, cur.OwnerAgentID)
	}
	// 0.6.9 / ADR-0032：accepted 与 canceled 同为终态不可修订；reviewing 验收
	// 基线冻结（须先退回）。
	switch cur.Status {
	case StatusCanceled:
		return Mission{}, fmt.Errorf("%w: canceled 不可修订", ErrInvalidTransition)
	case StatusAccepted:
		return Mission{}, fmt.Errorf("%w: accepted 不可修订", ErrInvalidTransition)
	case StatusReviewing:
		return Mission{}, fmt.Errorf("%w: 验收期间契约冻结（先退回）", ErrInvalidTransition)
	}
	// 0.6.12 / ADR-0035 §3：在途交接冻结窗——修订通道整包换 owner 与交接通道不得
	// 竞态（先承接/婉拒/撤回）；存储事务内同守卫。
	if cur.PendingOwnerAgentID != "" {
		return Mission{}, fmt.Errorf("%w: 交接待承接（→ %s），先了结交接再修订", ErrInvalidTransition, cur.PendingOwnerAgentID)
	}
	next := cur
	next.ContractVersion = cur.ContractVersion + 1
	next.OwnerAgentID = strings.TrimSpace(ownerAgentID)
	next.Content = normalizeContent(c)
	next.Status = statusOf(next)
	// 0.6.6 / ADR-0029 决策 5：执行控制投影保持（存储事务内以现行状态再核）。
	switch cur.Status {
	case StatusRunning, StatusPaused, StatusCanceling, StatusWaiting:
		next.Status = cur.Status
	}
	next.UpdatedAt = now
	return next, nil
}

// Cancel 取消（= cancel_mission，0.6.0 单步版）——0.6.6 起取消为两段
// （MarkCanceling → 在途停止 → FinishCancel），编排经 missioncontrol；
// 本方法移除（单一通往 canceled 的路径——防两条路径语义漂移）。

// Pause 暂停（= pause_mission，0.6.6 / ADR-0029）：ready | running →
// paused（执行控制态——阻止新调度）；与调度器 SetRunning 竞态由 CAS 条件
// 更新串行化（≤3 次重读重试）。幂等：已 paused（版本匹配）返回当前。
// draft 不可暂停（从未被调度）；canceled/canceling 拒绝。
func (s *Service) Pause(ctx context.Context, missionID string, expectedVersion int, reason string) (Mission, error) {
	if utf8.RuneCountInString(reason) > 200 {
		return Mission{}, fmt.Errorf("%w: reason ≤200 字", ErrInvalidMission)
	}
	for attempt := 0; attempt < 3; attempt++ {
		cur, ok, err := s.store.MissionByID(ctx, missionID)
		if err != nil {
			return Mission{}, err
		}
		if !ok {
			return Mission{}, ErrMissionNotFound
		}
		switch cur.Status {
		case StatusPaused:
			if cur.ContractVersion != expectedVersion {
				return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
			}
			return cur, nil // 幂等：重试安全
		case StatusReady, StatusRunning, StatusWaiting:
			out, ok2, err := s.store.PauseMission(ctx, missionID, expectedVersion, reason, s.now())
			if err != nil {
				return Mission{}, err
			}
			if ok2 {
				return out, nil
			}
			continue // 状态被并发改（如 SetRunning 抢先）——重读重试
		default:
			// draft（从未被调度）/ canceled / canceling
			return Mission{}, fmt.Errorf("%w: %s 不可暂停", ErrInvalidTransition, cur.Status)
		}
	}
	return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
}

// Resume 恢复（= resume_mission）：paused → ready | draft——目标态按当前
// 契约完备性重判（0.6.6 审校 P2：暂停期间修订删除验收条件后恢复不得直通
// ready——缺验收契约直接入调度列；落 draft 待补齐，修订补齐完备即回 ready）。
// 此后调度器自动拾取（不重启已停止的 Run：canceled 终态留档，新执行 attempt
// 递增）。幂等：已 ready（版本匹配）返回当前；其余状态拒绝。
func (s *Service) Resume(ctx context.Context, missionID string, expectedVersion int) (Mission, error) {
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	switch cur.Status {
	case StatusReady:
		if cur.ContractVersion != expectedVersion {
			return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
		}
		return cur, nil // 幂等：重试安全
	case StatusPaused:
		target := statusOf(cur) // 完备性重判：ready | draft
		if cur.WaitingReason != "" {
			// 0.6.7 / ADR-0030：暂停前在 waiting（unknown 待处置）——恢复回
			// waiting 而非 ready（暂停/恢复不绕过禁止盲目重跑）
			target = StatusWaiting
		}
		out, ok2, err := s.store.ResumeMission(ctx, missionID, expectedVersion, target, s.now())
		if err != nil {
			return Mission{}, err
		}
		if ok2 {
			return out, nil
		}
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	default:
		return Mission{}, fmt.Errorf("%w: %s 不可恢复", ErrInvalidTransition, cur.Status)
	}
}

// MarkCanceling 停止意图（= cancel_mission 第一段）：非终态 → canceling；
// 在途 Run 停止由控制编排（missioncontrol）驱动。幂等：已 canceling
// （版本匹配）返回当前（续驱）；canceled 拒绝（终态幂等拒绝——0.6.0
// 语义不变）。
func (s *Service) MarkCanceling(ctx context.Context, missionID string, expectedVersion int, reason string) (Mission, error) {
	if utf8.RuneCountInString(reason) > 200 {
		return Mission{}, fmt.Errorf("%w: reason ≤200 字", ErrInvalidMission)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	if cur.Status == StatusCanceling {
		if cur.ContractVersion != expectedVersion {
			return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
		}
		return cur, nil // 续驱幂等
	}
	if cur.Status == StatusCanceled {
		return Mission{}, fmt.Errorf("%w: canceled 不可再取消", ErrInvalidTransition)
	}
	if cur.Status == StatusAccepted {
		return Mission{}, fmt.Errorf("%w: accepted 不可取消", ErrInvalidTransition)
	}
	// 0.6.12 / ADR-0035 §3：在途交接冻结窗——停止编排与归属转移不同时进行
	// （要取消先撤回/婉拒交接）；存储事务内同守卫。
	if cur.PendingOwnerAgentID != "" {
		return Mission{}, fmt.Errorf("%w: 交接待承接（→ %s），先了结交接再取消", ErrInvalidTransition, cur.PendingOwnerAgentID)
	}
	out, ok2, err := s.store.MarkCanceling(ctx, missionID, expectedVersion, reason, s.now())
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
}

// FinishCancel 收尾（= cancel_mission 第二段）：canceling → canceled（在途
// Run 已全部停止后由控制编排调用）。幂等：已 canceled 返回当前；非
// canceling 拒绝。
func (s *Service) FinishCancel(ctx context.Context, missionID, reason string) (Mission, error) {
	out, ok, err := s.store.FinishCancel(ctx, missionID, reason, s.now())
	if err != nil {
		return Mission{}, err
	}
	if ok {
		return out, nil
	}
	cur, found, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !found {
		return Mission{}, ErrMissionNotFound
	}
	if cur.Status == StatusCanceled {
		return cur, nil // 幂等：重试安全
	}
	return Mission{}, fmt.Errorf("%w: %s 不可收尾", ErrInvalidTransition, cur.Status)
}

// MarkRunning 执行投影：ready → running（run 域创建 Run 后调用；仅
// ready 可进——返回 changed=false 表示状态不符（canceled/非 ready），
// 审校 #7：调用方须校验 changed 拒启动）。
func (s *Service) MarkRunning(ctx context.Context, missionID string) (bool, error) {
	return s.projectStatus(ctx, missionID, StatusReady, StatusRunning, "")
}

// MarkReadyFromRunning 执行投影：running → ready（Run 终态后回可执行；
// reason 进事件 payload 供审计——fence_rejected/run_failed/restart_recovery）。
func (s *Service) MarkReadyFromRunning(ctx context.Context, missionID, reason string) error {
	_, err := s.projectStatus(ctx, missionID, StatusRunning, StatusReady, reason)
	return err
}

// ReviewAccepted / ReviewReturned 裁定结论（0.6.9 / ADR-0032）。
const (
	ReviewAccepted = "accepted"
	ReviewReturned = "returned"
)

// ReviewItemNote 逐项依据（验收条文 + 裁定备注）。
type ReviewItemNote struct {
	Criterion string `json:"criterion"`
	Note      string `json:"note"`
}

// ReviewDecision 裁定（= review_mission 载荷）：decision ∈ accepted | returned；
// returned 须携 reason；ItemNotes 为逐项依据（可选）。
type ReviewDecision struct {
	Decision  string           `json:"decision"`
	Reviewer  string           `json:"reviewer"`
	Reason    string           `json:"reason"`
	ItemNotes []ReviewItemNote `json:"item_notes"`
}

// RequestReview 提请验收（= request_review 的状态机面，0.6.9 / ADR-0032）：
// ready → reviewing + review_package_id。必需交付项检查在编排层（internal/
// review——包事实）；本方法只管状态机与 CAS。幂等：已 reviewing 且同包（版本
// 匹配）返回当前；换包须先退回。
func (s *Service) RequestReview(ctx context.Context, missionID string, expectedVersion int, packageID string) (Mission, error) {
	if !strings.HasPrefix(packageID, "dlv_") {
		return Mission{}, fmt.Errorf("%w: package_id 须为 dlv_ 前缀", ErrInvalidReview)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	switch cur.Status {
	case StatusReviewing:
		if cur.ContractVersion != expectedVersion {
			return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
		}
		if cur.ReviewPackageID == packageID {
			return cur, nil // 幂等：重试安全
		}
		return Mission{}, fmt.Errorf("%w: reviewing 中不可换包（先退回）", ErrInvalidTransition)
	case StatusReady:
		// 0.6.12 / ADR-0035 §3：在途交接冻结窗——验收基线含归属静定（先了结交接）；
		// 存储事务内同守卫。
		if cur.PendingOwnerAgentID != "" {
			return Mission{}, fmt.Errorf("%w: 交接待承接（→ %s），先了结交接再送验", ErrInvalidTransition, cur.PendingOwnerAgentID)
		}
		payload, err := json.Marshal(map[string]string{"from": StatusReady, "to": StatusReviewing, "package_id": packageID})
		if err != nil {
			return Mission{}, fmt.Errorf("mission: 送验事件构造失败：%w", err)
		}
		out, ok2, err := s.store.RequestReview(ctx, missionID, expectedVersion, packageID, Event{
			MissionID: missionID, EventType: EventStatusChanged, Payload: payload, OccurredAt: s.now(),
		})
		if err != nil {
			return Mission{}, err
		}
		if !ok2 {
			// 审校 #8：并发同版本同包送验——CAS 未胜者重读，若已是 reviewing 且
			// 同包同版本则幂等返回当前（单条送验事件）；其余维持冲突语义。
			again, found, rerr := s.store.MissionByID(ctx, missionID)
			if rerr == nil && found && again.Status == StatusReviewing &&
				again.ContractVersion == expectedVersion && again.ReviewPackageID == packageID {
				return again, nil
			}
			return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
		}
		return out, nil
	default:
		// draft/running/waiting/paused/canceling/canceled/accepted
		return Mission{}, fmt.Errorf("%w: %s 不可送验", ErrInvalidTransition, cur.Status)
	}
}

// RecordReview 验收裁定（= review_mission，0.6.9 / ADR-0032）：reviewing →
// accepted（终态，锚保留）| ready（退回，锚清空——可再执行/再交付/再送验）。
// 验收权限：契约 reviewer 非空时 d.Reviewer 须相等（空 = 请求人自验）。
func (s *Service) RecordReview(ctx context.Context, missionID string, expectedVersion, reviewRound int, d ReviewDecision) (Mission, error) {
	if d.Decision != ReviewAccepted && d.Decision != ReviewReturned {
		return Mission{}, fmt.Errorf("%w: decision 须为 accepted | returned", ErrInvalidReview)
	}
	if reviewRound < 1 {
		return Mission{}, fmt.Errorf("%w: review_round 必填（裁定所见的送验轮次）", ErrInvalidReview)
	}
	// 审校 #9：验收人按 Unicode 字符计数 ≤64（不入事件）
	if utf8.RuneCountInString(d.Reviewer) > 64 {
		return Mission{}, fmt.Errorf("%w: reviewer ≤64 字", ErrInvalidReview)
	}
	if utf8.RuneCountInString(d.Reason) > 500 {
		return Mission{}, fmt.Errorf("%w: reason ≤500 字", ErrInvalidReview)
	}
	if len(d.ItemNotes) > 16 {
		return Mission{}, fmt.Errorf("%w: item_notes ≤16 项", ErrInvalidReview)
	}
	for i, n := range d.ItemNotes {
		if l := utf8.RuneCountInString(strings.TrimSpace(n.Criterion)); l < 1 || l > 256 {
			return Mission{}, fmt.Errorf("%w: item_notes[%d].criterion 须 1–256 字", ErrInvalidReview, i)
		}
		if utf8.RuneCountInString(n.Note) > 500 {
			return Mission{}, fmt.Errorf("%w: item_notes[%d].note ≤500 字", ErrInvalidReview, i)
		}
	}
	if d.Decision == ReviewReturned && strings.TrimSpace(d.Reason) == "" {
		return Mission{}, fmt.Errorf("%w: 退回须携原因", ErrInvalidReview)
	}
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return Mission{}, err
	}
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	if cur.Reviewer != "" && d.Reviewer != cur.Reviewer {
		return Mission{}, fmt.Errorf("%w: 契约验收人为 %q", ErrReviewNotAuthorized, cur.Reviewer)
	}
	if cur.Status != StatusReviewing {
		return Mission{}, fmt.Errorf("%w: %s 不可裁定", ErrInvalidTransition, cur.Status)
	}
	itemNotes := make([]ReviewItemNote, 0, len(d.ItemNotes))
	itemNotes = append(itemNotes, d.ItemNotes...)
	to := StatusAccepted
	if d.Decision == ReviewReturned {
		to = StatusReady // 审校 #10：事件 to 为真实目标状态（decision 仍记 returned）
	}
	payload, err := json.Marshal(map[string]any{
		"from": StatusReviewing, "to": to, "decision": d.Decision,
		"reviewer": d.Reviewer, "reason": d.Reason, "package_id": cur.ReviewPackageID,
		// 0.6.10 审校 #1：裁定事实随当时契约版本留痕——回房投影不再读当前行
		"contract_version": expectedVersion,
		"review_round":     reviewRound, "item_notes": itemNotes,
	})
	if err != nil {
		return Mission{}, fmt.Errorf("mission: 裁定事件构造失败：%w", err)
	}
	out, ok2, err := s.store.RecordReview(ctx, missionID, expectedVersion, reviewRound, to, Event{
		MissionID: missionID, EventType: EventStatusChanged, Payload: payload, OccurredAt: s.now(),
	}, cur.ReviewPackageID)
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
}

// MarkWaiting 等待投影（0.6.7 / ADR-0030；0.6.9 审校 #2 扩 ready）：running | ready → waiting(reason)——恢复
// 分派结果不可判定时由 run 域调用；出调度列直至 ClearWaiting。幂等条件转移
// （非 running 无操作 changed=false）。ready 形态的残留不走 waiting（RFC
// §3.2 waiting 挂 running）——run 域执行面围栏挡（ErrResidueUnresolved）。
func (s *Service) MarkWaiting(ctx context.Context, missionID, reason string) (bool, error) {
	// 0.6.9 审校 #2：ready | running 皆可进等待——Run 落库与 running 投影两步
	// 间隙崩溃会残留 ready + 未终态 Run，恢复分派同样须进 waiting。
	changed, err := s.store.MarkWaiting(ctx, missionID, StatusRunning, reason, s.waitingEvent(StatusRunning, reason))
	if err != nil {
		return false, fmt.Errorf("mission: 等待投影失败：%w", err)
	}
	if changed {
		return true, nil
	}
	changed, err = s.store.MarkWaiting(ctx, missionID, StatusReady, reason, s.waitingEvent(StatusReady, reason))
	if err != nil {
		return false, fmt.Errorf("mission: 等待投影失败：%w", err)
	}
	return changed, nil
}

func (s *Service) waitingEvent(from, reason string) Event {
	payload, _ := json.Marshal(map[string]string{"from": from, "to": StatusWaiting, "reason": reason})
	return Event{EventType: EventStatusChanged, Payload: payload, OccurredAt: s.now()}
}

// ClearWaiting 出等待（处置后）：waiting → ready | draft（完备性重判——等待
// 期间修订削验收不盲回 ready）+ 清等待原因。幂等条件转移（非 waiting 无操作
// ——Mission 已取消/暂停时处置事实仍落 Run 侧）。
func (s *Service) ClearWaiting(ctx context.Context, missionID string) (bool, error) {
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return false, err
	}
	if !ok || cur.Status != StatusWaiting {
		return false, nil
	}
	return s.projectStatus(ctx, missionID, StatusWaiting, statusOf(cur), "resolved")
}

// projectStatus 条件状态投影（from 不符或 Mission 未知 = 幂等无操作
// changed=false；canceled 拒转）。status_changed 事件携 from/to/reason。
func (s *Service) projectStatus(ctx context.Context, missionID, from, to, reason string) (bool, error) {
	payload, err := json.Marshal(map[string]string{"from": from, "to": to, "reason": reason})
	if err != nil {
		return false, fmt.Errorf("mission: 投影事件构造失败：%w", err)
	}
	ev := Event{MissionID: missionID, EventType: EventStatusChanged, Payload: payload, OccurredAt: s.now()}
	changed, err := s.store.TransitionStatus(ctx, missionID, from, to, ev)
	if err != nil {
		return false, fmt.Errorf("mission: 状态投影失败：%w", err)
	}
	return changed, nil
}

// Get 契约详情；未知 ok=false。
func (s *Service) Get(ctx context.Context, missionID string) (Mission, bool, error) {
	return s.store.MissionByID(ctx, missionID)
}

// List 契约摘要（创建序）。
func (s *Service) ListMissions(ctx context.Context) ([]Summary, error) {
	return s.store.ListMissions(ctx)
}

// ListByStatus 契约摘要（创建序，按状态过滤——恢复面枚举 canceling 兑现
// 收尾用，0.6.6 审校 P2）。
func (s *Service) ListByStatus(ctx context.Context, status string) ([]Summary, error) {
	return s.store.ListMissionsByStatus(ctx, status)
}

// Versions 不可变版本行升序；未知 ok=false。
func (s *Service) MissionVersions(ctx context.Context, missionID string) ([]Mission, bool, error) {
	return s.store.MissionVersions(ctx, missionID)
}

// Events 事件流（seq 升序）；未知 ok=false。
func (s *Service) MissionEvents(ctx context.Context, missionID string) ([]Event, bool, error) {
	return s.store.MissionEvents(ctx, missionID)
}

// SourceMapped 来源映射查询（dispatch 预检/复核用）；未映射 ok=false。
func (s *Service) SourceMapped(ctx context.Context, roomID, taskID string) (string, bool, error) {
	return s.store.MissionIDBySource(ctx, roomID, taskID)
}

// checkOwner owner 存在性（AgentLookup 未装配时带 owner 的请求不可用——
// 宁拒不放行，避免无校验落库）。
func (s *Service) checkOwner(ctx context.Context, agentID string) error {
	if s.agentLookup == nil {
		return fmt.Errorf("mission: 未装配 AgentLookup（owner 校验不可用）")
	}
	ok, err := s.agentLookup.AgentExists(ctx, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAgentNotFound
	}
	return nil
}

// casOrTransition CAS 败因分辨：重读当前行——版本不匹配 → 版本冲突；
// 版本匹配仍败 → 终态/转移拒绝。
func (s *Service) casOrTransition(ctx context.Context, missionID string, expectedVersion int) error {
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMissionNotFound
	}
	if cur.ContractVersion != expectedVersion {
		return fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	return fmt.Errorf("%w: %s", ErrInvalidTransition, cur.Status)
}

// casOrOwnerOrTransition 修订 CAS 败因分辨（0.6.12 审校 #2）：版本不匹配 →
// 版本冲突；归属锚不匹配 → ErrOwnerMismatch（调用方所见 owner 已被承接翻转）；
// 其余 → 终态/转移拒绝。
func (s *Service) casOrOwnerOrTransition(ctx context.Context, missionID string, expectedVersion int, expectedOwner string) error {
	cur, ok, err := s.store.MissionByID(ctx, missionID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMissionNotFound
	}
	if cur.ContractVersion != expectedVersion {
		return fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, expectedVersion, cur.ContractVersion)
	}
	if cur.OwnerAgentID != expectedOwner {
		return fmt.Errorf("%w: 期望负责人 %q，当前 %q", ErrOwnerMismatch, expectedOwner, cur.OwnerAgentID)
	}
	return fmt.Errorf("%w: %s", ErrInvalidTransition, cur.Status)
}

// statusOf 完备性判定单源（创建与修订同源）：objective/scope 非空由校验
// 保证；验收/交付物/来源齐备且 owner 已指派 → ready，否则 draft。
func statusOf(m Mission) string {
	if m.OwnerAgentID == "" ||
		len(m.Content.AcceptanceCriteria) == 0 ||
		len(m.Content.DeliverableSpecs) == 0 ||
		len(m.Content.SourceRefs) == 0 {
		return StatusDraft
	}
	return StatusReady
}

// ---- 校验（单源：必填/上限/模式/枚举/预算界限） ----

var (
	sourceRefPattern = regexp.MustCompile(`^[A-Za-z0-9/_.:#@-]{1,128}$`)
	taskIDPattern    = regexp.MustCompile(`^tsk_[0-9A-Za-z_-]+$`)
)

// ValidateContent 契约内容校验（导出供 dispatch 预检共用——ADR-0024：
// 解析与创建同一校验单源）。
func ValidateContent(c Content) error { return validateContent(c) }

func validateContent(c Content) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Objective)); n < 1 || n > 512 {
		return fmt.Errorf("%w: objective 须 1–512 字", ErrInvalidMission)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Scope)); n < 1 || n > 512 {
		return fmt.Errorf("%w: scope 须 1–512 字", ErrInvalidMission)
	}
	if err := validateLines("constraints", c.Constraints, 16, 256, true); err != nil {
		return err
	}
	if err := validateLines("acceptance_criteria", c.AcceptanceCriteria, 16, 256, false); err != nil {
		return err
	}
	if err := validateLines("deliverable_specs", c.DeliverableSpecs, 16, 256, false); err != nil {
		return err
	}
	if len(c.SourceRefs) > 16 {
		return fmt.Errorf("%w: source_refs ≤16", ErrInvalidMission)
	}
	for i, ref := range c.SourceRefs {
		if !sourceRefPattern.MatchString(ref) {
			return fmt.Errorf("%w: source_refs[%d] 非法（%s）", ErrInvalidMission, i, ref)
		}
	}
	if err := validateLines("return_targets", c.ReturnTargets, 8, 128, true); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(c.Reviewer); n > 64 {
		return fmt.Errorf("%w: reviewer ≤64 字", ErrInvalidMission)
	}
	if c.Budget != (Budget{}) {
		if err := validateBudget(c.Budget); err != nil {
			return err
		}
	}
	if c.Delegation.MaxDepth < 0 || c.Delegation.MaxDepth > 8 {
		return fmt.Errorf("%w: delegation.max_depth 须 0–8", ErrInvalidMission)
	}
	seen := map[string]bool{}
	for _, a := range c.Delegation.Allowed {
		if a != "existing_agent" && a != "ephemeral_worker" {
			return fmt.Errorf("%w: delegation.allowed 非法枚举（%s）", ErrInvalidMission, a)
		}
		if seen[a] {
			return fmt.Errorf("%w: delegation.allowed 重复（%s）", ErrInvalidMission, a)
		}
		seen[a] = true
	}
	return nil
}

func validateLines(field string, lines []string, maxItems, maxLen int, allowBlank bool) error {
	if len(lines) > maxItems {
		return fmt.Errorf("%w: %s ≤%d 项", ErrInvalidMission, field, maxItems)
	}
	for i, l := range lines {
		if allowBlank {
			if n := utf8.RuneCountInString(l); n > maxLen {
				return fmt.Errorf("%w: %s[%d] ≤%d 字", ErrInvalidMission, field, i, maxLen)
			}
			continue
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(l)); n < 1 || n > maxLen {
			return fmt.Errorf("%w: %s[%d] 须 1–%d 字", ErrInvalidMission, field, i, maxLen)
		}
	}
	return nil
}

// ValidateBudget 显式预算校验（导出供 HTTP 边界使用——审校 P2#3：请求体
// 显式携带 budget 键时即使全零也按界限拒绝，不得当省略套默认）。
func ValidateBudget(b Budget) error { return validateBudget(b) }

// validateBudget 显式预算须五项齐备且在界限内（零值整体 = 缺省，不混并）。
func validateBudget(b Budget) error {
	if b.Concurrency < 1 || b.Concurrency > 8 {
		return fmt.Errorf("%w: budget.concurrency 须 1–8", ErrInvalidMission)
	}
	if b.MaxRuns < 1 || b.MaxRuns > 1000 {
		return fmt.Errorf("%w: budget.max_runs 须 1–1000", ErrInvalidMission)
	}
	if b.MaxWallClockSeconds < 60 || b.MaxWallClockSeconds > 2592000 {
		return fmt.Errorf("%w: budget.max_wall_clock_seconds 须 60–2592000", ErrInvalidMission)
	}
	if b.MaxRetries < 0 || b.MaxRetries > 10 {
		return fmt.Errorf("%w: budget.max_retries 须 0–10", ErrInvalidMission)
	}
	if b.MaxDelegationDepth < 0 || b.MaxDelegationDepth > 8 {
		return fmt.Errorf("%w: budget.max_delegation_depth 须 0–8", ErrInvalidMission)
	}
	return nil
}

// normalizeContent 空切片/零值补齐（JSON 往返稳定 + 缺省单源）。
func normalizeContent(c Content) Content {
	if c.Constraints == nil {
		c.Constraints = []string{}
	}
	if c.AcceptanceCriteria == nil {
		c.AcceptanceCriteria = []string{}
	}
	if c.DeliverableSpecs == nil {
		c.DeliverableSpecs = []string{}
	}
	if c.SourceRefs == nil {
		c.SourceRefs = []string{}
	}
	if c.ReturnTargets == nil {
		c.ReturnTargets = []string{}
	}
	if c.Budget == (Budget{}) {
		c.Budget = DefaultBudget()
	}
	if c.Delegation.Allowed == nil {
		c.Delegation.Allowed = []string{}
	}
	return c
}
