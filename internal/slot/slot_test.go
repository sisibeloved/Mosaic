package slot

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func fixedNow() string { return "2026-10-29T12:00:00.000Z" }

func newTestService(t *testing.T) *Service {
	t.Helper()
	return New(NewMemStore(), WithNow(fixedNow))
}

func validContent() SpecContent {
	return SpecContent{
		Name:           "研究负责人",
		Responsibility: "汇总证据、组织协作，并对研究交付负责",
		Deliverables:   []string{"结论、来源与具名异议", "可验收产物清单"},
		Skills:         []SkillRef{{Ref: "workspace/skills/evidence-review", Version: 1}},
		Tools: &CapabilityList{
			Required: []string{"documents.read", "documents.write"},
			Optional: []string{"web.search"},
		},
		Context: &CapabilityList{
			Required: []string{"mission.contract", "decision.capsule"},
			Optional: []string{"room.recent"},
		},
		Delegation: &DelegationPolicy{Allowed: []string{"existing_agent", "ephemeral_worker"}, MaxDepth: 2},
		Runtime:    &RuntimeRequirements{Required: []string{"execute", "cancel"}, WorkspaceAccess: "read_write"},
		Handoff:    &HandoffRequirements{Required: []string{"checkpoint", "artifact_refs"}},
	}
}

// TestCreateFirstVersion 建谱系：首版 version=1、schema_version=1、内容往返。
func TestCreateFirstVersion(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	spec, err := svc.Create(ctx, "slot.research-lead", validContent())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if spec.SlotSpecID != "slot.research-lead" || spec.Version != 1 || spec.SchemaVersion != 1 {
		t.Fatalf("首版形状错: %+v", spec)
	}
	if spec.Name != "研究负责人" || len(spec.Deliverables) != 2 || spec.Tools == nil || len(spec.Tools.Required) != 2 {
		t.Fatalf("内容往返错: %+v", spec)
	}
	if spec.CreatedAt != fixedNow() {
		t.Fatalf("created_at = %s", spec.CreatedAt)
	}

	sums, err := svc.List(ctx)
	if err != nil || len(sums) != 1 {
		t.Fatalf("list: %v %d", err, len(sums))
	}
	s := sums[0]
	if s.SlotSpecID != "slot.research-lead" || s.LatestVersion != 1 || s.VersionCount != 1 ||
		s.Name != "研究负责人" || s.CreatedAt != fixedNow() || s.UpdatedAt != fixedNow() {
		t.Fatalf("摘要错: %+v", s)
	}
}

// TestAppendVersionIncrementsAndImmutable 追加版本递增；旧版本内容不可变。
func TestAppendVersionIncrementsAndImmutable(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	c1 := validContent()
	if _, err := svc.Create(ctx, "slot.research-lead", c1); err != nil {
		t.Fatalf("create: %v", err)
	}

	c2 := validContent()
	c2.Name = "研究负责人（修订）"
	c2.Deliverables = []string{"新增交付项"}
	v2, err := svc.AppendVersion(ctx, "slot.research-lead", c2)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if v2.Version != 2 || v2.Name != "研究负责人（修订）" {
		t.Fatalf("v2 形状错: %+v", v2)
	}

	latest, ok, err := svc.Latest(ctx, "slot.research-lead")
	if err != nil || !ok || latest.Version != 2 {
		t.Fatalf("latest: %v %v %+v", err, ok, latest)
	}
	versions, ok, err := svc.Versions(ctx, "slot.research-lead")
	if err != nil || !ok || len(versions) != 2 {
		t.Fatalf("versions: %v %v n=%d", err, ok, len(versions))
	}
	if versions[0].Version != 1 || versions[0].Name != "研究负责人" ||
		!reflect.DeepEqual(versions[0].Deliverables, c1.Deliverables) {
		t.Fatalf("v1 被改写: %+v", versions[0])
	}
	if versions[1].Version != 2 {
		t.Fatalf("升序错: %+v", versions)
	}

	sums, _ := svc.List(ctx)
	if sums[0].LatestVersion != 2 || sums[0].VersionCount != 2 || sums[0].Name != "研究负责人（修订）" {
		t.Fatalf("追加后摘要错: %+v", sums[0])
	}
}

// TestAppendSameContentNewVersion 同内容追加仍生成新版本（快照语义，
// 修订以显式版本区分，不做内容比对去重）。
func TestAppendSameContentNewVersion(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.same", validContent()); err != nil {
		t.Fatalf("create: %v", err)
	}
	v2, err := svc.AppendVersion(ctx, "slot.same", validContent())
	if err != nil {
		t.Fatalf("append same: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("version = %d, want 2", v2.Version)
	}
}

// TestCreateDuplicateLineage 谱系碰撞 → ErrSpecExists（版本追加是唯一演进路径）。
func TestCreateDuplicateLineage(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.dupe", validContent()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Create(ctx, "slot.dupe", validContent()); !errors.Is(err, ErrSpecExists) {
		t.Fatalf("err = %v, want ErrSpecExists", err)
	}
}

// TestUnknownLineage 未知谱系 → ErrNotFound。
func TestUnknownLineage(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.AppendVersion(ctx, "slot.missing", validContent()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append err = %v, want ErrNotFound", err)
	}
	if _, ok, err := svc.Latest(ctx, "slot.missing"); err != nil || ok {
		t.Fatalf("latest: %v %v", err, ok)
	}
	if _, ok, err := svc.Versions(ctx, "slot.missing"); err != nil || ok {
		t.Fatalf("versions: %v %v", err, ok)
	}
}

// TestValidationFamily 校验族：必填/上限/模式/枚举/required-optional 相交全拒。
func TestValidationFamily(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		id   string
		mut  func(*SpecContent)
	}{
		{"id 无 slot. 前缀", "research-lead", nil},
		{"id 含大写", "slot.Research", nil},
		{"id 含非法字符", "slot.research lead", nil},
		{"空名", "slot.a", func(c *SpecContent) { c.Name = "  " }},
		{"名超长", "slot.a", func(c *SpecContent) { c.Name = maxRunes(65) }},
		{"职责超长", "slot.a", func(c *SpecContent) { c.Responsibility = maxRunes(513) }},
		{"无交付项", "slot.a", func(c *SpecContent) { c.Deliverables = nil }},
		{"交付项超数", "slot.a", func(c *SpecContent) { c.Deliverables = repeat("交付", 17) }},
		{"交付项超长", "slot.a", func(c *SpecContent) { c.Deliverables = []string{maxRunes(257)} }},
		{"技能引用非法字符", "slot.a", func(c *SpecContent) { c.Skills = []SkillRef{{Ref: "bad ref!", Version: 1}} }},
		{"技能版本零", "slot.a", func(c *SpecContent) { c.Skills = []SkillRef{{Ref: "ok/ref", Version: 0}} }},
		{"技能超数", "slot.a", func(c *SpecContent) { c.Skills = repeatSkills(17) }},
		{"工具必选可选相交", "slot.a", func(c *SpecContent) {
			c.Tools = &CapabilityList{Required: []string{"documents.read"}, Optional: []string{"documents.read"}}
		}},
		{"工具名大写", "slot.a", func(c *SpecContent) {
			c.Tools = &CapabilityList{Required: []string{"Documents.Read"}}
		}},
		{"工具超数", "slot.a", func(c *SpecContent) {
			c.Tools = &CapabilityList{Required: repeat("tool", 33)}
		}},
		{"上下文必选可选相交", "slot.a", func(c *SpecContent) {
			c.Context = &CapabilityList{Required: []string{"mission.contract"}, Optional: []string{"mission.contract"}}
		}},
		{"委派非法枚举", "slot.a", func(c *SpecContent) {
			c.Delegation = &DelegationPolicy{Allowed: []string{"anyone"}, MaxDepth: 1}
		}},
		{"委派深度越界", "slot.a", func(c *SpecContent) {
			c.Delegation = &DelegationPolicy{Allowed: []string{"existing_agent"}, MaxDepth: 9}
		}},
		{"运行时工作区非法", "slot.a", func(c *SpecContent) {
			c.Runtime = &RuntimeRequirements{WorkspaceAccess: "write_all"}
		}},
		{"运行时工作区空串（审校 P2#3）", "slot.a", func(c *SpecContent) {
			c.Runtime = &RuntimeRequirements{WorkspaceAccess: ""}
		}},
		{"运行时工作区缺失零值（审校 P2#3）", "slot.a", func(c *SpecContent) {
			c.Runtime = &RuntimeRequirements{Required: []string{"execute"}}
		}},
		{"交接件非法字符", "slot.a", func(c *SpecContent) {
			c.Handoff = &HandoffRequirements{Required: []string{"bad key!"}}
		}},
	}
	for _, tc := range cases {
		c := validContent()
		if tc.mut != nil {
			tc.mut(&c)
		}
		if _, err := svc.Create(ctx, tc.id, c); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec", tc.name, err)
		}
	}
	// 全拒后库内零谱系
	sums, _ := svc.List(ctx)
	if len(sums) != 0 {
		t.Fatalf("校验失败不应落库: %d", len(sums))
	}
}

// TestMinimalContent 仅必填字段的极简模板合法（可选段缺省）。
func TestMinimalContent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	spec, err := svc.Create(ctx, "slot.min", SpecContent{
		Name: "值守", Responsibility: "例行检查并汇报", Deliverables: []string{"日报"},
	})
	if err != nil {
		t.Fatalf("minimal: %v", err)
	}
	if spec.Tools != nil || spec.Delegation != nil || spec.Skills != nil {
		t.Fatalf("可选段应缺省: %+v", spec)
	}
}

// TestConcurrentAppendVersions 并发追加 8 路：版本 2–9 连续唯一（无空洞无重复）。
func TestConcurrentAppendVersions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.race", validContent()); err != nil {
		t.Fatalf("create: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.AppendVersion(ctx, "slot.race", validContent()); err != nil {
				t.Errorf("并发追加: %v", err)
			}
		}()
	}
	wg.Wait()
	versions, ok, _ := svc.Versions(ctx, "slot.race")
	if !ok || len(versions) != 9 {
		t.Fatalf("版本数 = %d, want 9", len(versions))
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("版本 %d 处为 %d（应连续）", i, v.Version)
		}
	}
	if !reflect.DeepEqual(deliverablesOf(versions[0]), []string{"结论、来源与具名异议", "可验收产物清单"}) {
		t.Fatalf("v1 内容漂移: %+v", versions[0])
	}
}

func maxRunes(n int) string {
	out := []rune{}
	for len(out) < n {
		out = append(out, '字')
	}
	return string(out)
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s + string(rune('a'+i%26))
	}
	return out
}

func repeatSkills(n int) []SkillRef {
	out := make([]SkillRef, n)
	for i := range out {
		out[i] = SkillRef{Ref: "ok/ref", Version: 1}
	}
	return out
}

func deliverablesOf(s SlotSpec) []string { return s.Deliverables }
