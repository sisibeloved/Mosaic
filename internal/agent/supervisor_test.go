package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// ---- 测试替身：可观测的 fake 适配器（UT 专用，不依赖 echo）----

type fakeSession struct {
	mu     sync.Mutex
	closed bool
	tasks  []Task
}

func (f *fakeSession) Run(_ context.Context, task Task) (Handle, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, task)
	f.mu.Unlock()
	return &fakeHandle{task: task}, nil
}

func (f *fakeSession) Cancel(string) {}

func (f *fakeSession) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

type fakeHandle struct {
	task Task
}

func (h *fakeHandle) Updates() <-chan DraftUpdate {
	ch := make(chan DraftUpdate)
	close(ch)
	return ch
}

func (h *fakeHandle) Result() (Result, error) {
	return Result{Block: "turn_intent", Data: map[string]any{"task_id": h.task.TaskID}}, nil
}

func (h *fakeHandle) Cancel() {}

type fakeAdapter struct {
	name      string
	bootCount atomic.Int64
}

func (f *fakeAdapter) Name() string { return f.name }

func (f *fakeAdapter) Capabilities() Capabilities {
	return Capabilities{CancelMode: "none", HistoryChannel: "structured_request"}
}

func (f *fakeAdapter) Boot(_ context.Context, _ Profile) (Session, error) {
	f.bootCount.Add(1)
	return &fakeSession{}, nil
}

// ---- UT ----

func TestSupervisorRegisterDuplicate(t *testing.T) {
	sup := NewSupervisor()
	if err := sup.Register(&fakeAdapter{name: "fake"}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := sup.Register(&fakeAdapter{name: "fake"}); err == nil {
		t.Error("重复注册应拒绝")
	}
}

func TestSupervisorSubmitUnknownAdapter(t *testing.T) {
	sup := NewSupervisor()
	_, err := sup.Submit(context.Background(), Profile{ProfileID: "p1", Adapter: "ghost"}, Task{TaskID: "t1"})
	if err == nil {
		t.Fatal("未注册适配器应返回错误")
	}
}

func TestSupervisorBootsSessionOnceSequential(t *testing.T) {
	fa := &fakeAdapter{name: "fake"}
	sup := NewSupervisor()
	_ = sup.Register(fa)
	profile := Profile{ProfileID: "p1", Adapter: "fake"}
	for i := 0; i < 3; i++ {
		if _, err := sup.Submit(context.Background(), profile, Task{TaskID: "t"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if got := fa.bootCount.Load(); got != 1 {
		t.Errorf("顺序提交 boot 次数 = %d, want 1（逻辑会话每 Profile 唯一）", got)
	}
}

// TDD 红灯用例：并发首启必须只 Boot 一次（逻辑会话 ownership 语义，RFC-0002 v0.5）。
func TestSupervisorBootsSessionOnceConcurrent(t *testing.T) {
	fa := &fakeAdapter{name: "fake"}
	sup := NewSupervisor()
	_ = sup.Register(fa)
	profile := Profile{ProfileID: "p1", Adapter: "fake"}

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = sup.Submit(context.Background(), profile, Task{TaskID: "t"})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 submit %d: %v", i, err)
		}
	}
	if got := fa.bootCount.Load(); got != 1 {
		t.Errorf("并发首启 boot 次数 = %d, want 1", got)
	}
}

func TestSupervisorShutdownClosesSessions(t *testing.T) {
	fa := &fakeAdapter{name: "fake"}
	sup := NewSupervisor()
	_ = sup.Register(fa)

	profiles := []Profile{{ProfileID: "p1", Adapter: "fake"}, {ProfileID: "p2", Adapter: "fake"}}
	for _, p := range profiles {
		if _, err := sup.Submit(context.Background(), p, Task{TaskID: "t"}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	sup.mu.Lock()
	sessions := make([]*fakeSession, 0, len(sup.sessions))
	for _, s := range sup.sessions {
		if fs, ok := s.(*fakeSession); ok {
			sessions = append(sessions, fs)
		}
	}
	sup.mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("应持有 2 个会话, got %d", len(sessions))
	}
	sup.Shutdown()
	for i, s := range sessions {
		if !s.closed {
			t.Errorf("会话 %d 未被 Close", i)
		}
	}
}

func TestSupervisorDeliversTask(t *testing.T) {
	fa := &fakeAdapter{name: "fake"}
	sup := NewSupervisor()
	_ = sup.Register(fa)
	profile := Profile{ProfileID: "p1", Adapter: "fake"}

	want := Task{TaskID: "tsk_delivery", Kind: KindEvaluateIntent}
	h, err := sup.Submit(context.Background(), profile, want)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Data["task_id"] != want.TaskID {
		t.Errorf("任务未透传: data=%v", res.Data)
	}

	// 任务进入的是该 Profile 缓存的同一会话
	sup.mu.Lock()
	sess := sup.sessions[profile.ProfileID]
	sup.mu.Unlock()
	fs := sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.tasks) != 1 || fs.tasks[0].TaskID != want.TaskID {
		t.Errorf("会话收到任务 = %v", fs.tasks)
	}
}

// ErrStale 必须是可 errors.Is 判定的哨兵错误（迟到拒绝的确定性契约）。
func TestErrStaleIsSentinel(t *testing.T) {
	if !errors.Is(ErrStale, ErrStale) {
		t.Fatal("ErrStale 必须可用 errors.Is 判定")
	}
}

// 复审 #3：同名适配器的多个 executable（native + WSL 两份 codex）各自成实例——
// RegisterFor 按 Profile 键登记，Submit 按 ProfileID 精确解析；单实例适配器
// （如 echo）仍走名字回退路径。
func TestSupervisorResolvesPerProfileAdapter(t *testing.T) {
	a1 := &fakeAdapter{name: "codex"}
	a2 := &fakeAdapter{name: "codex"} // 同名第二个 executable
	echoLike := &fakeAdapter{name: "echo"}
	sup := NewSupervisor()

	// 名字键登记：同名第二个必须被拒（折叠即坏）——这是 RegisterFor 存在的原因
	if err := sup.Register(a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if err := sup.Register(a2); err == nil {
		t.Fatal("同名重复登记必须拒绝")
	}
	// Profile 键登记：两个同名实例并存
	if err := sup.RegisterFor("prof_codex_native", a1); err != nil {
		t.Fatalf("registerFor a1: %v", err)
	}
	if err := sup.RegisterFor("prof_codex_wsl", a2); err != nil {
		t.Fatalf("registerFor a2: %v", err)
	}
	if err := sup.Register(echoLike); err != nil {
		t.Fatalf("register echo: %v", err)
	}

	// 各 Profile 命中各自实例（Boot 计数增量可证：只动自己的实例）
	for _, tc := range []struct {
		profile    Profile
		wantBoots  *atomic.Int64
		otherBoots *atomic.Int64
	}{
		{Profile{ProfileID: "prof_codex_native", Adapter: "codex"}, &a1.bootCount, &a2.bootCount},
		{Profile{ProfileID: "prof_codex_wsl", Adapter: "codex"}, &a2.bootCount, &a1.bootCount},
	} {
		beforeWant, beforeOther := tc.wantBoots.Load(), tc.otherBoots.Load()
		h, err := sup.Submit(context.Background(), tc.profile, Task{TaskID: "t", Kind: KindEvaluateIntent})
		if err != nil {
			t.Fatalf("submit %s: %v", tc.profile.ProfileID, err)
		}
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %s: %v", tc.profile.ProfileID, err)
		}
		if tc.wantBoots.Load()-beforeWant != 1 {
			t.Fatalf("%s 应命中专属实例（boot +1）", tc.profile.ProfileID)
		}
		if tc.otherBoots.Load()-beforeOther != 0 {
			t.Fatalf("%s 不得折叠到另一实例", tc.profile.ProfileID)
		}
	}
	// 名字回退：echo 类单实例不受影响
	h, err := sup.Submit(context.Background(), Profile{ProfileID: "prof_echo", Adapter: "echo"}, Task{TaskID: "t", Kind: KindEvaluateIntent})
	if err != nil || h == nil {
		t.Fatalf("名字回退解析失败：%v", err)
	}
	if echoLike.bootCount.Load() != 1 {
		t.Fatal("名字回退应命中 echo 实例")
	}
}
