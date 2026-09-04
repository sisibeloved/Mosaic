// Evidence Request（RFC-0005 §3.1.9，M3-5）：证据需求单——争议依赖外部事实时
// 创建，open → resolved/dismissed；满足后不自动重开（时间线提示人类 reopen_thread）。
// 命令面：create_evidence_request / resolve_evidence_request（dismiss 走同命令的
// resolution=dismissed）。纯事件溯源：投影自事件流重建（无独立表）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// EvidenceRequestView 证据需求单投影（快照 evidence_requests 视图）。
type EvidenceRequestView struct {
	RequestID          string   `json:"request_id"`
	ClaimID            string   `json:"claim_id,omitempty"`
	Question           string   `json:"question"`
	RequiredEvidence   []string `json:"required_evidence"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Owners             []string `json:"owners"`
	Status             string   `json:"status"` // open | resolved | dismissed
	EvidenceRefs       []string `json:"evidence_refs,omitempty"`
	ReopenOnResolution bool     `json:"reopen_thread_on_resolution"`
}

// EvidenceRequestsOf 自事件流重建（创建序；终态覆盖状态）。
func EvidenceRequestsOf(events []StoredEvent) []EvidenceRequestView {
	out := []EvidenceRequestView{}
	index := map[string]int{}
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventEvidenceRequestCreated:
			var p protocol.EvidenceRequestCreatedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			view := EvidenceRequestView{
				RequestID: p.RequestID, ClaimID: p.ClaimID, Question: p.Question,
				RequiredEvidence: p.RequiredEvidence, AcceptanceCriteria: p.AcceptanceCriteria,
				Owners: p.Owners, Status: "open", ReopenOnResolution: p.ReopenOnResolution,
			}
			if view.RequiredEvidence == nil {
				view.RequiredEvidence = []string{}
			}
			if view.Owners == nil {
				view.Owners = []string{}
			}
			if _, dup := index[p.RequestID]; !dup {
				index[p.RequestID] = len(out)
				out = append(out, view)
			}
		case protocol.EventEvidenceRequestResolved:
			var p protocol.EvidenceRequestResolvedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if i, ok := index[p.RequestID]; ok {
				out[i].Status = p.Resolution
				out[i].EvidenceRefs = p.EvidenceRefs
			}
		case protocol.EventEvidenceRequestClaimed:
			var p protocol.EvidenceRequestClaimedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if i, ok := index[p.RequestID]; ok && out[i].Status == "open" {
				dup := false
				for _, o := range out[i].Owners {
					if o == p.ClaimedBy {
						dup = true
						break
					}
				}
				if !dup {
					out[i].Owners = append(out[i].Owners, p.ClaimedBy)
				}
			}
		}
	}
	return out
}

// createEvidenceRequest 创建证据需求单（question 必填；owners 可空=人类待认领）。
func (s *Service) createEvidenceRequest(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		ClaimID            string   `json:"claim_id"`
		Question           string   `json:"question"`
		RequiredEvidence   []string `json:"required_evidence"`
		AcceptanceCriteria string   `json:"acceptance_criteria"`
		Owners             []string `json:"owners"`
		ReopenOnResolution bool     `json:"reopen_thread_on_resolution"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: create_evidence_request payload: %v", ErrInvalidCommand, err)
	}
	if q := strings.TrimSpace(payload.Question); len([]rune(q)) < 1 || len([]rune(q)) > 280 {
		return nil, fmt.Errorf("%w: question 必填 1..280 字", ErrInvalidCommand)
	}
	if len([]rune(payload.AcceptanceCriteria)) > 280 {
		return nil, fmt.Errorf("%w: acceptance_criteria 上限 280 字", ErrInvalidCommand)
	}
	if payload.RequiredEvidence == nil {
		payload.RequiredEvidence = []string{}
	}
	if payload.Owners == nil {
		payload.Owners = []string{}
	}
	env := protocol.Envelope{
		EventID:  s.cfg.NewID("evt"),
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventEvidenceRequestCreated, SchemaVersion: 1,
		OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.EvidenceRequestCreatedPayload{
			RequestID: s.cfg.NewID("ereq"), ClaimID: payload.ClaimID,
			Question: payload.Question, RequiredEvidence: payload.RequiredEvidence,
			AcceptanceCriteria: payload.AcceptanceCriteria, Owners: payload.Owners,
			ReopenOnResolution: payload.ReopenOnResolution,
		}),
		Metadata: map[string]any{},
	}
	return s.commitWith(ctx, cmd, actor, env)
}

// claimEvidenceRequest 认领证据需求单（M3-5 v1.54 补齐）：owners 空=人类待认领，
// 认领即追加（open 态、非重复）；claimant 为命令 actor（命令面仅 human/system
// ——agent 自主认领随工具面分期，当前由人类认领/指派）。
func (s *Service) claimEvidenceRequest(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		RequestID string `json:"request_id"`
		Note      string `json:"note"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: claim_evidence_request payload: %v", ErrInvalidCommand, err)
	}
	if len([]rune(payload.Note)) > 280 {
		return nil, fmt.Errorf("%w: note 上限 280 字", ErrInvalidCommand)
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	reqs := EvidenceRequestsOf(history)
	found := false
	for _, r := range reqs {
		if r.RequestID == payload.RequestID {
			found = true
			if r.Status != "open" {
				return nil, fmt.Errorf("%w: 需求单已 %s（不可认领）", ErrInvalidCommand, r.Status)
			}
			for _, o := range r.Owners {
				if o == actor.ParticipantID {
					return nil, fmt.Errorf("%w: 已是认领人（不可重复认领）", ErrInvalidCommand)
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: 无此证据需求单 %s", ErrInvalidCommand, payload.RequestID)
	}
	env := protocol.Envelope{
		EventID:  s.cfg.NewID("evt"),
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventEvidenceRequestClaimed, SchemaVersion: 1,
		OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.EvidenceRequestClaimedPayload{
			RequestID: payload.RequestID, ClaimedBy: actor.ParticipantID, Note: payload.Note,
		}),
		Metadata: map[string]any{},
	}
	return s.commitWith(ctx, cmd, actor, env)
}

// resolveEvidenceRequest 解决/驳回证据需求单（resolution=resolved|dismissed）。
func (s *Service) resolveEvidenceRequest(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		RequestID    string   `json:"request_id"`
		Resolution   string   `json:"resolution"`
		EvidenceRefs []string `json:"evidence_refs"`
		Note         string   `json:"note"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: resolve_evidence_request payload: %v", ErrInvalidCommand, err)
	}
	if payload.Resolution == "" {
		payload.Resolution = "resolved"
	}
	if payload.Resolution != "resolved" && payload.Resolution != "dismissed" {
		return nil, fmt.Errorf("%w: resolution 取值 resolved | dismissed", ErrInvalidCommand)
	}
	history, err := s.historyOf(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: history: %w", err)
	}
	reqs := EvidenceRequestsOf(history)
	found := false
	for _, r := range reqs {
		if r.RequestID == payload.RequestID {
			found = true
			if r.Status != "open" {
				return nil, fmt.Errorf("%w: 需求单已 %s（不可重复解决）", ErrInvalidCommand, r.Status)
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: 无此证据需求单 %s", ErrInvalidCommand, payload.RequestID)
	}
	if payload.Resolution == "resolved" && len(payload.EvidenceRefs) == 0 {
		return nil, fmt.Errorf("%w: resolved 必须携带 evidence_refs（可解析引用）", ErrInvalidCommand)
	}
	if payload.EvidenceRefs == nil {
		payload.EvidenceRefs = []string{}
	}
	env := protocol.Envelope{
		EventID:  s.cfg.NewID("evt"),
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventEvidenceRequestResolved, SchemaVersion: 1,
		OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.EvidenceRequestResolvedPayload{
			RequestID: payload.RequestID, EvidenceRefs: payload.EvidenceRefs,
			Resolution: payload.Resolution, Note: payload.Note,
		}),
		Metadata: map[string]any{},
	}
	return s.commitWith(ctx, cmd, actor, env)
}
