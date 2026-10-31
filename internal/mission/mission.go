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
)

// 事件类型（mission_events 流）。
const (
	EventCreated         = "created"
	EventContractRevised = "contract_revised"
	EventStatusChanged   = "status_changed"
)

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
	// ErrSourceAlreadyMapped 房间来源已被交办（ADR-0024：(room_id, task_id)
	// 全局唯一映射——同一任务卡至多交办一次，撞键回显已映射 mission_id）。
	ErrSourceAlreadyMapped = errors.New("mission: 房间来源已被交办")
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
	MissionID       string      `json:"mission_id"`
	ContractVersion int         `json:"contract_version"`
	Status          string      `json:"status"`
	OwnerAgentID    string      `json:"owner_agent_id"` // 空 = 显式 unassigned（§3.3）
	Requester       string      `json:"requester"`
	RoomSource      *RoomSource `json:"room_source"` // 房间交办来源（非房间交办 null；创建事实）
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
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
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
// 行为能力由执行/交接时点校验；只需存在性）。
type AgentLookup interface {
	// AgentExists Agent 是否存在。
	AgentExists(ctx context.Context, agentID string) (bool, error)
}

// SourceLookup 房间域查询端口（ADR-0024 / 0.6.1）：房间任务卡存在性——
// app 装配注入适配（EventReader 分页读全量事件 → room.TasksOf 纯函数重建）。
type SourceLookup interface {
	// RoomTaskExists 房间任务卡是否存在于当前投影。
	RoomTaskExists(ctx context.Context, roomID, taskID string) (bool, error)
}

// RoomSource 房间交办来源（ADR-0024 / 0.6.1）：(room_id, task_id) 全局唯一
// 映射——映射为创建事实，修订不可改（换来源 = 新交办）。
type RoomSource struct {
	RoomID string `json:"room_id"`
	TaskID string `json:"task_id"`
}

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
// 一切写路径单事务 [校验→投影→版本行→事件]（BEGIN IMMEDIATE 全库约定）。
type Store interface {
	// CreateMission 投影 v1 + 不可变版本行 + created 事件（+ 可选房间来源
	// 映射），单事务；来源撞键 → ErrSourceAlreadyMapped（携已映射 mission_id）。
	CreateMission(ctx context.Context, m Mission, src *RoomSource) (Mission, error)
	// MissionIDBySource 来源映射查询；未映射 ok=false。
	MissionIDBySource(ctx context.Context, roomID, taskID string) (string, bool, error)
	// Revise CAS 修订：contract_version 匹配且非终态才落新版本行 + 投影更新
	// + contract_revised（及完备性变化时 status_changed）事件，单事务；
	// ok=false = 版本不匹配或已终态（服务方重读区分）。
	ReviseMission(ctx context.Context, missionID string, expectedVersion int, next Mission) (Mission, bool, error)
	// PauseMission CAS 暂停（0.6.6 / ADR-0029）：contract_version 匹配且
	// status ∈ (ready, running) → paused + status_changed（from/to/reason）
	// 事件，单事务；ok=false = 版本不匹配/状态不合法/未知（服务方重读区分；
	// 幂等由服务层先读实现——存储面只认 ready/running）。
	PauseMission(ctx context.Context, missionID string, expectedVersion int, reason, occurredAt string) (Mission, bool, error)
	// ResumeMission CAS 恢复：contract_version 匹配且 status = paused →
	// ready + status_changed 事件，单事务；ok=false 同上。
	ResumeMission(ctx context.Context, missionID string, expectedVersion int, occurredAt string) (Mission, bool, error)
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
	// Versions 不可变版本行升序（每行含该版本完备性事实）；未知 ok=false。
	MissionVersions(ctx context.Context, missionID string) ([]Mission, bool, error)
	// Events 事件流（seq 升序）；未知 ok=false。
	MissionEvents(ctx context.Context, missionID string) ([]Event, bool, error)
	// TransitionStatus 条件状态投影（0.6.5 / ADR-0028）：当前状态 == from
	// 才更新为 to + 事件（单事务）；from 不符/Mission 未知/终态 = 无操作
	// 成功（幂等——调用方序已知）。canceled 拒转。
	TransitionStatus(ctx context.Context, missionID, from, to string, event Event) (changed bool, err error)
}

// Service 契约服务：校验单源 + 完备性判定单源 + CAS 编排。
type Service struct {
	store        Store
	now          func() string
	newMissionID func() string
	agentLookup  AgentLookup  // nil = owner 存在性校验不可用（创建/修订拒带 owner）
	sourceLookup SourceLookup // nil = 房间来源校验不可用（创建拒带 room_source）
}

// Option 装配选项（测试注入确定性时钟/ID 生成器）。
type Option func(*Service)

// WithNow 注入时钟（缺省 RFC3339Nano UTC）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewMissionID 注入 ID 生成器（缺省：mi_ + 时间有序 + 随机后缀）。
func WithNewMissionID(f func() string) Option { return func(s *Service) { s.newMissionID = f } }

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

// Create 建契约（= create_mission，§11.2）：校验 → owner/来源存在性 →
// 完备性判定（draft/ready）→ 投影 v1 + 版本行 + created 事件（+ 可选来源
// 映射）单事务。
func (s *Service) Create(ctx context.Context, ownerAgentID string, c Content, src *RoomSource) (Mission, error) {
	if err := ValidateContent(c); err != nil {
		return Mission{}, err
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
	out, err := s.store.CreateMission(ctx, m, src)
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

// Revise 修订契约（= revise_mission）：全量替换 + 版本追加；expected_version
// CAS——败者 ErrVersionConflict（可见重试）；终态拒修订；完备性重判
// （draft ↔ ready 双向，事件留痕）；执行控制投影保持（running/paused/
// canceling 中修订不改执行状态——ADR-0029 决策 5）。
func (s *Service) Revise(ctx context.Context, missionID string, expectedVersion int, ownerAgentID string, c Content) (Mission, error) {
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
	if cur.Status == StatusCanceled {
		return Mission{}, fmt.Errorf("%w: canceled 不可修订", ErrInvalidTransition)
	}
	next := cur
	next.ContractVersion = cur.ContractVersion + 1
	next.OwnerAgentID = strings.TrimSpace(ownerAgentID)
	next.Content = normalizeContent(c)
	next.Status = statusOf(next)
	// 0.6.6 / ADR-0029 决策 5：执行控制投影保持——running/paused/canceling
	// 中修订不回写 draft/ready（在途 Run 不静默换输入；停止/完成后下一
	// attempt 自然用新版本）。
	switch cur.Status {
	case StatusRunning, StatusPaused, StatusCanceling:
		next.Status = cur.Status
	}
	next.UpdatedAt = s.now()
	out, ok2, err := s.store.ReviseMission(ctx, missionID, expectedVersion, next)
	if err != nil {
		return Mission{}, err
	}
	if !ok2 {
		return Mission{}, s.casOrTransition(ctx, missionID, expectedVersion)
	}
	return out, nil
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
		case StatusReady, StatusRunning:
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

// Resume 恢复（= resume_mission）：paused → ready——此后调度器自动拾取
// （不重启已停止的 Run：canceled 终态留档，新执行 attempt 递增）。幂等：
// 已 ready（版本匹配）返回当前；其余状态拒绝。
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
		out, ok2, err := s.store.ResumeMission(ctx, missionID, expectedVersion, s.now())
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
