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
	Clock  func() string              // RFC3339
	NewID  func(prefix string) string // 事件/房间 ID 生成（前缀 evt_/room_）
	Tenant string
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
	default:
		return nil, fmt.Errorf("%w: 未知命令 %q", ErrInvalidCommand, cmd.CommandKind)
	}
}

func (s *Service) createRoom(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if cmd.RoomID != "" {
		return nil, fmt.Errorf("%w: create_room 不接受 room_id", ErrInvalidCommand)
	}
	var payload struct {
		DisplayName string `json:"display_name"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: create_room payload: %v", ErrInvalidCommand, err)
	}
	if len([]rune(payload.DisplayName)) > 120 {
		return nil, fmt.Errorf("%w: display_name 超 120 字", ErrInvalidCommand)
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
		Payload:       mustJSON(map[string]any{"display_name": payload.DisplayName, "thread_id": rootThread}),
		Metadata:      map[string]any{},
	}
	receipt := CommandReceipt{
		TenantID:           s.cfg.Tenant,
		RoomID:             roomID,
		IdempotencyKey:     cmd.IdempotencyKey,
		CommandKind:        cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor),
		EventID:            env.EventID,
		ExecutedAt:         s.cfg.Clock(),
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
		TenantID:           s.cfg.Tenant,
		RoomID:             cmd.RoomID,
		IdempotencyKey:     cmd.IdempotencyKey,
		CommandKind:        cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor),
		EventID:            env.EventID,
		ExecutedAt:         s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}

// postMessagePayload 消息命令载荷（严格字段集：多余字段拒绝）。
type postMessagePayload struct {
	Body        string   `json:"body"`
	ReplyTo     *string  `json:"reply_to"`
	AddressedTo []string `json:"addressed_to"`
	Relations   []any    `json:"relations"`
	ThreadID    *string  `json:"thread_id"` // 可选：发往指定线程（根线程随 room.created 载荷）
}

var threadIDPattern = regexp.MustCompile(`^thr_[0-9A-Za-z_-]+$`)

// commit 原子落库 + 回执；回执竞态时重查回放（并发同键后到者）。
func (s *Service) commit(ctx context.Context, env protocol.Envelope, receipt CommandReceipt) (*CommandResult, error) {
	appended, err := s.cfg.Store.AppendWithReceipt(ctx, []protocol.Envelope{env}, receipt)
	if err != nil {
		if errors.Is(err, ErrDuplicateReceipt) { // 并发同键竞态：后到者按回放处理
			if res, rerr := s.replayIfReceived(ctx, Command{
				RoomID:         receipt.RoomID,
				CommandKind:    receipt.CommandKind,
				IdempotencyKey: receipt.IdempotencyKey,
				Payload:        nil,
			}, Actor{ParticipantID: "", Kind: "human"}); res != nil || rerr != nil { // actor 置空：跳过指纹比对（上游已比对过）
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
	rc, err := s.cfg.Store.LookupReceipt(ctx, s.cfg.Tenant, cmd.IdempotencyKey, cmd.CommandKind)
	if err != nil {
		return nil, fmt.Errorf("room: lookup receipt: %w", err)
	}
	if rc == nil {
		return nil, nil
	}
	if actor.ParticipantID != "" && rc.RequestFingerprint != fingerprint(cmd, actor) {
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
		TenantID:           s.cfg.Tenant,
		RoomID:             cmd.RoomID,
		IdempotencyKey:     cmd.IdempotencyKey,
		CommandKind:        cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor),
		EventID:            env.EventID,
		ExecutedAt:         s.cfg.Clock(),
	}
	return s.commit(ctx, env, receipt)
}
