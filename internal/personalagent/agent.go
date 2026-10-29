// Package personalagent 独立 Agent 身份域（RFC-0018 §3.1 / ADR-0017，0.5.0）。
// Agent 是跨房间、跨部署位置的稳定主体；旧 ID（BotID / ProfileID /
// ParticipantID）收敛为别名，本域是身份权威；harness 注册表只持有对
// AgentID 的引用（单向同步：注册表 → 身份表，只增不改）。
package personalagent

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// newAgentID 缺省 AgentID 生成：ag_ + 时间有序毫秒（36 进制）+ 随机后缀
// （与装配层 newID 前缀族同构；生产装配注入 app 侧生成器）。
func newAgentID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "ag_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// nowUTC 缺省时钟（RFC3339Nano UTC）。
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// AliasKind 旧 ID 的三类形态——房间事件 / 座位 / 注册表三处 ID 的统一解析键。
type AliasKind string

const (
	AliasBotID         AliasKind = "bot_id"         // ADR-0013 稳定 BotID（身份等价键）
	AliasProfileID     AliasKind = "profile_id"     // prof_<adapter>_<key>（会话键）
	AliasParticipantID AliasKind = "participant_id" // par_<adapter>_<key>（房间名册）
)

// 生命周期状态（RFC-0018 §3.2 / ADR-0018）：
// active ↔ suspended；active|suspended → retiring → retired（唯一终态）。
// active 不代表正在运行——身份状态，不是进程状态。
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusRetiring  = "retiring"
	StatusRetired   = "retired"
)

// 事件类型（agent_events 流，migration v8）。
const (
	EventCreated       = "created"        // 建行事实（含 v7 存量回填）
	EventStatusChanged = "status_changed" // 生命周期转移
)

// AgentEvent 事件流事件（seq 全表单调升序 = 流序）。
type AgentEvent struct {
	Seq        int64           `json:"seq"`
	AgentID    string          `json:"agent_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	OccurredAt string          `json:"occurred_at"`
}

// createdPayload EventCreated 事件体。
type createdPayload struct {
	Source      string `json:"source"`
	Adapter     string `json:"adapter"`
	DisplayName string `json:"display_name"`
}

// statusChangedPayload EventStatusChanged 事件体。
type statusChangedPayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Agent 聚合投影（v8 起为 agent_events 流的投影——转移与事件同事务）。
type Agent struct {
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
	Adapter     string `json:"adapter"`
	Status      string `json:"status"` // 生命周期状态机（ADR-0018）
	Source      string `json:"source"` // migration | manual
	CreatedAt   string `json:"created_at"`
}

// Alias 已登记别名行。
type Alias struct {
	Kind  AliasKind `json:"kind"`
	Value string    `json:"value"`
}

// ExecutableSnapshot 注册表 → 身份域的可执行项视图（同步输入）。
type ExecutableSnapshot struct {
	ExeID   string
	BotID   string
	Adapter string
	Runtime string
	Distro  string
	Channel string
	Enabled bool
}

// ExecutableRef AgentView 里的可执行项引用（只读、现算）。
type ExecutableRef struct {
	ExecutableID string `json:"executable_id"`
	Runtime      string `json:"runtime"`
	Distro       string `json:"distro"`
	Channel      string `json:"channel"`
	Enabled      bool   `json:"enabled"`
}

// AliasSet 三向别名的视图形态（schema: IdentityAgent.aliases）。
type AliasSet struct {
	BotID          string   `json:"bot_id"`
	ProfileIDs     []string `json:"profile_ids"`
	ParticipantIDs []string `json:"participant_ids"`
}

// AgentView API 视图 = Agent + 别名集 + 可执行引用（schema: IdentityAgent）。
type AgentView struct {
	Agent
	Aliases     AliasSet        `json:"aliases"`
	Executables []ExecutableRef `json:"executables"`
}

// profileKey BotID → 座位键（与 app.go v1.66 内联式逐字节等价——单源化不改输出，
// 有 derivation_test 钉住；存量 participantID 的别名解析不漂移）。
func profileKey(botID string) string {
	var b strings.Builder
	for _, r := range botID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if out := b.String(); out != "" {
		return out
	}
	return "x"
}

// ProfileIDOf 座位 ProfileID 派生（单源；装配层消费）。
func ProfileIDOf(adapter, botID string) string {
	return "prof_" + adapter + "_" + profileKey(botID)
}

// ParticipantIDOf 座位 ParticipantID 派生（单源；装配层消费）。
func ParticipantIDOf(adapter, botID string) string {
	return "par_" + adapter + "_" + profileKey(botID)
}

// DisplayNameOf 迁移来源的展示名（adapter 家族名；与装配层座位 DisplayName 同源口径）。
func DisplayNameOf(adapter string) string {
	switch adapter {
	case "codex":
		return "Codex"
	case "kimi":
		return "Kimi"
	case "minimax":
		return "MiniMax"
	case "zcode":
		return "ZCode"
	case "claude":
		return "Claude"
	default:
		return adapter
	}
}
