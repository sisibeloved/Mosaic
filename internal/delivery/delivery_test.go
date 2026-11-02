package delivery

// 0.6.8 DeliveryPackage 成果提交（ADR-0031）：submit_result 不自动送验——
// 提交零副作用（端口无任何 Mission 变更方法——结构性保证）、可交付判定
// （completed | unknown+verified_completed）、代次/归属/契约版本守卫、形状
// 上限单源、append-only 与回执审计。测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeMissions struct{ facts map[string]MissionFacts }

func (f fakeMissions) MissionFacts(_ context.Context, id string) (MissionFacts, bool, error) {
	m, ok := f.facts[id]
	return m, ok, nil
}

type fakeRuns struct {
	facts    map[string]RunFacts
	receipts []string // runID:eventType:package_id（经存储回执汇捕获——与包同"事务"）
	sinkErr  error    // 回执汇故障注入（原子性：包不得落）
}

func (f *fakeRuns) RunFacts(_ context.Context, id string) (RunFacts, bool, error) {
	r, ok := f.facts[id]
	return r, ok, nil
}

// sink 存储回执汇（MemStore 在写包的同一临界区内调用——失败则包不落）。
func (f *fakeRuns) sink(_ context.Context, runID, eventType string, payload map[string]any) error {
	if f.sinkErr != nil {
		return f.sinkErr
	}
	pid, _ := payload["package_id"].(string)
	f.receipts = append(f.receipts, runID+":"+eventType+":"+pid)
	return nil
}

type fixture struct {
	svc      *Service
	missions fakeMissions
	runs     *fakeRuns
	store    *MemStore
	// txFacts 事务时真值（审校 #1：读-写之间可被并发修订/取消改写）；nil = 与端口读值一致
	txFacts map[string]MissionFacts
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	missions := fakeMissions{facts: map[string]MissionFacts{
		"mi_a":  {MissionID: "mi_a", ContractVersion: 2, Status: "ready"},
		"mi_c":  {MissionID: "mi_c", ContractVersion: 1, Status: "canceled"},
		"mi_rv": {MissionID: "mi_rv", ContractVersion: 1, Status: "reviewing"},
		"mi_ac": {MissionID: "mi_ac", ContractVersion: 1, Status: "accepted"},
	}}
	runs := &fakeRuns{facts: map[string]RunFacts{
		"run_ok":      {RunID: "run_ok", MissionID: "mi_a", Attempt: 1, Generation: 3, State: "completed"},
		"run_ok2":     {RunID: "run_ok2", MissionID: "mi_a", Attempt: 2, Generation: 4, State: "completed"},
		"run_unk_ok":  {RunID: "run_unk_ok", MissionID: "mi_a", Attempt: 3, Generation: 5, State: "unknown", Resolution: "verified_completed"},
		"run_unk_no":  {RunID: "run_unk_no", MissionID: "mi_a", Attempt: 4, Generation: 6, State: "unknown"},
		"run_unk_bad": {RunID: "run_unk_bad", MissionID: "mi_a", Attempt: 5, Generation: 7, State: "unknown", Resolution: "verified_failed"},
		"run_failed":  {RunID: "run_failed", MissionID: "mi_a", Attempt: 6, Generation: 8, State: "failed"},
		"run_cancel":  {RunID: "run_cancel", MissionID: "mi_a", Attempt: 7, Generation: 9, State: "canceled"},
		"run_running": {RunID: "run_running", MissionID: "mi_a", Attempt: 8, Generation: 10, State: "running"},
		"run_other":   {RunID: "run_other", MissionID: "mi_z", Attempt: 1, Generation: 1, State: "completed"},
		"run_rv":      {RunID: "run_rv", MissionID: "mi_rv", Attempt: 1, Generation: 1, State: "completed"},
		"run_ac":      {RunID: "run_ac", MissionID: "mi_ac", Attempt: 1, Generation: 1, State: "completed"},
	}}
	var n int
	tick := 0
	f := &fixture{missions: missions, runs: runs}
	store := NewMemStore().
		WithGuard(func(missionID string) (int, string, bool) {
			src := missions.facts
			if f.txFacts != nil {
				src = f.txFacts
			}
			m, ok := src[missionID]
			return m.ContractVersion, m.Status, ok
		}).
		WithReceiptSink(runs.sink)
	svc := New(store, missions, runs,
		WithNow(func() string { tick++; return "2026-11-01T12:00:0" + string(rune('0'+tick)) + ".000Z" }),
		WithNewPackageID(func() string { n++; return "dlv_t" + string(rune('0'+n)) }))
	f.svc = svc
	f.store = store
	return f
}

func goodSubmission(runID string, gen, ver int) Submission {
	return Submission{
		RunID: runID, Generation: gen, ExpectedVersion: ver,
		ArtifactRefs: []string{"workspace://out/report.md"},
		AcceptanceMapping: []MappingEntry{
			{Criterion: "七端点全绿", Evidence: "CI run 41373970521"},
			{Criterion: "CAS 冲突可见", Evidence: ""}, // 补证路径：证据可空
		},
		VerificationEvidence: "负责人本地复跑并核对产物",
		OpenItems:            []string{"Windows 路径分隔符待核"},
		Risks:                []string{"大文件截断"},
	}
}

// TestSubmitHappyPathZeroSideEffects 提交 → 包行 + 回执审计；Mission 事实端口
// 无变更方法（结构性零副作用）；列表倒序、单包可查。
func TestSubmitHappyPathZeroSideEffects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2))
	if err != nil {
		t.Fatalf("提交: %v", err)
	}
	if !strings.HasPrefix(p.PackageID, "dlv_") || p.MissionID != "mi_a" || p.RunID != "run_ok" ||
		p.Attempt != 1 || p.Generation != 3 || p.ContractVersion != 2 || p.SubmittedAt == "" {
		t.Fatalf("包溯源: %+v", p)
	}
	if len(p.AcceptanceMapping) != 2 || p.AcceptanceMapping[1].Evidence != "" {
		t.Fatalf("验收对应原样保留（证据可空——补证路径）: %+v", p.AcceptanceMapping)
	}
	// 缺省切片补齐 []（JSON 往返稳定）
	if p.ActionReceipts == nil || p.SuggestedMemory == nil {
		t.Fatalf("空切片须补齐: %+v", p)
	}
	// 回执审计落 Run 流
	if len(f.runs.receipts) != 1 || f.runs.receipts[0] != "run_ok:"+EventDeliverySubmitted+":"+p.PackageID {
		t.Fatalf("回执审计: %v", f.runs.receipts)
	}
	// 读面
	list, ok, err := f.svc.List(ctx, "mi_a")
	if err != nil || !ok || len(list) != 1 || list[0].PackageID != p.PackageID {
		t.Fatalf("列表: ok=%v %+v %v", ok, list, err)
	}
	got, ok, err := f.svc.Get(ctx, p.PackageID)
	if err != nil || !ok || got.RunID != "run_ok" {
		t.Fatalf("单包: ok=%v %+v %v", ok, got, err)
	}
	if _, ok, _ := f.svc.Get(ctx, "dlv_none"); ok {
		t.Fatal("未知包应 ok=false")
	}
	if _, ok, _ := f.svc.List(ctx, "mi_none"); ok {
		t.Fatal("未知 Mission 列表应 ok=false")
	}
}

// TestDeliverableRules 可交付判定：completed / unknown+verified_completed 可；
// failed / canceled / running / 未处置或异处置 unknown 不可。
func TestDeliverableRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, ok := range []struct {
		run string
		gen int
	}{{"run_ok", 3}, {"run_unk_ok", 5}} {
		if _, err := f.svc.Submit(ctx, "mi_a", goodSubmission(ok.run, ok.gen, 2)); err != nil {
			t.Fatalf("%s 应可交付: %v", ok.run, err)
		}
	}
	for _, bad := range []struct {
		run string
		gen int
	}{{"run_unk_no", 6}, {"run_unk_bad", 7}, {"run_failed", 8}, {"run_cancel", 9}, {"run_running", 10}} {
		if _, err := f.svc.Submit(ctx, "mi_a", goodSubmission(bad.run, bad.gen, 2)); !errors.Is(err, ErrRunNotDeliverable) {
			t.Fatalf("%s 不可交付应拒绝: %v", bad.run, err)
		}
	}
}

// TestSubmitGuards 守卫族：代次陈旧 / Run 不属 Mission / 契约版本冲突 /
// Mission 未知 / Run 未知 / Mission 已取消。守卫拒绝不落包、不记回执。
func TestSubmitGuards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := map[string]struct {
		mission string
		sub     Submission
		want    error
	}{
		"代次陈旧":       {"mi_a", goodSubmission("run_ok", 2, 2), ErrStaleGeneration},
		"Run 不属":     {"mi_a", goodSubmission("run_other", 1, 2), ErrRunMismatch},
		"版本冲突":       {"mi_a", goodSubmission("run_ok", 3, 1), ErrVersionConflict},
		"Mission 未知": {"mi_none", goodSubmission("run_ok", 3, 2), ErrMissionNotFound},
		"Run 未知":     {"mi_a", goodSubmission("run_none", 1, 2), ErrRunNotFound},
		"Mission 取消": {"mi_c", goodSubmission("run_ok", 3, 1), ErrMissionCanceled},
		// 0.6.9 / ADR-0032：验收期间受审包固定、accepted 终态——提交拒绝
		"Mission 验收中": {"mi_rv", goodSubmission("run_rv", 1, 1), ErrMissionReviewing},
		"Mission 已验收": {"mi_ac", goodSubmission("run_ac", 1, 1), ErrMissionAccepted},
	}
	for name, tc := range cases {
		if _, err := f.svc.Submit(ctx, tc.mission, tc.sub); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
	}
	if len(f.runs.receipts) != 0 {
		t.Fatalf("守卫拒绝不得记回执: %v", f.runs.receipts)
	}
	if list, _, _ := f.svc.List(ctx, "mi_a"); len(list) != 0 {
		t.Fatalf("守卫拒绝不得落包: %+v", list)
	}
}

// TestValidateLimits 形状上限单源（ADR-0031 决策 4）。
func TestValidateLimits(t *testing.T) {
	long := func(n int) string { return strings.Repeat("字", n) }
	rep := func(n int, s string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	base := goodSubmission("run_ok", 3, 2)
	for name, mutate := range map[string]func(*Submission){
		"成果引用超项":    func(s *Submission) { s.ArtifactRefs = rep(17, "r") },
		"成果引用超长":    func(s *Submission) { s.ArtifactRefs = []string{long(257)} },
		"成果引用空串":    func(s *Submission) { s.ArtifactRefs = []string{" "} },
		"验收对应超项":    func(s *Submission) { s.AcceptanceMapping = make([]MappingEntry, 17) },
		"验收条文空":     func(s *Submission) { s.AcceptanceMapping = []MappingEntry{{Criterion: "", Evidence: "x"}} },
		"验收条文超长":    func(s *Submission) { s.AcceptanceMapping = []MappingEntry{{Criterion: long(257)}} },
		"证据超长":      func(s *Submission) { s.AcceptanceMapping = []MappingEntry{{Criterion: "c", Evidence: long(1001)}} },
		"验证证据超长":    func(s *Submission) { s.VerificationEvidence = long(4001) },
		"未解决项超项":    func(s *Submission) { s.OpenItems = rep(17, "o") },
		"风险超项":      func(s *Submission) { s.Risks = rep(9, "r") },
		"动作回执超项":    func(s *Submission) { s.ActionReceipts = rep(17, "a") },
		"动作回执超长":    func(s *Submission) { s.ActionReceipts = []string{long(129)} },
		"记忆建议超项":    func(s *Submission) { s.SuggestedMemory = rep(9, "m") },
		"备注超长":      func(s *Submission) { s.Notes = long(1001) },
		"run_id 形状": func(s *Submission) { s.RunID = "bad" },
		"代次非正":      func(s *Submission) { s.Generation = 0 },
		"版本非正":      func(s *Submission) { s.ExpectedVersion = 0 },
	} {
		sub := base
		// 深拷贝切片避免交叉污染
		sub.ArtifactRefs = append([]string(nil), base.ArtifactRefs...)
		sub.AcceptanceMapping = append([]MappingEntry(nil), base.AcceptanceMapping...)
		mutate(&sub)
		if err := Validate(sub); !errors.Is(err, ErrInvalidDelivery) {
			t.Errorf("%s 应 ErrInvalidDelivery: %v", name, err)
		}
	}
	// 边界合法：恰 16 项 / 恰 256 字 / 空证据
	ok := base
	ok.ArtifactRefs = rep(16, long(256))
	ok.Risks = rep(8, "r")
	if err := Validate(ok); err != nil {
		t.Fatalf("边界合法: %v", err)
	}
}

// TestRepeatSubmitAppendOnly 同 Run 重复提交（补证）→ 两行两回执；列表倒序
// 首项为最新；不同 Run 亦可。
func TestRepeatSubmitAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p1, _ := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2))
	s2 := goodSubmission("run_ok", 3, 2)
	s2.AcceptanceMapping[1].Evidence = "补证：并发修订测试日志"
	p2, err := f.svc.Submit(ctx, "mi_a", s2)
	if err != nil || p2.PackageID == p1.PackageID {
		t.Fatalf("补证新包: %+v %v", p2, err)
	}
	p3, _ := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok2", 4, 2))
	list, _, _ := f.svc.List(ctx, "mi_a")
	if len(list) != 3 || list[0].PackageID != p3.PackageID || list[1].PackageID != p2.PackageID || list[2].PackageID != p1.PackageID {
		t.Fatalf("倒序（最新在前）: %+v", list)
	}
	if len(f.runs.receipts) != 3 {
		t.Fatalf("每次提交一条回执: %v", f.runs.receipts)
	}
	got, _, _ := f.svc.Get(ctx, p2.PackageID)
	if got.AcceptanceMapping[1].Evidence != "补证：并发修订测试日志" {
		t.Fatalf("补证内容: %+v", got)
	}
	// 历史不改写
	old, _, _ := f.svc.Get(ctx, p1.PackageID)
	if old.AcceptanceMapping[1].Evidence != "" {
		t.Fatalf("旧包不得被改写: %+v", old)
	}
}

// TestSubmitGuardInTxVersionDrift 审校 #1：端口读到 v2、写包事务时契约已修订为
// v3（并发修订）→ 必须在同一事务复核并以 version_conflict 拒绝；不落包不记回执。
func TestSubmitGuardInTxVersionDrift(t *testing.T) {
	f := newFixture(t)
	f.txFacts = map[string]MissionFacts{"mi_a": {MissionID: "mi_a", ContractVersion: 3, Status: "ready"}}
	ctx := context.Background()
	_, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2))
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("读-写间契约修订须版本冲突: %v", err)
	}
	if list, _, _ := f.svc.List(ctx, "mi_a"); len(list) != 0 {
		t.Fatalf("不得落包: %+v", list)
	}
	if len(f.runs.receipts) != 0 {
		t.Fatalf("不得记回执: %v", f.runs.receipts)
	}
}

// TestSubmitGuardInTxCancelDrift 审校 #1：端口读到 ready、写包事务时已取消 →
// mission_canceled 拒绝；不落包不记回执。
func TestSubmitGuardInTxCancelDrift(t *testing.T) {
	f := newFixture(t)
	f.txFacts = map[string]MissionFacts{"mi_a": {MissionID: "mi_a", ContractVersion: 2, Status: "canceled"}}
	ctx := context.Background()
	if _, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2)); !errors.Is(err, ErrMissionCanceled) {
		t.Fatalf("读-写间取消须拒绝: %v", err)
	}
	if list, _, _ := f.svc.List(ctx, "mi_a"); len(list) != 0 || len(f.runs.receipts) != 0 {
		t.Fatalf("不得落包/记回执: %+v %v", list, f.runs.receipts)
	}
}

// TestReceiptAtomicWithPackage 审校 #2：回执与包同事务——回执写失败则包不落，
// 调用方得到明确错误（无"部分成功"）；恢复后重试恰一包一回执。
func TestReceiptAtomicWithPackage(t *testing.T) {
	f := newFixture(t)
	f.runs.sinkErr = errors.New("run_events 写失败")
	ctx := context.Background()
	if _, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2)); err == nil {
		t.Fatal("回执失败须报错")
	}
	if list, _, _ := f.svc.List(ctx, "mi_a"); len(list) != 0 {
		t.Fatalf("回执失败包不得落（原子）: %+v", list)
	}
	f.runs.sinkErr = nil
	p, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2))
	if err != nil {
		t.Fatalf("恢复后重试: %v", err)
	}
	list, _, _ := f.svc.List(ctx, "mi_a")
	if len(list) != 1 || len(f.runs.receipts) != 1 || !strings.HasSuffix(f.runs.receipts[0], p.PackageID) {
		t.Fatalf("恰一包一回执: %+v %v", list, f.runs.receipts)
	}
}

// TestListOrderByRealTime 审校 #3：submitted_at 为 RFC3339Nano（小数位可变）——
// 须按真实时间倒序，不得按字符串（".1Z" 字符串序会排在更晚的 ".11Z" 之前）。
func TestListOrderByRealTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stamps := []string{"2026-11-01T12:00:00.1Z", "2026-11-01T12:00:00.011Z", "2026-11-01T12:00:00.11Z"}
	i := 0
	WithNow(func() string { s := stamps[i%len(stamps)]; i++; return s })(f.svc)
	for range stamps {
		if _, err := f.svc.Submit(ctx, "mi_a", goodSubmission("run_ok", 3, 2)); err != nil {
			t.Fatalf("提交: %v", err)
		}
	}
	list, _, _ := f.svc.List(ctx, "mi_a")
	got := []string{list[0].SubmittedAt, list[1].SubmittedAt, list[2].SubmittedAt}
	want := []string{"2026-11-01T12:00:00.11Z", "2026-11-01T12:00:00.1Z", "2026-11-01T12:00:00.011Z"}
	for k := range want {
		if got[k] != want[k] {
			t.Fatalf("真实时间倒序: got %v want %v", got, want)
		}
	}
}
