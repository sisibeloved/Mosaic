// M4-5 UT：水位三分（观察/处理/送达）、去重与重试分离、源失败不动水位、
// 待处理水位接力、指令组装与行级差异。
package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLauncher struct {
	mu     sync.Mutex
	launch []struct{ room, assignee, instr string }
	nextID int
	fail   bool
}

func (f *fakeLauncher) Launch(_ context.Context, room, assignee, instr string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errors.New("boom")
	}
	f.nextID++
	f.launch = append(f.launch, struct{ room, assignee, instr string }{room, assignee, instr})
	return "run_fake_" + strings.Repeat("x", f.nextID), nil
}

func (f *fakeLauncher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launch)
}

func newTestManager(t *testing.T, fetch Fetcher) (*Manager, *fakeLauncher, *map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor.json")
	m, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	l := &fakeLauncher{}
	statuses := map[string]string{} // run_id → status（测试注入）
	m.Launcher = l
	m.Fetch = fetch
	m.Status = func(_ context.Context, _, runID string) string { return statuses[runID] }
	m.Now = func() time.Time { return time.Now() }
	m.NewID = func(p string) string { return p + "_t" }
	return m, l, &statuses
}

func validSource(t *testing.T) Source {
	t.Helper()
	return Source{Kind: KindScript, Target: filepath.Join(t.TempDir(), "check.sh"), RoomID: "room_x",
		Assignee: "par_minimax_t", IntervalSec: 60, Enabled: true}
}

func viewOf(t *testing.T, m *Manager, id string) View {
	t.Helper()
	for _, v := range m.List() {
		if v.MonitorID == id {
			return v
		}
	}
	t.Fatalf("源不存在: %s", id)
	return View{}
}

func TestMonitorLifecycleWatermarks(t *testing.T) {
	current := "v1"
	m, l, statuses := newTestManager(t, func(context.Context, Source) (string, error) { return current, nil })
	v, err := m.Add(validSource(t))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	id := v.MonitorID

	// 首次观察 = 变化（从无到有）→ 发起 run，处理水位推进、送达未动
	check(t, m, id)
	if got := l.count(); got != 1 {
		t.Fatalf("首次观察应发起 1 次 run, got %d", got)
	}
	st := viewOf(t, m, id).State
	if st.LastObserved == "" || st.LastProcessed != st.LastObserved || st.LastDelivered != "" || st.InFlightRun == "" {
		t.Fatalf("水位不符: %+v", st)
	}

	// 无变化：跳过模型调用（去重）
	check(t, m, id)
	if got := l.count(); got != 1 {
		t.Fatalf("无变化不得再发起 run, got %d", got)
	}
	if viewOf(t, m, id).State.NoChangeCount != 1 {
		t.Fatalf("no-change 计数应递增: %+v", viewOf(t, m, id).State)
	}

	// run 完成 → 送达水位对齐
	(*statuses)[st.InFlightRun] = "completed"
	m.Tick(context.Background())
	st = viewOf(t, m, id).State
	if st.LastDelivered != st.LastObserved || st.InFlightRun != "" {
		t.Fatalf("完成后送达水位应推进: %+v", st)
	}

	// 新变化 → 新 run；在途期间再变化 → 待处理水位（不丢/不重复启动）
	current = "v2"
	check(t, m, id)
	if got := l.count(); got != 2 {
		t.Fatalf("变化应发起 run, got %d", got)
	}
	st = viewOf(t, m, id).State
	inflight2 := st.InFlightRun
	current = "v3"
	check(t, m, id)
	if got := l.count(); got != 2 {
		t.Fatalf("在途期到达的新变化应先挂待处理，不再启动: got %d", got)
	}
	st = viewOf(t, m, id).State
	if st.PendingHash == "" || st.InFlightRun != inflight2 {
		t.Fatalf("待处理水位不符: %+v", st)
	}
	// 在途完成 → 待处理接力（新 run 处理 v3）
	(*statuses)[inflight2] = "completed"
	m.Tick(context.Background())
	st = viewOf(t, m, id).State
	if st.InFlightRun == "" || st.InFlightRun == inflight2 {
		t.Fatalf("待处理变化应接力新 run: %+v", st)
	}
	if st.LastProcessed != st.LastObserved {
		t.Fatalf("接力后处理水位应推进到最新观察: %+v", st)
	}
}

func TestMonitorSourceFailureIsolatedAndRetries(t *testing.T) {
	current := "v1"
	fail := false
	m, l, statuses := newTestManager(t, func(context.Context, Source) (string, error) {
		if fail {
			return "", errors.New("connection refused")
		}
		return current, nil
	})
	v, _ := m.Add(validSource(t))
	id := v.MonitorID

	check(t, m, id)
	st := viewOf(t, m, id).State
	firstRun := st.InFlightRun
	(*statuses)[firstRun] = "completed"
	m.Tick(context.Background()) // 终态核对 → 送达水位对齐

	// 源失败：独立记录，不动水位，不调模型
	fail = true
	check(t, m, id)
	st = viewOf(t, m, id).State
	if st.LastError == "" || l.count() != 1 {
		t.Fatalf("源失败应只记录不启动: %+v launches=%d", st, l.count())
	}

	// 恢复 + 无变化：no-change（失败期间没漏任何变化）
	fail = false
	check(t, m, id)
	if l.count() != 1 || viewOf(t, m, id).State.LastError != "" {
		t.Fatalf("恢复后无变化不应启动: launches=%d", l.count())
	}

	// 处理失败（run failed）→ 下个检查刻重试（去重与重试分离）
	current = "v2"
	check(t, m, id)
	st = viewOf(t, m, id).State
	failRun := st.InFlightRun
	(*statuses)[failRun] = "failed"
	m.Tick(context.Background()) // 终态核对 → ProcessError 记录
	check(t, m, id)              // 重试：观察哈希领先送达哈希 → 重发
	st = viewOf(t, m, id).State
	if l.count() != 3 { // v1 首发 + v2 首发 + v2 重试
		t.Fatalf("处理失败应重试: launches=%d state=%+v", l.count(), st)
	}
	if st.ProcessError != "" && st.InFlightRun == "" {
		t.Fatalf("重试发起后处理错误应清空: %+v", st)
	}
}

func TestMonitorDisabledSkipsAndInstruction(t *testing.T) {
	m, l, _ := newTestManager(t, func(context.Context, Source) (string, error) { return "s", nil })
	v, _ := m.Add(Source{Kind: KindURL, Target: "https://example.com/feed", RoomID: "r", Assignee: "par_a", IntervalSec: 60, Enabled: false})
	// 停用源：手动触发也不得调度（Enabled 门一致生效）
	m.Tick(context.Background())
	if err := m.CheckNow(context.Background(), v.MonitorID); err != nil {
		t.Fatalf("checknow: %v", err)
	}
	if l.count() != 0 {
		t.Fatal("停用源不得调度")
	}
	_ = m.SetEnabled(v.MonitorID, true)
	check(t, m, v.MonitorID)
	if l.count() != 1 {
		t.Fatal("启用后应调度")
	}
	instr := lastInstruction(l)
	if !strings.Contains(instr, "首次观察") || !strings.Contains(instr, v.MonitorID) {
		t.Fatalf("指令应含差异与源标识: %q", instr)
	}
}

func lastInstruction(l *fakeLauncher) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.launch) == 0 {
		return ""
	}
	return l.launch[len(l.launch)-1].instr
}

func TestMonitorValidationAndDiff(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	bad := []Source{
		{Kind: "magic", Target: "/x", RoomID: "r", Assignee: "p", IntervalSec: 60},
		{Kind: KindScript, Target: "rel/path", RoomID: "r", Assignee: "p", IntervalSec: 60},
		{Kind: KindURL, Target: "ftp://x", RoomID: "r", Assignee: "p", IntervalSec: 60},
		{Kind: KindURL, Target: "https://x", IntervalSec: 60},                            // 缺 room/assignee
		{Kind: KindURL, Target: "https://x", RoomID: "r", Assignee: "p", IntervalSec: 5}, // 间隔过短
	}
	for i, s := range bad {
		if _, err := m.Add(s); err == nil {
			t.Errorf("case %d 应拒绝: %+v", i, s)
		}
	}

	d := DiffLines("a\nb\nc", "a\nX\nc\n")
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+X") || strings.Contains(d, "-a") {
		t.Fatalf("行级差异不符: %q", d)
	}
	if DiffLines("", "hello") == "" || !strings.Contains(DiffLines("", "hello"), "首次观察") {
		t.Fatal("首观差异应标注")
	}
}

// check 手动触发一次检查（忽略间隔——测试驱动确定性）。
func check(t *testing.T, m *Manager, monitorID string) {
	t.Helper()
	if err := m.CheckNow(context.Background(), monitorID); err != nil {
		t.Fatalf("check %s: %v", monitorID, err)
	}
}
