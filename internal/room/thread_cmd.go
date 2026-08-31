// Thread 生命周期命令族（RFC-0004 §3.1.5/3.1.6）：fork/pause/resume/close/
// reopen/merge。状态机转移在命令面校验（投影端容错双保险）；merge 在个人版
// 单 owner 形态下为直接命令（owner 即 OQ-05 的确认权——propose/accept 双步
// 为多人类协作形态语义，登记）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ExecuteThreadCommand thread 命令分派（service 调用）。
func (s *Service) executeThreadCommand(ctx context.Context, actor Actor, cmd Command, eventType string) (*CommandResult, error) {
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

	// 线程投影（转移合法性 + 目标存在性）
	var threads map[string]*ThreadView
	if s.cfg.Reader != nil {
		events, _, rerr := s.cfg.Reader.EventsAfter(ctx, cmd.RoomID, "", 1000)
		if rerr == nil {
			envs := make([]protocol.Envelope, len(events))
			for i := range events {
				envs[i] = events[i].Envelope
			}
			threads, _ = RebuildThreads(envs)
		}
	}

	payload := protocol.ThreadLifecyclePayload{Reason: ""}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(cmd.Payload, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s payload: %v", ErrInvalidCommand, cmd.CommandKind, err)
	}
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %s payload: %v", ErrInvalidCommand, cmd.CommandKind, err)
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
		Metadata:      map[string]any{},
	}

	switch cmd.CommandKind {
	case "fork_thread":
		var p struct {
			SourceEventID string   `json:"source_event_id"`
			Goal          string   `json:"goal"`
			Participants  []string `json:"participants"`
		}
		strictDecode(cmd.Payload, &p)
		if p.SourceEventID == "" || !eventIDPattern.MatchString(p.SourceEventID) {
			return nil, fmt.Errorf("%w: source_event_id 形如 evt_*", ErrInvalidCommand)
		}
		if len([]rune(strings.TrimSpace(p.Goal))) < 1 || len([]rune(p.Goal)) > 280 {
			return nil, fmt.Errorf("%w: fork 目标（goal）必填 1..280 字", ErrInvalidCommand)
		}
		// 源事件定位其线程（fork 的父线程）；找不到源事件即拒
		parentThread := ""
		if s.cfg.Reader != nil {
			events, _, _ := s.cfg.Reader.EventsAfter(ctx, cmd.RoomID, "", 1000)
			for _, ev := range events {
				if ev.Envelope.EventID == p.SourceEventID {
					if ev.Envelope.ThreadID != nil && *ev.Envelope.ThreadID != "" {
						parentThread = *ev.Envelope.ThreadID
					}
					break
				}
			}
			if parentThread == "" {
				// room.created 载根线程兜底
				for _, ev := range events {
					if ev.Envelope.Type == protocol.EventRoomCreated {
						var rp struct {
							ThreadID string `json:"thread_id"`
						}
						if json.Unmarshal(ev.Envelope.Payload, &rp) == nil && rp.ThreadID != "" {
							parentThread = rp.ThreadID
						}
						break
					}
				}
			}
		}
		payload = protocol.ThreadLifecyclePayload{
			ThreadID:       s.cfg.NewID("thr"),
			ParentThreadID: parentThread,
			SourceEventID:  p.SourceEventID,
			Goal:           p.Goal,
			Participants:   p.Participants,
		}
	case "pause_thread", "resume_thread", "close_thread", "reopen_thread":
		var p struct {
			ThreadID string `json:"thread_id"`
			Reason   string `json:"reason"`
		}
		strictDecode(cmd.Payload, &p)
		if !threadIDPattern.MatchString(p.ThreadID) {
			return nil, fmt.Errorf("%w: thread_id 形如 thr_*", ErrInvalidCommand)
		}
		if threads != nil {
			th, ok := threads[p.ThreadID]
			if !ok {
				return nil, fmt.Errorf("%w: 线程 %s 不存在", ErrInvalidCommand, p.ThreadID)
			}
			if _, err := threadStateTransition(th.State, eventType); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
			}
		}
		payload = protocol.ThreadLifecyclePayload{ThreadID: p.ThreadID, Reason: p.Reason}
	case "merge_thread":
		var p struct {
			ThreadID   string `json:"thread_id"`
			MergedInto string `json:"merged_into"`
		}
		strictDecode(cmd.Payload, &p)
		if !threadIDPattern.MatchString(p.ThreadID) || !threadIDPattern.MatchString(p.MergedInto) {
			return nil, fmt.Errorf("%w: thread_id/merged_into 形如 thr_*", ErrInvalidCommand)
		}
		if p.ThreadID == p.MergedInto {
			return nil, fmt.Errorf("%w: 不能合并到自身", ErrInvalidCommand)
		}
		if threads != nil {
			th, ok := threads[p.ThreadID]
			if !ok {
				return nil, fmt.Errorf("%w: 线程 %s 不存在", ErrInvalidCommand, p.ThreadID)
			}
			if _, err := threadStateTransition(th.State, eventType); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
			}
			if target, ok := threads[p.MergedInto]; !ok || target.State == ThreadMerged {
				return nil, fmt.Errorf("%w: 合并目标 %s 不存在或已合并", ErrInvalidCommand, p.MergedInto)
			}
		}
		payload = protocol.ThreadLifecyclePayload{ThreadID: p.ThreadID, MergedInto: p.MergedInto}
	default:
		return nil, fmt.Errorf("%w: 未知 thread 命令 %q", ErrInvalidCommand, cmd.CommandKind)
	}

	env.Payload = mustJSON(payload)
	if env.ThreadID == nil && payload.ThreadID != "" && cmd.CommandKind != "fork_thread" {
		tid := payload.ThreadID
		env.ThreadID = &tid // 生命周期事件归属其线程（fork 除外——新线程由 payload 载）
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

// strictDecode 严格字段集解码。
func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
