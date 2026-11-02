package httpapi

// 0.6.10 回房通知端点（ADR-0033）：台账列表（source_seq 升序）、404
// mission_not_found、503 return_notices_unavailable、MissionView
// room_source.deleted_at（来源墓碑）回显。真 mission 域 + 台账端口 fake。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/missionreturn"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

func missionContentForTest() mission.Content {
	return mission.Content{
		Objective:          "目标",
		Scope:              "范围",
		AcceptanceCriteria: []string{"验收"},
		DeliverableSpecs:   []string{"交付"},
		SourceRefs:         []string{"room:abc:seq1"},
	}
}

type epReturnNotices struct{ notices []missionreturn.Notice }

func (p epReturnNotices) ListReturnNotices(_ context.Context, missionID string) ([]missionreturn.Notice, error) {
	out := []missionreturn.Notice{}
	for _, n := range p.notices {
		if n.MissionID == missionID {
			out = append(out, n)
		}
	}
	return out, nil
}

func returnNoticeTestServer(t *testing.T, rn epReturnNotices, withMissions bool) (*httptest.Server, *mission.Service) {
	t.Helper()
	var seq int64
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-11-02T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { seq++; return "mi_rn" }), // 固定单任务即可
	)
	deps := Deps{ReturnNotices: rn}
	if withMissions {
		deps.Missions = mSvc
	}
	ts := httptest.NewServer(New(deps))
	t.Cleanup(ts.Close)
	return ts, mSvc
}

func TestListMissionReturnNotices(t *testing.T) {
	ts, mSvc := returnNoticeTestServer(t, epReturnNotices{notices: []missionreturn.Notice{{
		NoticeID: "rn_2", MissionID: "mi_rn", RoomID: "room_1", TaskID: "tsk_1",
		Kind: missionreturn.KindAccepted, SourceSeq: 2, ContractVersion: 1, ReviewRound: 2,
		PackageID: "dlv_1", Reviewer: "rv", Objective: "目标",
		Status: missionreturn.StatusDelivered, RoomEventID: "evt_card_2",
		CreatedAt: "2026-11-02T12:00:01.000Z", UpdatedAt: "2026-11-02T12:00:02.000Z",
	}, {
		NoticeID: "rn_1", MissionID: "mi_rn", RoomID: "room_1", TaskID: "tsk_1",
		Kind: missionreturn.KindReviewing, SourceSeq: 1, ContractVersion: 1, ReviewRound: 1,
		Objective: "目标", Status: missionreturn.StatusHeld, Attempts: 0,
		NextAttemptAt: "2026-11-02T12:00:05.000Z", LastError: "room paused",
		CreatedAt: "2026-11-02T12:00:00.000Z", UpdatedAt: "2026-11-02T12:00:03.000Z",
	}}}, true)
	m, err := mSvc.Create(context.Background(), "", missionContentForTest(), nil)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}

	resp, err := http.Get(ts.URL + "/v1/missions/" + m.MissionID + "/return-notices")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Notices []apigen.ReturnNoticeView `json:"notices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Notices) != 2 {
		t.Fatalf("notices = %d", len(out.Notices))
	}
	// fake 依入参序返回（已按 seq 降序排入）——端点映射不得改序；字段核对以首行（delivered）为准。
	first := out.Notices[0]
	if first.NoticeId != "rn_2" || first.Kind != apigen.ReturnNoticeViewKindAccepted ||
		first.Status != apigen.ReturnNoticeViewStatusDelivered || *first.RoomEventId != "evt_card_2" ||
		first.ReviewRound != 2 || *first.PackageId != "dlv_1" || first.RoomId != "room_1" ||
		first.TaskId != "tsk_1" || first.SourceSeq != 2 {
		t.Fatalf("delivered 视图 = %+v", first)
	}
	second := out.Notices[1]
	if second.Status != apigen.ReturnNoticeViewStatusHeld || *second.LastError != "room paused" ||
		second.NextAttemptAt == nil || second.RoomEventId != nil || second.PackageId != nil {
		t.Fatalf("held 视图 = %+v", second)
	}
}

func TestListMissionReturnNoticesNotFound(t *testing.T) {
	ts, _ := returnNoticeTestServer(t, epReturnNotices{}, true)
	resp, err := http.Get(ts.URL + "/v1/missions/mi_none/return-notices")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var e apigen.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "mission_not_found" {
		t.Fatalf("code = %s", e.Error.Code)
	}
}

func TestListMissionReturnNoticesUnavailable(t *testing.T) {
	ts, _ := returnNoticeTestServer(t, epReturnNotices{}, true)
	// 重新起一个未装台账端口的服务面。
	ts2 := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts2.Close)
	_ = ts
	resp, err := http.Get(ts2.URL + "/v1/missions/mi_any/return-notices")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var e apigen.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "return_notices_unavailable" {
		t.Fatalf("code = %s", e.Error.Code)
	}
}

// TestMissionViewRoomSourceTombstone 来源墓碑回显（ADR-0033）：room_source.deleted_at。
func TestMissionViewRoomSourceTombstone(t *testing.T) {
	m := mission.Mission{MissionID: "mi_t", RoomSource: &mission.RoomSource{
		RoomID: "room_t", TaskID: "tsk_t", DeletedAt: "2026-11-02T12:00:00Z",
	}}
	v := missionView(m)
	if v.RoomSource == nil || v.RoomSource.DeletedAt == nil ||
		v.RoomSource.DeletedAt.Format("2006-01-02T15:04:05Z") != "2026-11-02T12:00:00Z" {
		t.Fatalf("deleted_at 未回显: %+v", v.RoomSource)
	}
	// 房间仍在：null（非空串）。
	m2 := mission.Mission{MissionID: "mi_t2", RoomSource: &mission.RoomSource{RoomID: "r", TaskID: "t"}}
	if v2 := missionView(m2); v2.RoomSource == nil || v2.RoomSource.DeletedAt != nil {
		t.Fatalf("未删房应 null: %+v", v2.RoomSource)
	}
}
