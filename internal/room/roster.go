// 房间成员 roster（RFC-0001 Membership 族最小落地）：create_room 可选 agents
// 选择（缺省 = 物化当时在席名单，v1.24）+ invite_agent → participant.admitted。
// 引擎按 roster 过滤全局座位——房间讨论面 = 名单内且在席者；名单内座位未启用时
// 后续启用即入房。旧房间无 agents 载荷 → 历史推导（见 RosterOf）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// RosterOf 房间成员投影：room.created.payload.agents（v1.24 起含缺省物化快照）+
// participant.admitted 链。
// 旧房间（v1.24 前建房、无 agents 载荷）：从历史推导——曾出现在房间里的 agent
// （消息/意向/授予的参与者）即为名单（dogfood v1.25：动态全席对存量活房间同样
// 不可接受）；从未有 agent 参与的历史（空转旧房/测试夹具）回退 nil = 全部在席
// （首轮兼容：历史无信息量时维持旧语义，首轮之后即有推导名单）。
func RosterOf(envs []protocol.Envelope) map[string]bool {
	roster := map[string]bool{}
	explicit := false
	for _, env := range envs {
		switch env.Type {
		case protocol.EventRoomCreated:
			var p struct {
				Agents []string `json:"agents"`
			}
			if json.Unmarshal(env.Payload, &p) == nil && len(p.Agents) > 0 {
				explicit = true
				for _, a := range p.Agents {
					roster[a] = true
				}
			}
		case protocol.EventParticipantAdmitted:
			var p struct {
				ParticipantID string `json:"participant_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil && p.ParticipantID != "" {
				roster[p.ParticipantID] = true
				explicit = true
			}
		}
	}
	if explicit {
		return roster
	}
	// 旧房间：历史推导（消息 actor 为 agent 的参与者 + 意向/授予载荷目标）
	derived := map[string]bool{}
	for _, env := range envs {
		if env.Actor.Kind == "agent" && participantIDPattern.MatchString(env.Actor.ParticipantID) {
			derived[env.Actor.ParticipantID] = true
		}
		switch env.Type {
		case protocol.EventIntentRecorded, protocol.EventFloorGranted:
			var p struct {
				ParticipantID string `json:"participant_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil && participantIDPattern.MatchString(p.ParticipantID) {
				derived[p.ParticipantID] = true
			}
		}
	}
	if len(derived) > 0 {
		return derived
	}
	return nil // 无 agent 历史：全部在席（首轮兼容）
}

// inviteAgent 拉人命令（RFC-0001 Membership：participant.admitted；人类 actor）。
// 不校验目标在席（登记制：座位启用后即入房）——校验 participant ID 形态。
func (s *Service) inviteAgent(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
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
		ParticipantID string `json:"participant_id"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: invite_agent payload: %v", ErrInvalidCommand, err)
	}
	if !participantIDPattern.MatchString(payload.ParticipantID) {
		return nil, fmt.Errorf("%w: participant_id 形如 par_*", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID:       s.cfg.NewID("evt"),
		TenantID:      s.cfg.Tenant,
		RoomID:        cmd.RoomID,
		Type:          protocol.EventParticipantAdmitted,
		SchemaVersion: 1,
		OccurredAt:    s.cfg.Clock(),
		Actor:         protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload: mustJSON(map[string]any{
			"participant_id": payload.ParticipantID,
			"invited_by":     actor.ParticipantID,
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
