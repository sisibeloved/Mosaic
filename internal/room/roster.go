// 房间成员 roster（RFC-0001 Membership 族最小落地）：create_room 可选 agents
// 选择（缺省全部在席）+ invite_agent → participant.admitted。引擎按 roster 过滤
// 全局座位——房间讨论面 = 选中者；roster 中的座位未启用时后续启用即入房。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// RosterOf 房间成员投影：room.created.payload.agents（空/缺省 = 全部）+
// participant.admitted 链。空集语义 = 未选择（全部在席，向后兼容）。
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
	if !explicit {
		return nil // 全部在席
	}
	return roster
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
