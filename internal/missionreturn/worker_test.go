package missionreturn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// fakeStore 依 Notice 列表模拟存储；deliverErr 按 roomID 注入核对结论。
type fakeStore struct {
	rows         []Notice
	deliverErr   map[string]error
	delivered    []string // 依投递序记录 notice_id
	envs         []EnvelopeInput
	updates      []string // "id:status:attempts:next"
	projectCalls int
	projectN     int
}

func (f *fakeStore) Project(context.Context) (int, error) {
	f.projectCalls++
	return f.projectN, nil
}

func (f *fakeStore) Due(_ context.Context, now string, _ int) ([]Notice, error) {
	nowT, _ := time.Parse(time.RFC3339, now)
	var out []Notice
	for _, r := range f.rows {
		if r.Status != StatusPending && r.Status != StatusHeld {
			continue
		}
		if r.NextAttemptAt != "" {
			t, _ := time.Parse(time.RFC3339, r.NextAttemptAt)
			if t.After(nowT) {
				continue
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RoomID != out[j].RoomID {
			return out[i].RoomID < out[j].RoomID
		}
		return out[i].SourceSeq < out[j].SourceSeq
	})
	return out, nil
}

func (f *fakeStore) set(i int, mutate func(*Notice)) { mutate(&f.rows[i]) }

func (f *fakeStore) find(id string) int {
	for i := range f.rows {
		if f.rows[i].NoticeID == id {
			return i
		}
	}
	return -1
}

func (f *fakeStore) Deliver(_ context.Context, n Notice, env EnvelopeInput) error {
	if err := f.deliverErr[n.RoomID]; err != nil {
		return err
	}
	i := f.find(n.NoticeID)
	if i < 0 || (f.rows[i].Status != StatusPending && f.rows[i].Status != StatusHeld) {
		return ErrNoticeNotPending
	}
	f.set(i, func(r *Notice) { r.Status = StatusDelivered; r.RoomEventID = env.EventID })
	f.delivered = append(f.delivered, n.NoticeID)
	f.envs = append(f.envs, env)
	return nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, id string, st Status, attempts int, next, lastErr string) error {
	i := f.find(id)
	if i < 0 {
		return fmt.Errorf("notice %s 不存在", id)
	}
	if f.rows[i].Status == StatusDelivered || f.rows[i].Status == StatusCanceled || f.rows[i].Status == StatusFailed {
		return nil // 终态不动（与 sqlite 守卫同口径）
	}
	f.set(i, func(r *Notice) {
		r.Status = st
		r.Attempts = attempts
		r.NextAttemptAt = next
		r.LastError = lastErr
		r.UpdatedAt = "u"
	})
	f.updates = append(f.updates, fmt.Sprintf("%s:%s:%d:%s", id, st, attempts, next))
	return nil
}

func notice(id, room string, seq int64, st Status) Notice {
	return Notice{
		NoticeID: id, MissionID: "mi_1", RoomID: room, TaskID: "tsk_1",
		Kind: KindReviewing, SourceSeq: seq, ContractVersion: 1,
		Status: st, Objective: "整理会议纪要",
	}
}

func newTestWorker(f *fakeStore, now *time.Time) *Worker {
	return New(f,
		WithClock(func() time.Time { return *now }),
		WithNewID(func(p string) string { return p + "_x1" }),
		WithTenant("ten_local"),
		WithMaxAttempts(3),
	)
}

const base = "2026-11-02T12:00:00Z"

func parseT(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

// 到期通知按房分组、房内 source_seq 保序投递；跨房互不阻塞。
func TestTickDeliversDueInRoomOrder(t *testing.T) {
	f := &fakeStore{rows: []Notice{
		notice("a1", "roomA", 10, StatusPending),
		notice("a2", "roomA", 20, StatusPending),
		notice("b1", "roomB", 15, StatusPending),
	}}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := []string{"a1", "a2", "b1"} // roomA < roomB；房内按 seq
	if strings.Join(f.delivered, ",") != strings.Join(want, ",") {
		t.Fatalf("投递序 = %v，want %v", f.delivered, want)
	}
	if f.projectCalls != 1 {
		t.Fatalf("Tick 应各调一次 Project，得 %d", f.projectCalls)
	}
}

// 暂停：房头 held（attempts 不增、+5s 复查），该房后继本轮不再尝试；异房照常。
func TestTickPausedHoldsAndSkipsRestOfRoom(t *testing.T) {
	f := &fakeStore{
		rows: []Notice{
			notice("a1", "roomA", 10, StatusPending),
			notice("a2", "roomA", 20, StatusPending),
			notice("b1", "roomB", 15, StatusPending),
		},
		deliverErr: map[string]error{"roomA": ErrRoomPaused},
	}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if strings.Join(f.delivered, ",") != "b1" {
		t.Fatalf("roomA 不应落卡，delivered=%v", f.delivered)
	}
	i := f.find("a1")
	if i < 0 || f.rows[i].Status != StatusHeld || f.rows[i].Attempts != 0 {
		t.Fatalf("a1 应 held 且 attempts=0，得 %+v", f.rows[i])
	}
	if got := f.rows[i].NextAttemptAt; got != base[:len(base)-1]+"5Z" && got != "2026-11-02T12:00:05Z" {
		t.Fatalf("a1 next_attempt_at = %q", got)
	}
	if j := f.find("a2"); j < 0 || f.rows[j].Status != StatusPending {
		t.Fatalf("a2 应保持 pending（房内保序，房头未投出不越序），得 %+v", f.rows[j])
	}
}

// 房间删除：逐条 canceled（含后继——同房全部失效）；不影响异房。
func TestTickDeletedCancelsRoom(t *testing.T) {
	f := &fakeStore{
		rows: []Notice{
			notice("a1", "roomA", 10, StatusPending),
			notice("a2", "roomA", 20, StatusPending),
			notice("b1", "roomB", 15, StatusPending),
		},
		deliverErr: map[string]error{"roomA": ErrRoomDeleted},
	}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if strings.Join(f.delivered, ",") != "b1" {
		t.Fatalf("delivered=%v", f.delivered)
	}
	for _, id := range []string{"a1", "a2"} {
		i := f.find(id)
		if i < 0 || f.rows[i].Status != StatusCanceled || f.rows[i].LastError != "room deleted" {
			t.Fatalf("%s 应 canceled(room deleted)，得 %+v", id, f.rows[i])
		}
	}
}

// 一般错误：指数退避（base<<attempts，封顶 60s）+ 有界——达到上限置 failed，不再投递。
func TestTickGenericErrorBackoffAndBounded(t *testing.T) {
	f := &fakeStore{
		rows:       []Notice{notice("a1", "roomA", 10, StatusPending)},
		deliverErr: map[string]error{"roomA": fmt.Errorf("boom")},
	}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	// maxAttempts=3：三轮失败 → failed。
	for k := 0; k < 3; k++ {
		if err := w.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", k, err)
		}
		now = now.Add(time.Minute)
	}
	i := f.find("a1")
	if i < 0 || f.rows[i].Status != StatusFailed || f.rows[i].Attempts != 3 {
		t.Fatalf("a1 应 failed(attempts=3)，得 %+v", f.rows[i])
	}
	if f.rows[i].LastError != "boom" {
		t.Fatalf("last_error = %q", f.rows[i].LastError)
	}
	// failed 行不再到期、不再尝试。
	before := len(f.updates)
	now = now.Add(time.Minute)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick after failed: %v", err)
	}
	if len(f.updates) != before {
		t.Fatalf("failed 后不应再有状态更新")
	}
	// 退避轨迹：+2s →（时钟已前进 1 分钟）当前 +4s（attempts=1/2，相对当前时刻）。
	if len(f.updates) < 2 {
		t.Fatalf("应有两次退避更新，得 %v", f.updates)
	}
	if !strings.Contains(f.updates[0], ":1:2026-11-02T12:00:02Z") || !strings.Contains(f.updates[1], ":2:2026-11-02T12:01:04Z") {
		t.Fatalf("退避轨迹 = %v", f.updates)
	}
}

// held 到期复查：恢复（投递成功）后落卡。
func TestTickHeldRetryAfterDelay(t *testing.T) {
	f := &fakeStore{rows: []Notice{notice("a1", "roomA", 10, StatusHeld)}}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	// 未到期（held 默认 next 为空 = 立即可试——本例直接投）。
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if strings.Join(f.delivered, ",") != "a1" {
		t.Fatalf("held 到期应投递，delivered=%v", f.delivered)
	}
}

// 信封组成：body 携 objective/轮次/契约版本；mission_ref 字段快照正确；evt_ 前缀。
func TestEnvelopeComposition(t *testing.T) {
	n := notice("a1", "roomA", 42, StatusPending)
	n.Kind = KindAccepted
	n.ReviewRound = 2
	n.PackageID = "dlv_1"
	n.Reviewer = "验收人甲"
	n.ContractVersion = 3
	f := &fakeStore{rows: []Notice{n}}
	now := parseT(t, base)
	w := newTestWorker(f, &now)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.envs) != 1 {
		t.Fatalf("envs=%d", len(f.envs))
	}
	env := f.envs[0]
	if !strings.HasPrefix(env.EventID, "evt_") {
		t.Fatalf("EventID = %q", env.EventID)
	}
	if env.OccurredAt != base {
		t.Fatalf("OccurredAt = %q", env.OccurredAt)
	}
	for _, want := range []string{"整理会议纪要", "验收通过", "第 2 轮", "契约 v3"} {
		if !strings.Contains(env.Body, want) {
			t.Fatalf("body 缺 %q：%q", want, env.Body)
		}
	}
	ref := env.Ref
	if ref.MissionID != "mi_1" || ref.Kind != "accepted" || ref.ContractVersion != 3 ||
		ref.ReviewRound != 2 || ref.PackageID != "dlv_1" || ref.TaskID != "tsk_1" || ref.SourceSeq != 42 {
		t.Fatalf("mission_ref = %+v", ref)
	}
	_ = protocol.MissionRef{} // 类型存在性
}
