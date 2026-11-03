// Package reopen：决策重开请求（0.6.11 / ADR-0034，RFC-0018 §10.2/§11.2）。
// 触及决策的 falsifier 出现时，负责人提交 reopen_request——事实先落
// （mission_events 追加 reopen_requested，append-only 不改写旧回执），逐项暂停
// 受影响工作项并如实报告；交回 Room 复用回房通知（ADR-0033 第五类事实）。
// 不自动重开 Room、不翻转 Mission 终态（任何状态可发起）；Room 不可用时由
// 读面与回房台账呈现"任务中心待决"。
package reopen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// ErrInvalidReopen 形状/上限不合法。
var ErrInvalidReopen = fmt.Errorf("reopen: 不合法的重开请求")

// ErrMissionNotFound 任务不存在（转译 mission 域语义供传输层映射 404）。
var ErrMissionNotFound = mission.ErrMissionNotFound

// Missions mission 域端口（事实 + 读面）。
type Missions interface {
	Get(ctx context.Context, missionID string) (mission.Mission, bool, error)
	// ReopenRequest CAS 版本（任意状态）+ reopen_requested 事件；版本不符
	// ErrVersionConflict（mission.Service 签名——构造与落库归 mission 域）。
	ReopenRequest(ctx context.Context, missionID string, expectedVersion int,
		requestID, decisionRef string, evidence, affected []string, reason string) (mission.Mission, error)
	MissionEvents(ctx context.Context, missionID string) ([]mission.Event, bool, error)
}

// Pauser 暂停面端口（0.6.6 / ADR-0029——missioncontrol 实现）。
type Pauser interface {
	PauseMission(ctx context.Context, missionID string, expectedVersion int, reason string) (mission.Mission, error)
}

// SubmitRequest 提交面（§11.2 reopen_request）。
type SubmitRequest struct {
	ExpectedVersion    int      `json:"expected_version"`
	DecisionRef        string   `json:"decision_ref"`
	EvidenceRefs       []string `json:"evidence_refs"`
	AffectedMissionIDs []string `json:"affected_mission_ids"`
	Reason             string   `json:"reason"`
}

// AffectedResult 逐项暂停结果（如实报告——失败不回滚请求事实）。
type AffectedResult struct {
	MissionID string `json:"mission_id"`
	Result    string `json:"result"` // paused | already_paused | terminal | not_found | failed
	Detail    string `json:"detail,omitempty"`
}

// RequestView 请求事实（事件流组装；Affected 仅提交响应携带）。
type RequestView struct {
	RequestID          string           `json:"request_id"`
	MissionID          string           `json:"mission_id"`
	DecisionRef        string           `json:"decision_ref"`
	EvidenceRefs       []string         `json:"evidence_refs"`
	AffectedMissionIDs []string         `json:"affected_mission_ids"`
	Reason             string           `json:"reason,omitempty"`
	ContractVersion    int              `json:"contract_version"`
	OccurredAt         string           `json:"occurred_at"`
	Affected           []AffectedResult `json:"affected,omitempty"`
}

// Service 编排：事实先落 → 逐项暂停 → 视图返回。
type Service struct {
	missions     Missions
	pauser       Pauser
	now          func() string
	newRequestID func(prefix string) string
}

// Option 装配选项。
type Option func(*Service)

// WithNow 注入时钟。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewRequestID 注入请求 ID 生成（rop_ 前缀）。
func WithNewRequestID(f func(prefix string) string) Option {
	return func(s *Service) { s.newRequestID = f }
}

// New 构造。
func New(m Missions, p Pauser, opts ...Option) *Service {
	s := &Service{missions: m, pauser: p,
		now:          func() string { return "1970-01-01T00:00:00Z" },
		newRequestID: func(string) string { return "" }}
	for _, o := range opts {
		o(s)
	}
	return s
}

func blankTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
}

func validRefLines(name string, lines []string, min, max, limit int) error {
	if len(lines) < min || len(lines) > max {
		return fmt.Errorf("%w: %s %d..%d 条", ErrInvalidReopen, name, min, max)
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(blankTrim(l)); n < 1 || n > limit {
			return fmt.Errorf("%w: %s[%d] 须 1..%d 字", ErrInvalidReopen, name, i, limit)
		}
	}
	return nil
}

// Submit 提交重开请求：校验 → CAS 落 reopen_requested 事件（Mission 状态不变）→
// 逐项暂停（reason=reopen:<request_id> 回链）→ 视图（含逐项结果）。
func (s *Service) Submit(ctx context.Context, missionID string, req SubmitRequest) (RequestView, error) {
	if n := utf8.RuneCountInString(blankTrim(req.DecisionRef)); n < 1 || n > 256 {
		return RequestView{}, fmt.Errorf("%w: decision_ref 须 1..256 字", ErrInvalidReopen)
	}
	if err := validRefLines("evidence_refs", req.EvidenceRefs, 1, 8, 256); err != nil {
		return RequestView{}, err
	}
	if len(req.AffectedMissionIDs) > 16 {
		return RequestView{}, fmt.Errorf("%w: affected_mission_ids ≤16", ErrInvalidReopen)
	}
	if utf8.RuneCountInString(req.Reason) > 280 {
		return RequestView{}, fmt.Errorf("%w: reason ≤280 字", ErrInvalidReopen)
	}
	if _, ok, err := s.missions.Get(ctx, missionID); err != nil {
		return RequestView{}, err
	} else if !ok {
		return RequestView{}, ErrMissionNotFound
	}
	requestID := s.newRequestID("rop")
	if _, err := s.missions.ReopenRequest(ctx, missionID, req.ExpectedVersion,
		requestID, blankTrim(req.DecisionRef), req.EvidenceRefs, req.AffectedMissionIDs, req.Reason); err != nil {
		return RequestView{}, err
	}
	view := RequestView{
		RequestID: requestID, MissionID: missionID,
		DecisionRef: blankTrim(req.DecisionRef), EvidenceRefs: req.EvidenceRefs,
		AffectedMissionIDs: req.AffectedMissionIDs, Reason: req.Reason,
		ContractVersion: req.ExpectedVersion, OccurredAt: s.now(),
	}
	// 审校 #2：事实已落地——暂停步骤用**独立上下文**（请求断线不丢暂停；
	// 与 0.6.6 取消两段的独立上下文同型）。
	pauseCtx := context.Background()
	for _, id := range req.AffectedMissionIDs {
		view.Affected = append(view.Affected, s.pauseAffected(pauseCtx, id, requestID))
	}
	return view, nil
}

// pauseAffected 单项暂停（状态映射 + 一次版本重读重试；失败不阻断其余项）。
func (s *Service) pauseAffected(ctx context.Context, id, requestID string) AffectedResult {
	cur, ok, err := s.missions.Get(ctx, id)
	if err != nil {
		return AffectedResult{MissionID: id, Result: "failed", Detail: err.Error()}
	}
	if !ok {
		return AffectedResult{MissionID: id, Result: "not_found"}
	}
	switch cur.Status {
	case mission.StatusPaused:
		// 审校 #3：已 paused 仍过控制面（幂等）——首次暂停后停止信号可能未送达
		//（执行器仍在跑），重试须重发停止信号（missioncontrol.PauseMission 幂等：
		// 已 paused 时跳过 CAS 但仍调 SignalStopByMission）。
		if _, err := s.pauser.PauseMission(ctx, id, cur.ContractVersion, "reopen:"+requestID); err != nil && !isVersionConflict(err) {
			return AffectedResult{MissionID: id, Result: "failed", Detail: "停止信号补发失败：" + err.Error()}
		}
		return AffectedResult{MissionID: id, Result: "already_paused"}
	case mission.StatusReady, mission.StatusRunning, mission.StatusWaiting:
		reason := "reopen:" + requestID
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := s.pauser.PauseMission(ctx, id, cur.ContractVersion, reason); err == nil {
				return AffectedResult{MissionID: id, Result: "paused"}
			} else if !isVersionConflict(err) {
				return AffectedResult{MissionID: id, Result: "failed", Detail: err.Error()}
			}
			// 版本竞态（并发修订）：重读版本再试一次
			again, ok2, gerr := s.missions.Get(ctx, id)
			if gerr != nil || !ok2 {
				d := "任务已不存在"
				if gerr != nil {
					d = gerr.Error()
				}
				return AffectedResult{MissionID: id, Result: "failed", Detail: d}
			}
			cur = again
		}
		return AffectedResult{MissionID: id, Result: "failed", Detail: "版本冲突重试耗尽"}
	case mission.StatusAccepted, mission.StatusCanceled:
		return AffectedResult{MissionID: id, Result: "terminal", Detail: string(cur.Status)}
	default:
		// 审校 #4：draft（从未调度）/ reviewing（冻结中）/ canceling（停止进行中）
		//——非终态但不可暂停，如实报告状态而非笼统 terminal。
		return AffectedResult{MissionID: id, Result: "not_pausable", Detail: string(cur.Status)}
	}
}

func isVersionConflict(err error) bool {
	return errors.Is(err, mission.ErrVersionConflict)
}

// List 读面：reopen_requested 事件按 seq 升序组装（不回填逐项结果——留痕在各
// 任务 paused 事件的 reason=reopen:<request_id>）。
func (s *Service) List(ctx context.Context, missionID string) ([]RequestView, error) {
	if _, ok, err := s.missions.Get(ctx, missionID); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrMissionNotFound
	}
	evs, _, err := s.missions.MissionEvents(ctx, missionID)
	if err != nil {
		return nil, err
	}
	out := []RequestView{}
	for _, e := range evs {
		if e.EventType != mission.EventReopenRequested {
			continue
		}
		var p struct {
			RequestID          string   `json:"request_id"`
			DecisionRef        string   `json:"decision_ref"`
			EvidenceRefs       []string `json:"evidence_refs"`
			AffectedMissionIDs []string `json:"affected_mission_ids"`
			Reason             string   `json:"reason"`
			ContractVersion    int      `json:"contract_version"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.RequestID == "" {
			continue
		}
		if p.EvidenceRefs == nil {
			p.EvidenceRefs = []string{}
		}
		if p.AffectedMissionIDs == nil {
			p.AffectedMissionIDs = []string{}
		}
		out = append(out, RequestView{
			RequestID: p.RequestID, MissionID: missionID, DecisionRef: p.DecisionRef,
			EvidenceRefs: p.EvidenceRefs, AffectedMissionIDs: p.AffectedMissionIDs,
			Reason: p.Reason, ContractVersion: p.ContractVersion, OccurredAt: e.OccurredAt,
		})
	}
	return out, nil
}
