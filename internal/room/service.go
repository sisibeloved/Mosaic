// Package room 的命令处理服务。
package room

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// MaxBodyRunes 消息正文字长上限（UTF-8 字符数；超限拒绝而非截断——严格写）。
const MaxBodyRunes = 20000

var uuidv7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Actor 命令行动者（个人版：human=本地 owner；agent 命令属内部写入不经本服务）。
type Actor struct {
	ParticipantID string
	Kind          string // human | agent | system
}

// Command 外部命令（RFC-0001 命令契约；tenant 由服务持有，actor 由调用方绑定）。
type Command struct {
	RoomID              string
	CommandKind         string
	ExpectedRoomVersion int64
	IdempotencyKey      string
	IssuedAt            string
	Payload             json.RawMessage
}

// CommandResult 命令执行结果（Replay 场景返回原事件标识）。
type CommandResult struct {
	RoomID      string
	EventID     string
	RoomVersion int64
	Replayed    bool
}

// Config 服务依赖注入。
type Config struct {
	Store  AtomicStore
	Reader EventReader                // 可选：endorse_intent 的目标存在性检查（MemStore/SQLite 双实现）
	Lister RoomLister                 // 可选：GET /v1/rooms 房间列表读路径（nil 时该端点 500）
	Clock  func() string              // RFC3339
	NewID  func(prefix string) string // 事件/房间 ID 生成（前缀 evt_/room_）
	Tenant string
	// Seats 可选：当前在席座位（装配层注入引擎快照）。create_room 未选人时
	// 物化当时在席名单（v1.24：roster 是创建时点快照——建房后新启用的 Agent
	// 不自动入房，增量走 invite_agent）。nil = 不物化（空 agents，旧语义）。
	Seats func() []AgentSeat
}

// Service 命令处理服务：校验 → 幂等 → 并发检查 → 事件生产（原子落库）。
type Service struct {
	cfg Config
}

// NewService 构造服务；Config 的依赖为必填。
func NewService(cfg Config) *Service {
	if cfg.Store == nil || cfg.Clock == nil || cfg.NewID == nil {
		panic("room: Config.Store/Clock/NewID 必填")
	}
	return &Service{cfg: cfg}
}

// ExecuteCommand 执行外部命令。幂等回放优先于一切检查（RFC-0001：回放返回已记录结果）。
func (s *Service) ExecuteCommand(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if !uuidv7Pattern.MatchString(cmd.IdempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency_key 必须为 UUIDv7", ErrInvalidCommand)
	}
	// 二轮审校 #21：issued_at 运行时校验（Schema 只管 fixture 门禁，不管运行时）。
	// 未来时间戳不拒（时钟偏差容忍），非法格式拒——契约字段不得是自由文本。
	if _, err := time.Parse(time.RFC3339, cmd.IssuedAt); err != nil {
		return nil, fmt.Errorf("%w: issued_at 必须为 RFC3339 时间戳", ErrInvalidCommand)
	}
	if actor.ParticipantID == "" || (actor.Kind != "human" && actor.Kind != "system") {
		return nil, fmt.Errorf("%w: 外部命令 actor 必须为具名 human/system", ErrInvalidCommand)
	}
	switch cmd.CommandKind {
	case "create_room":
		return s.createRoom(ctx, actor, cmd)
	case "post_message":
		return s.postMessage(ctx, actor, cmd)
	case "pause_room":
		return s.roomLifecycle(ctx, actor, cmd, protocol.EventRoomPaused, "pause_room payload")
	case "resume_room":
		return s.roomLifecycle(ctx, actor, cmd, protocol.EventRoomStarted, "resume_room payload")
	case "rename_room":
		return s.renameRoom(ctx, actor, cmd)
	case "endorse_intent":
		return s.endorseIntent(ctx, actor, cmd)
	case "invite_agent":
		return s.inviteAgent(ctx, actor, cmd)
	case "propose_closure":
		return s.proposeClosure(ctx, actor, cmd)
	case "accept_closure":
		return s.acceptClosure(ctx, actor, cmd)
	case "create_evidence_request":
		return s.createEvidenceRequest(ctx, actor, cmd)
	case "resolve_evidence_request":
		return s.resolveEvidenceRequest(ctx, actor, cmd)
	case "resolve_task":
		return s.resolveTask(ctx, actor, cmd)
	case "edit_memory":
		return s.editMemory(ctx, actor, cmd)
	case "delete_room":
		return s.deleteRoom(ctx, actor, cmd)
	case "fork_thread", "pause_thread", "resume_thread", "close_thread", "reopen_thread", "merge_thread":
		eventType := map[string]string{
			"fork_thread": "thread.forked", "pause_thread": "thread.paused",
			"resume_thread": "thread.resumed", "close_thread": "thread.closed",
			"reopen_thread": "thread.reopened", "merge_thread": "thread.merged",
		}[cmd.CommandKind]
		return s.executeThreadCommand(ctx, actor, cmd, eventType)
	default:
		return nil, fmt.Errorf("%w: 未知命令 %q", ErrInvalidCommand, cmd.CommandKind)
	}
}

func (s *Service) createRoom(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	// 幂等回放最先（与 post_message 一致：同键异载荷是冲突，同载荷返回原房间）
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if cmd.RoomID != "" {
		return nil, fmt.Errorf("%w: create_room 不接受 room_id", ErrInvalidCommand)
	}
	var payload struct {
		DisplayName string   `json:"display_name"`
		Agents      []string `json:"agents"` // 可选：入房 Agent（participant ID；缺省 = 物化当时在席名单）
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: create_room payload: %v", ErrInvalidCommand, err)
	}
	if len([]rune(payload.DisplayName)) > 120 {
		return nil, fmt.Errorf("%w: display_name 超 120 字", ErrInvalidCommand)
	}
	if len(payload.Agents) > 8 {
		return nil, fmt.Errorf("%w: agents ≤ 8", ErrInvalidCommand)
	}
	for _, a := range payload.Agents {
		if !participantIDPattern.MatchString(a) {
			return nil, fmt.Errorf("%w: agents 项须为 participant ID（par_*）", ErrInvalidCommand)
		}
	}
	// 缺省选人 → 物化当时在席座位（v1.24：roster 是创建时点快照——建房后新启用的
	// Agent 不自动入房，增量走 invite_agent）。取前 8（与显式选人同一上界）。
	if len(payload.Agents) == 0 && s.cfg.Seats != nil {
		for _, seat := range s.cfg.Seats() {
			if len(payload.Agents) >= 8 {
				break
			}
			if participantIDPattern.MatchString(seat.ParticipantID) {
				payload.Agents = append(payload.Agents, seat.ParticipantID)
			}
		}
	}

	roomID := s.cfg.NewID("room")
	rootThread := s.cfg.NewID("thr") // 根线程：房间创建即有（Thread 生命周期 M2 展开）
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        roomID,
		Type:          protocol.EventRoomCreated,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(map[string]any{
			"display_name": payload.DisplayName, "thread_id": rootThread,
			// 名单快照（v1.24 起含缺省物化）；空数组仅出现在未接 Seats 的旧装配（兼容投影）
			"agents": payload.Agents,
		}),
		Metadata: map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              roomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

func (s *Service) postMessage(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	// 幂等回放最先：房间演进/删除不影响已受理命令的重放语义
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}

	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		// 复审 #22：并发同键的首次执行可能恰在本预检前提交回执——重查回放，
		// 重试须回放而非误报版本冲突（入口的回放检查与此处之间存在竞态窗口）。
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}

	var payload postMessagePayload
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: post_message payload: %v", ErrInvalidCommand, err)
	}
	if payload.ThreadID != nil && !threadIDPattern.MatchString(*payload.ThreadID) {
		return nil, fmt.Errorf("%w: thread_id 形如 thr_*", ErrInvalidCommand)
	}
	bodyRunes := len([]rune(payload.Body))
	if bodyRunes < 1 || bodyRunes > MaxBodyRunes {
		return nil, fmt.Errorf("%w: body 长度 1..%d 字", ErrInvalidCommand, MaxBodyRunes)
	}
	if len(payload.AddressedTo) > 3 {
		return nil, fmt.Errorf("%w: addressed_to ≤ 3", ErrInvalidCommand)
	}
	// M2 定稿（message.posted Schema）：relations 为带类型声明（RFC-0004 §3.1.4）——
	// 命令侧只收 target_event_id + kind（provenance 由系统固化为 explicit，不收客户端值）。
	for i, rel := range payload.Relations {
		if !eventIDPattern.MatchString(rel.TargetEventID) {
			return nil, fmt.Errorf("%w: relations[%d].target_event_id 形如 evt_*", ErrInvalidCommand, i)
		}
		if !relationKinds[rel.Kind] {
			return nil, fmt.Errorf("%w: relations[%d].kind 非法 %q", ErrInvalidCommand, i, rel.Kind)
		}
		rel.Provenance = "explicit"
		payload.Relations[i] = rel
	}

	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		ThreadID:      payload.ThreadID,
		Type:          protocol.EventMessagePosted,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       mustJSON(payload),
		Metadata:      map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// postMessagePayload 消息命令载荷（严格字段集：多余字段拒绝；
// 字段集与 events/message.posted.schema.json 对齐，M2 定稿）。
type postMessagePayload struct {
	Body        string          `json:"body"`
	ReplyTo     *string         `json:"reply_to"`
	AddressedTo []string        `json:"addressed_to"`
	Relations   []typedRelation `json:"relations"`
	ThreadID    *string         `json:"thread_id"` // 可选：发往指定线程（根线程随 room.created 载荷）
}

// typedRelation 类型化关系声明（RFC-0004 §3.1.4）。命令侧不收 provenance
// （DisallowUnknownFields 即拒绝），落库前由服务固化为 explicit。
type typedRelation struct {
	TargetEventID string `json:"target_event_id"`
	Kind          string `json:"kind"`
	Provenance    string `json:"provenance"`
}

// relationKinds 关系类型枚举（RFC-0004：八种，无无类型简写）。
var relationKinds = map[string]bool{
	"supports": true, "challenges": true, "extends": true, "questions": true,
	"evidence_for": true, "supersedes": true, "analogy": true, "relates": true,
}

var (
	threadIDPattern      = regexp.MustCompile(`^thr_[0-9A-Za-z_-]+$`)
	eventIDPattern       = regexp.MustCompile(`^evt_[0-9A-Za-z_-]+$`)
	intentIDPattern      = regexp.MustCompile(`^int_[0-9A-Za-z_-]+$`)
	participantIDPattern = regexp.MustCompile(`^par_[0-9A-Za-z_-]+$`)
	taskIDPattern        = regexp.MustCompile(`^tsk_[0-9A-Za-z_-]+$`)
	closureIDPattern     = regexp.MustCompile(`^clo_[0-9A-Za-z_-]+$`)
)

// resolveTask 人类裁定派生任务（tasklist 人工门控——delivered/dismissed 由人
// 定，自动判定交付会伪装闭环）。校验：任务存在且 pending。
func (s *Service) resolveTask(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		TaskID     string `json:"task_id"`
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: resolve_task payload: %v", ErrInvalidCommand, err)
	}
	if !taskIDPattern.MatchString(payload.TaskID) {
		return nil, fmt.Errorf("%w: task_id 形如 tsk_*", ErrInvalidCommand)
	}
	if payload.Resolution != "delivered" && payload.Resolution != "dismissed" {
		return nil, fmt.Errorf("%w: resolution 取值 delivered | dismissed", ErrInvalidCommand)
	}
	if len([]rune(payload.Note)) > 280 {
		return nil, fmt.Errorf("%w: note 超 280 字", ErrInvalidCommand)
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	tasks := TasksOf(history)
	var target *TaskItem
	for i := range tasks {
		if tasks[i].TaskID == payload.TaskID {
			target = &tasks[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("%w: 任务 %s 不存在", ErrInvalidCommand, payload.TaskID)
	}
	if target.Status != "pending" {
		return nil, fmt.Errorf("%w: 任务已裁定（%s），不可重复裁定", ErrInvalidCommand, target.Status)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventTaskResolved,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.TaskResolvedPayload{
			TaskID:     payload.TaskID,
			Owner:      target.Owner,
			Resolution: payload.Resolution,
			Note:       payload.Note,
			ResolvedBy: actor.ParticipantID,
		}),
		Metadata: map[string]any{},
	}
	return s.commitWith(ctx, cmd, actor, env)
}

// editMemory 人工编辑胶囊记忆（RFC-0007 §7.4 裁定 5：纠错留 edit_history、
// 生效于下次组装）。conclusions/assumptions 为编辑后全文（整组替换，至少一项）。
func (s *Service) editMemory(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		MemoryID    string   `json:"memory_id"`
		Conclusions []string `json:"conclusions"`
		Assumptions []string `json:"assumptions"`
		Note        string   `json:"note"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: edit_memory payload: %v", ErrInvalidCommand, err)
	}
	if !closureIDPattern.MatchString(payload.MemoryID) {
		return nil, fmt.Errorf("%w: memory_id 形如 clo_*（胶囊 closure_id）", ErrInvalidCommand)
	}
	if len([]rune(payload.Note)) > 280 {
		return nil, fmt.Errorf("%w: note 超 280 字", ErrInvalidCommand)
	}
	if len(payload.Conclusions) == 0 && len(payload.Assumptions) == 0 {
		return nil, fmt.Errorf("%w: conclusions/assumptions 至少一项（编辑后全文，整组替换）", ErrInvalidCommand)
	}
	for name, items := range map[string][]string{"conclusions": payload.Conclusions, "assumptions": payload.Assumptions} {
		if len(items) > 12 {
			return nil, fmt.Errorf("%w: %s ≤ 12 条", ErrInvalidCommand, name)
		}
		for i, it := range items {
			if len([]rune(it)) < 1 || len([]rune(it)) > 500 {
				return nil, fmt.Errorf("%w: %s[%d] 长度 1..500 字", ErrInvalidCommand, name, i)
			}
		}
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	known := false
	for _, c := range AcceptedCapsulesOf(history) {
		if c.ClosureID == payload.MemoryID {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("%w: 胶囊 %s 不存在（须为已接受收束）", ErrInvalidCommand, payload.MemoryID)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventMemoryEdited,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.MemoryEditedPayload{
			MemoryID:    payload.MemoryID,
			Conclusions: payload.Conclusions,
			Assumptions: payload.Assumptions,
			Note:        payload.Note,
			EditVersion: NextEditVersionOf(history, payload.MemoryID),
			EditedBy:    actor.ParticipantID,
		}),
		Metadata: map[string]any{},
	}
	return s.commitWith(ctx, cmd, actor, env)
}

// commit 原子落库 + 回执；回执竞态时重查回放（并发同键后到者）。
// 存储在事务内强制乐观并发（ExpectedRoomVersion）——本函数之上只做快速失败预检。
func (s *Service) commit(ctx context.Context, env protocol.Envelope, receipt CommandReceipt) (*CommandResult, error) {
	appended, err := s.cfg.Store.AppendWithReceipt(ctx, []protocol.Envelope{env}, receipt)
	if err != nil {
		if errors.Is(err, ErrDuplicateReceipt) { // 并发同键竞态：后到者按回放处理
			// 指纹比对不可跳过：同键异载荷是幂等冲突，静默回放等于吞掉冲突（RFC-0001）
			if res, rerr := s.replayByReceipt(ctx, receipt); res != nil || rerr != nil {
				return res, rerr
			}
			// 回放未果（非回执撞车而是其他唯一约束）：按原错误上抛，绝不 (nil,nil)
			return nil, fmt.Errorf("room: append: %w", err)
		}
		return nil, fmt.Errorf("room: append: %w", err)
	}
	return &CommandResult{
		RoomID:      appended[0].RoomID,
		EventID:     appended[0].EventID,
		RoomVersion: appended[0].Seq,
	}, nil
}

// replayIfReceived 已受理则回放；同键不同指纹报冲突；未受理返回 (nil, nil)。
func (s *Service) replayIfReceived(ctx context.Context, cmd Command, actor Actor) (*CommandResult, error) {
	return s.replayCore(ctx, cmd.IdempotencyKey, cmd.CommandKind, fingerprint(cmd, actor))
}

// replayByReceipt 竞态路径回放：以本方回执携带的指纹比对（commit 内唯一调用点）。
func (s *Service) replayByReceipt(ctx context.Context, receipt CommandReceipt) (*CommandResult, error) {
	return s.replayCore(ctx, receipt.IdempotencyKey, receipt.CommandKind, receipt.RequestFingerprint)
}

func (s *Service) replayCore(ctx context.Context, idemKey, kind, wantFingerprint string) (*CommandResult, error) {
	rc, err := s.cfg.Store.LookupReceipt(ctx, s.cfg.Tenant, idemKey, kind)
	if err != nil {
		return nil, fmt.Errorf("room: lookup receipt: %w", err)
	}
	if rc == nil {
		return nil, nil
	}
	if rc.RequestFingerprint != wantFingerprint {
		return nil, fmt.Errorf("%w: 同幂等键不同请求指纹", ErrIdempotencyConflict)
	}
	return &CommandResult{
		RoomID:      rc.RoomID,
		EventID:     rc.EventID,
		RoomVersion: rc.RoomVersion,
		Replayed:    true,
	}, nil
}

// fingerprint 请求指纹：命令身份要素的规范化哈希（版本不入指纹——回放与房间演进解耦）。
func fingerprint(cmd Command, actor Actor) string {
	h := sha256.New()
	for _, part := range []string{cmd.CommandKind, cmd.RoomID, actor.ParticipantID, actor.Kind, string(cmd.Payload)} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("room: marshal: " + err.Error())
	}
	return raw
}

// setPolicy 策略配置命令链（RFC-0003 §3.1.7 / R-10）：严格校验 → policy.changed。
// 版本并发语义同 pause/resume；变更只在 round 边界生效（引擎开轮自历史投影）。

// endorseIntent 人类保送命令链（RFC-0003 §3.1.11 / OQ-17）：intent.endorsed 事件
// （对全体可见；Agent 不能保送 Agent——外部命令 actor 恒 human）。effect 本切片
// 只受理 grant（直接授予 Floor；boost 语义依赖 Policy 加权参数定稿，未定不收）。
// 执行面（发授/生成/发布）由引擎消费 intent.endorsed 驱动——不绕过预算/硬资格。
func (s *Service) endorseIntent(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		IntentID string `json:"intent_id"`
		Effect   string `json:"effect"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: endorse_intent payload: %v", ErrInvalidCommand, err)
	}
	if !intentIDPattern.MatchString(payload.IntentID) {
		return nil, fmt.Errorf("%w: intent_id 形如 int_*", ErrInvalidCommand)
	}
	if payload.Effect != "grant" {
		return nil, fmt.Errorf("%w: effect %q 暂不可用（本切片仅 grant；boost 随 Policy 加权参数定稿开放）", ErrInvalidCommand, payload.Effect)
	}
	// 目标 Intent 必须已记录（保送的是已评估的意向——不虚构发言资格）
	if s.cfg.Reader != nil {
		events, _, err := s.cfg.Reader.EventsAfter(ctx, cmd.RoomID, "", 1000)
		if err == nil {
			found := false
			for _, ev := range events {
				if ev.Envelope.Type != protocol.EventIntentRecorded {
					continue
				}
				var p struct {
					IntentID string `json:"intent_id"`
				}
				if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.IntentID == payload.IntentID {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: intent %s 不存在", ErrInvalidCommand, payload.IntentID)
			}
		}
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventIntentEndorsed,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"}, // 对全体可见（OQ-17）
		Payload: mustJSON(protocol.IntentEndorsedPayload{
			IntentID:   payload.IntentID,
			EndorsedBy: actor.ParticipantID,
			Effect:     payload.Effect,
		}),
		Metadata: map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// roomLifecycle pause/resume 命令链：版本并发 + 事件落库（RFC-0001 room.paused/room.started）。
func (s *Service) roomLifecycle(ctx context.Context, actor Actor, cmd Command, eventType, payloadName string) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		// 复审 #22：同 post_message——并发同键竞态先重查回放再判冲突
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidCommand, payloadName, err)
	}
	if len([]rune(payload.Reason)) > 280 {
		return nil, fmt.Errorf("%w: reason 超 280 字", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          eventType,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       mustJSON(map[string]any{"reason": payload.Reason}),
		Metadata:      map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// renameRoom 房间改名命令链（UI 重设计切片 1）：与 pause/resume 同一纪律——
// 幂等回放优先、版本并发预检（提交事务内再强制）、事件落日志（room.renamed）。
// display_name 必填 1..120 字（上限同 create_room；改名不接受置空/纯空白）。
func (s *Service) renameRoom(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		// 复审 #22：同 post_message——并发同键竞态先重查回放再判冲突
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		DisplayName string `json:"display_name"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: rename_room payload: %v", ErrInvalidCommand, err)
	}
	if len([]rune(strings.TrimSpace(payload.DisplayName))) < 1 || len([]rune(payload.DisplayName)) > 120 {
		return nil, fmt.Errorf("%w: display_name 必填 1..120 字", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventRoomRenamed,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       mustJSON(map[string]any{"display_name": payload.DisplayName}),
		Metadata:      map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// ListRooms 房间列表只读路径（GET /v1/rooms）：存储聚合直传，服务层不做改写。
func (s *Service) ListRooms(ctx context.Context) ([]RoomSummary, error) {
	if s.cfg.Lister == nil {
		return nil, fmt.Errorf("room: 未装配 RoomLister")
	}
	return s.cfg.Lister.ListRooms(ctx)
}

// historyOf 全量房间事件（收束命令面读取；分页拉全）。
func (s *Service) historyOf(ctx context.Context, roomID string) ([]StoredEvent, error) {
	reader := s.cfg.Reader
	if reader == nil {
		if r, ok := s.cfg.Store.(EventReader); ok {
			reader = r
		}
	}
	if reader == nil {
		return nil, fmt.Errorf("room: reader 不可用")
	}
	var all []StoredEvent
	cursor := ""
	for {
		events, next, err := reader.EventsAfter(ctx, roomID, cursor, 1000)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
		if next == "" || len(events) == 0 {
			return all, nil
		}
		cursor = next
	}
}

// propose_closure 人类显式提议收束（M3-2 裁剪口径：唯一触发源；引擎经 outbox
// 驱动全员三态评估——合格异议中止，否则待人类接受）。
func (s *Service) proposeClosure(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		ThreadID    string `json:"thread_id"`
		ClosureHint string `json:"closure_hint"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: propose_closure payload: %v", ErrInvalidCommand, err)
	}
	if payload.ClosureHint != "" && payload.ClosureHint != "consensus" && payload.ClosureHint != "bounded_disagreement" {
		return nil, fmt.Errorf("%w: closure_hint 取值 consensus | bounded_disagreement", ErrInvalidCommand)
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	threadID := payload.ThreadID
	if threadID == "" {
		threadID = RootThreadOf(history)
	}
	if threadID == "" {
		return nil, fmt.Errorf("%w: 无目标线程且房间无根线程", ErrInvalidCommand)
	}
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	if ThreadStateOf(envs, threadID) != ThreadActive {
		return nil, fmt.Errorf("%w: 线程不在活跃态（活跃线程才可收束）", ErrInvalidCommand)
	}
	if _, pending := PendingClosureOf(history); pending {
		return nil, fmt.Errorf("%w: 已有待决收束（先接受或由合格异议中止）", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventClosureProposed,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.ClosureProposedPayload{
			ClosureID:   s.cfg.NewID("clo"),
			ThreadID:    threadID,
			Trigger:     "human",
			ClosureHint: payload.ClosureHint,
			Watermark:   version,
		}),
		Metadata: map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// accept_closure 人类接受收束：自事件流确定性组装 Capsule，与线程关闭同事务提交
// （收束即线程终态；重开走 reopen_thread——新证据留痕于重开首条消息）。
func (s *Service) acceptClosure(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		ClosureID string `json:"closure_id"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: accept_closure payload: %v", ErrInvalidCommand, err)
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	pending, ok := PendingClosureOf(history)
	if !ok || (payload.ClosureID != "" && payload.ClosureID != pending.ClosureID) {
		return nil, fmt.Errorf("%w: 无此待决收束", ErrInvalidCommand)
	}
	if !pending.Ready {
		return nil, fmt.Errorf("%w: 收束评估未完成（尚无表态，稍后再试）", ErrInvalidCommand)
	}
	capsule, ok := BuildCapsule(history, pending.ClosureID)
	if !ok {
		return nil, fmt.Errorf("%w: 胶囊组装失败（提议事件缺失）", ErrInvalidCommand)
	}
	mk := func(eventType, causation, correlation string, payload any) protocol.Envelope {
		return protocol.Envelope{
			EventID:       s.cfg.NewID("evt"),
			TenantID:      s.cfg.Tenant,
			RoomID:        cmd.RoomID,
			Type:          eventType,
			SchemaVersion: 1,
			OccurredAt:    s.cfg.Clock(),
			Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
			Visibility:    protocol.Visibility{Kind: "public"},
			Payload:       mustJSON(payload),
			Metadata:      map[string]any{},
		}
	}
	_ = mk // 占位防未用（下方直接构造，见 accepted/closed）
	accepted := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventClosureAccepted,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.ClosureAcceptedPayload{
			ClosureID:   pending.ClosureID,
			ClosureType: capsule.ClosureType,
			ThreadID:    capsule.ThreadID,
			Capsule:     capsule,
			AcceptedBy:  actor.ParticipantID,
		}),
		Metadata: map[string]any{},
	}
	causation := accepted.EventID
	closed := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		ThreadID:      &capsule.ThreadID,
		Type:          protocol.EventThreadClosed,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       mustJSON(map[string]any{"thread_id": capsule.ThreadID, "reason": "closure_accepted"}),
		Metadata:      map[string]any{},
	}
	closed.CausationID = &causation
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             accepted.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	appended, err := s.cfg.Store.AppendWithReceipt(ctx, []protocol.Envelope{accepted, closed}, receipt)
	if err != nil {
		if errors.Is(err, ErrDuplicateReceipt) {
			if res, rerr := s.replayByReceipt(ctx, receipt); res != nil || rerr != nil {
				return res, rerr
			}
		}
		return nil, fmt.Errorf("room: append: %w", err)
	}
	return &CommandResult{
		RoomID:      appended[0].RoomID,
		EventID:     appended[0].EventID,
		RoomVersion: appended[len(appended)-1].Seq,
	}, nil
}

// RootThreadOf 房间根线程（room.created payload 的 thread_id）。
func RootThreadOf(events []StoredEvent) string {
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventRoomCreated {
			continue
		}
		var p struct {
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil {
			return p.ThreadID
		}
	}
	return ""
}

// roomVersionPrecheck 通用前置：存在性 + 乐观并发（同键竞态回放优先）。
func (s *Service) roomVersionPrecheck(ctx context.Context, cmd Command, actor Actor) (*CommandResult, error) {
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
			return res, rerr
		}
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	return nil, nil
}

// commitWith 单事件提交 + 回执（证据需求单命令共用）。
func (s *Service) commitWith(ctx context.Context, cmd Command, actor Actor, env protocol.Envelope) (*CommandResult, error) {
	receipt := CommandReceipt{
		TenantID:            s.cfg.Tenant,
		RoomID:              cmd.RoomID,
		IdempotencyKey:      cmd.IdempotencyKey,
		CommandKind:         cmd.CommandKind,
		RequestFingerprint:  fingerprint(cmd, actor),
		EventID:             env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion,
		ExecutedAt:          s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// deleteRoom 删除房间（M3-6，RFC-0010 个人版）：先落墓碑事件（room.deleted——
// 全库审计可回溯"曾存在"），再级联清理（事件/outbox/回执/声明）——墓碑本身
// 随级联一并清除后以独立墓碑表留痕（SQLite 实现）；返回结果后房间不可再访问。
func (s *Service) deleteRoom(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	exists, err := s.cfg.Store.RoomExists(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room exists: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, cmd.RoomID)
	}
	version, err := s.cfg.Store.RoomVersion(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: room version: %w", err)
	}
	if cmd.ExpectedRoomVersion != version {
		return nil, fmt.Errorf("%w: expected=%d current=%d", ErrVersionConflict, cmd.ExpectedRoomVersion, version)
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: delete_room payload: %v", ErrInvalidCommand, err)
	}
	if len([]rune(payload.Reason)) < 1 || len([]rune(payload.Reason)) > 280 {
		return nil, fmt.Errorf("%w: reason 必填 1..280 字（删除不可逆，理由留痕）", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID:  s.cfg.NewID("evt"),
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventRoomDeleted, SchemaVersion: 1,
		OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    mustJSON(map[string]any{"room_id": cmd.RoomID, "reason": payload.Reason}),
		Metadata:   map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		IdempotencyKey: cmd.IdempotencyKey, CommandKind: cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor), EventID: env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion, ExecutedAt: s.cfg.Clock(),
	}
	res, err := s.commit(ctx, env, receipt)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.DeleteRoom(ctx, cmd.RoomID); err != nil {
		return nil, fmt.Errorf("room: delete cascade: %w", err)
	}
	return res, nil
}
