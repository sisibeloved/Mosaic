package reopen

// 0.6.11 / ADR-0034：重开请求编排——事实先落（reopen_requested 事件），逐项
// 暂停受影响工作项并如实报告（paused / already_paused / terminal / not_found /
// failed）；不自动重开 Room、不翻转 Mission 终态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// ---- 测试假件 ----

type fakeMissions struct {
	byID    map[string]mission.Mission
	events  map[string][]mission.Event
	reopen  map[string]int // 调用计数
	failOn  string
	failErr error
}

func (f *fakeMissions) Get(_ context.Context, id string) (mission.Mission, bool, error) {
	m, ok := f.byID[id]
	return m, ok, nil
}

func (f *fakeMissions) ReopenRequest(_ context.Context, id string, expectedVersion int,
	requestID, decisionRef string, evidence, affected []string, reason string) (mission.Mission, error) {
	f.reopen[id]++
	if f.failOn == id {
		return mission.Mission{}, f.failErr
	}
	m, ok := f.byID[id]
	if !ok {
		return mission.Mission{}, mission.ErrMissionNotFound
	}
	if m.ContractVersion != expectedVersion {
		return mission.Mission{}, mission.ErrVersionConflict
	}
	payload, _ := json.Marshal(map[string]any{
		"request_id": requestID, "decision_ref": decisionRef,
		"evidence_refs": evidence, "affected_mission_ids": affected,
		"reason": reason, "contract_version": expectedVersion,
	})
	f.events[id] = append(f.events[id], mission.Event{
		MissionID: id, EventType: mission.EventReopenRequested,
		Payload: payload, OccurredAt: "2026-11-03T12:00:00.000Z", Seq: int64(len(f.events[id]) + 1),
	})
	return m, nil
}

func (f *fakeMissions) MissionEvents(_ context.Context, id string) ([]mission.Event, bool, error) {
	return f.events[id], true, nil
}

type fakePauser struct {
	calls    []string
	paused   map[string]bool
	failOn   string
	failErr  error
	versions map[string]int
}

func (f *fakePauser) PauseMission(_ context.Context, id string, expectedVersion int, reason string) (mission.Mission, error) {
	if f.failOn == id {
		return mission.Mission{}, f.failErr
	}
	m := mission.Mission{MissionID: id, Status: mission.StatusPaused, ContractVersion: expectedVersion}
	f.calls = append(f.calls, id+"|"+reason)
	if !f.paused[id] {
		f.paused[id] = true
	}
	return m, nil
}

var errPauseTransition = fmt.Errorf("%w: 状态不允许", mission.ErrInvalidTransition)

func newTestService(m *fakeMissions, p *fakePauser) *Service {
	return New(m, p,
		WithNow(func() string { return "2026-11-03T12:00:00.000Z" }),
		WithNewRequestID(func(prefix string) string { return prefix + "_t1" }))
}

func mkMission(id string, status string, version int) mission.Mission {
	return mission.Mission{MissionID: id, Status: status, ContractVersion: version,
		OwnerAgentID: "ag_a", Content: mission.Content{Objective: "目标"}}
}

// 提交全链：事实留痕 + 逐项暂停结果五型 + 状态不变。
func TestSubmitResults(t *testing.T) {
	m := &fakeMissions{
		byID: map[string]mission.Mission{
			"mi_src":  mkMission("mi_src", mission.StatusReady, 1),
			"mi_run":  mkMission("mi_run", mission.StatusRunning, 3),
			"mi_p":    mkMission("mi_p", mission.StatusPaused, 1),
			"mi_term": mkMission("mi_term", mission.StatusAccepted, 2),
			"mi_cz":   mkMission("mi_cz", mission.StatusCanceling, 1),
			"mi_drft": mkMission("mi_drft", mission.StatusDraft, 1),
			"mi_rv":   mkMission("mi_rv", mission.StatusReviewing, 1),
			"mi_wait": mkMission("mi_wait", mission.StatusWaiting, 1),
		},
		events: map[string][]mission.Event{},
		reopen: map[string]int{},
	}
	p := &fakePauser{paused: map[string]bool{}}
	s := newTestService(m, p)

	out, err := s.Submit(context.Background(), "mi_src", SubmitRequest{
		ExpectedVersion: 1, DecisionRef: "closure:r:seq9",
		EvidenceRefs: []string{"doc:d:r1"}, Reason: "反证",
		AffectedMissionIDs: []string{"mi_run", "mi_p", "mi_term", "mi_none", "mi_cz", "mi_drft", "mi_rv", "mi_wait"},
	})
	if err != nil {
		t.Fatalf("提交: %v", err)
	}
	if out.RequestID != "rop_t1" || out.DecisionRef != "closure:r:seq9" || out.ContractVersion != 1 {
		t.Fatalf("视图: %+v", out)
	}
	if len(m.events["mi_src"]) != 1 || m.events["mi_src"][0].EventType != mission.EventReopenRequested {
		t.Fatalf("事件留痕: %+v", m.events["mi_src"])
	}
	want := map[string]string{
		"mi_run": "paused", "mi_p": "already_paused", "mi_term": "terminal",
		"mi_none": "not_found", "mi_cz": "not_pausable", "mi_drft": "not_pausable",
		"mi_rv": "not_pausable", "mi_wait": "paused",
	}
	if len(out.Affected) != len(want) {
		t.Fatalf("逐项结果数: %+v", out.Affected)
	}
	for _, a := range out.Affected {
		if a.Result != want[a.MissionID] {
			t.Fatalf("%s = %s（want %s）", a.MissionID, a.Result, want[a.MissionID])
		}
	}
	// 暂停 reason 回链——审校 #3 后 already_paused 也过控制面（重发停止信号）
	if len(p.calls) != 3 || p.calls[0] != "mi_run|reopen:rop_t1" || p.calls[1] != "mi_p|reopen:rop_t1" || p.calls[2] != "mi_wait|reopen:rop_t1" {
		t.Fatalf("暂停调用: %+v", p.calls)
	}
	// 请求不改发起 Mission 状态
	if got := m.byID["mi_src"].Status; got != mission.StatusReady {
		t.Fatalf("发起任务状态不得变: %s", got)
	}
}

// CAS 冲突 / 404 / 形状。
func TestSubmitGuards(t *testing.T) {
	m := &fakeMissions{byID: map[string]mission.Mission{"mi_a": mkMission("mi_a", mission.StatusReady, 2)},
		events: map[string][]mission.Event{}, reopen: map[string]int{}}
	s := newTestService(m, &fakePauser{paused: map[string]bool{}})
	ctx := context.Background()

	if _, err := s.Submit(ctx, "mi_zz", SubmitRequest{ExpectedVersion: 1, DecisionRef: "d", EvidenceRefs: []string{"e"}}); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := s.Submit(ctx, "mi_a", SubmitRequest{ExpectedVersion: 1, DecisionRef: "d", EvidenceRefs: []string{"e"}}); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("CAS 冲突: %v", err)
	}
	bad := SubmitRequest{ExpectedVersion: 2, DecisionRef: " ", EvidenceRefs: nil}
	if _, err := s.Submit(ctx, "mi_a", bad); !errors.Is(err, ErrInvalidReopen) {
		t.Fatalf("形状: %v", err)
	}
	sixteen := make([]string, 17)
	for i := range sixteen {
		sixteen[i] = "mi_x"
	}
	bad2 := SubmitRequest{ExpectedVersion: 2, DecisionRef: "d", EvidenceRefs: []string{"e"}, AffectedMissionIDs: sixteen}
	if _, err := s.Submit(ctx, "mi_a", bad2); !errors.Is(err, ErrInvalidReopen) {
		t.Fatalf("受影响超限: %v", err)
	}
}

// 暂停面失败：不回滚请求事实，结果 failed 携原因；transition 且已 paused → already。
func TestSubmitPauseFailure(t *testing.T) {
	m := &fakeMissions{byID: map[string]mission.Mission{
		"mi_src": mkMission("mi_src", mission.StatusReady, 1),
		"mi_f":   mkMission("mi_f", mission.StatusRunning, 1),
	}, events: map[string][]mission.Event{}, reopen: map[string]int{}}
	p := &fakePauser{paused: map[string]bool{}, failOn: "mi_f", failErr: errPauseTransition}
	s := newTestService(m, p)
	out, err := s.Submit(context.Background(), "mi_src", SubmitRequest{
		ExpectedVersion: 1, DecisionRef: "d", EvidenceRefs: []string{"e"}, AffectedMissionIDs: []string{"mi_f"}})
	if err != nil {
		t.Fatalf("暂停失败不回滚请求: %v", err)
	}
	if len(m.events["mi_src"]) != 1 {
		t.Fatal("请求事实须已留痕")
	}
	if len(out.Affected) != 1 || out.Affected[0].Result != "failed" || out.Affected[0].Detail == "" {
		t.Fatalf("failed 结果: %+v", out.Affected)
	}
}

// 读面：事件流组装（seq 升序）。
func TestList(t *testing.T) {
	m := &fakeMissions{byID: map[string]mission.Mission{"mi_a": mkMission("mi_a", mission.StatusReady, 1)}}
	m.events = map[string][]mission.Event{}
	for i := 0; i < 2; i++ {
		m.events["mi_a"] = append(m.events["mi_a"],
			mission.Event{MissionID: "mi_a", EventType: mission.EventStatusChanged, Payload: []byte(`{}`), OccurredAt: "t"},
			mission.Event{MissionID: "mi_a", EventType: mission.EventReopenRequested,
				Payload:    []byte(fmt.Sprintf(`{"request_id":"rop_%d","decision_ref":"d%d","evidence_refs":["e"],"affected_mission_ids":[],"reason":"r","contract_version":1}`, i, i)),
				OccurredAt: fmt.Sprintf("2026-11-03T12:00:0%d.000Z", i)})
	}
	s := newTestService(m, &fakePauser{paused: map[string]bool{}})
	list, err := s.List(context.Background(), "mi_a")
	if err != nil || len(list) != 2 {
		t.Fatalf("列表: %v %+v", err, list)
	}
	if list[0].RequestID != "rop_0" || list[1].RequestID != "rop_1" || list[0].OccurredAt > list[1].OccurredAt {
		t.Fatalf("升序: %+v", list)
	}
	if list[0].Affected != nil {
		t.Fatalf("读面不回填逐项结果: %+v", list[0])
	}
	if _, err := s.List(context.Background(), "mi_zz"); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("404: %v", err)
	}
}

// blankTrim 判空与 mission/SPA 同集（White_Space ∪ BOM）。
func TestBlankTrim(t *testing.T) {
	if blankTrim(" \ufeff\u0085") != "" {
		t.Fatal("BOM/NEL 判空")
	}
	_ = utf8.RuneCountInString
	_ = strings.TrimSpace
	_ = unicode.IsSpace
}
