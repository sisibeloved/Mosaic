package slot

// 职责准入（RFC-0018 §5.2 / ADR-0022，0.5.5）：用户授权（AgentGrant）×
// 职责范围（SlotSpec 声明的要求）→ readiness report。EvaluateReadiness 为
// 交集单源——门（prepare/retry 的 blocked 判定）、试算端点与前端报告共用；
// 0.6+ 并入 Workspace 政策 / Mission 范围 / Runtime 强制能力时只加判定行。

import (
	"context"
	"errors"
	"fmt"
)

// 领域错误。
var (
	// ErrInvalidGrant 授权档案不合法（模式/上限/枚举/重复 ref）。
	ErrInvalidGrant = errors.New("slot: 授权档案不合法")
	// ErrGrantNotFound 授权档案未设置（空授权安全缺省——非错误态，UI 可编辑
	// 空表单）。
	ErrGrantNotFound = errors.New("slot: 授权档案未设置")
)

// AgentGrantRuntime 授权侧运行能力（与 RuntimeRequirements 对偶）。
type AgentGrantRuntime struct {
	Capabilities    []string `json:"capabilities"`
	WorkspaceAccess string   `json:"workspace_access"` // none | read | read_write
}

// AgentGrant 用户对 Agent 的授权档案（一 Agent 一档，upsert 全量替换）。
// 无档案 = 空授权（安全缺省）：一切声明要求缺失 → blocked。
type AgentGrant struct {
	AgentID   string           `json:"agent_id"`
	Skills    []SkillRef       `json:"skills"`
	Tools     []string         `json:"tools"`
	Context   []string         `json:"context"`
	Runtime   AgentGrantRuntime `json:"runtime"`
	UpdatedAt string           `json:"updated_at"`
}

// ReadinessReport 准入报告（RFC §5.2"满足项、缺失项、可选降级"）。
type ReadinessReport struct {
	Ready                     bool     `json:"ready"`
	Satisfied                 []string `json:"satisfied"`
	Missing                   []string `json:"missing"`
	Degraded                  []string `json:"degraded"`
	EffectiveWorkspaceAccess  string   `json:"effective_workspace_access"`
}

// ReadinessResult 试算结果：报告 + 钉住的模板上下文。
type ReadinessResult struct {
	SlotInstanceID  string          `json:"slot_instance_id"`
	AgentID         string          `json:"agent_id"`
	SlotSpecID      string          `json:"slot_spec_id"`
	SlotSpecVersion int             `json:"slot_spec_version"`
	Report          ReadinessReport `json:"report"`
}

// workspaceLevel 权限格栅：none < read < read_write。
func workspaceLevel(s string) int {
	switch s {
	case "read":
		return 1
	case "read_write":
		return 2
	default:
		return 0
	}
}

// EvaluateReadiness 交集单源（纯函数）：模板声明的要求 × 授权档案 → 报告。
// 遍历序 = 模板声明序（清单确定性）；有效权限 = min(授权级, 模板要求)，
// 模板未声明 runtime 时不构成约束（有效 = 授权级）。
func EvaluateReadiness(c SpecContent, g AgentGrant) ReadinessReport {
	if g.Runtime.WorkspaceAccess == "" {
		g.Runtime.WorkspaceAccess = "none" // 零值授权归一（无档案 = 空授权）
	}
	rep := ReadinessReport{
		Satisfied:                []string{},
		Missing:                  []string{},
		Degraded:                 []string{},
		EffectiveWorkspaceAccess: g.Runtime.WorkspaceAccess,
	}
	hasTool := map[string]bool{}
	for _, t := range g.Tools {
		hasTool[t] = true
	}
	hasCtx := map[string]bool{}
	for _, x := range g.Context {
		hasCtx[x] = true
	}
	hasCap := map[string]bool{}
	for _, x := range g.Runtime.Capabilities {
		hasCap[x] = true
	}
	grantSkill := map[string]int{}
	for _, sk := range g.Skills {
		if sk.Version > grantSkill[sk.Ref] {
			grantSkill[sk.Ref] = sk.Version
		}
	}

	// skills：模板列出即必需；授权同 ref 且 version ≥ 引用版（向后兼容）
	for _, sk := range c.Skills {
		item := fmt.Sprintf("skill:%s@v%d", sk.Ref, sk.Version)
		if grantSkill[sk.Ref] >= sk.Version {
			rep.Satisfied = append(rep.Satisfied, item)
		} else {
			rep.Missing = append(rep.Missing, item)
		}
	}
	// tools / context：required 缺失即 blocked；optional 缺失降级（AT-07）
	if c.Tools != nil {
		for _, t := range c.Tools.Required {
			if hasTool[t] {
				rep.Satisfied = append(rep.Satisfied, "tool:"+t)
			} else {
				rep.Missing = append(rep.Missing, "tool:"+t)
			}
		}
		for _, t := range c.Tools.Optional {
			if hasTool[t] {
				rep.Satisfied = append(rep.Satisfied, "tool:"+t)
			} else {
				rep.Degraded = append(rep.Degraded, "tool:"+t+"（选配缺失，降级）")
			}
		}
	}
	if c.Context != nil {
		for _, x := range c.Context.Required {
			if hasCtx[x] {
				rep.Satisfied = append(rep.Satisfied, "context:"+x)
			} else {
				rep.Missing = append(rep.Missing, "context:"+x)
			}
		}
		for _, x := range c.Context.Optional {
			if hasCtx[x] {
				rep.Satisfied = append(rep.Satisfied, "context:"+x)
			} else {
				rep.Degraded = append(rep.Degraded, "context:"+x+"（选配缺失，降级）")
			}
		}
	}
	if c.Runtime != nil {
		for _, cap := range c.Runtime.Required {
			if hasCap[cap] {
				rep.Satisfied = append(rep.Satisfied, "runtime:"+cap)
			} else {
				rep.Missing = append(rep.Missing, "runtime:"+cap)
			}
		}
		// 权限交集：授权 ≥ 要求 → 满足且有效 = 要求（min）；不足 → 缺失
		item := "workspace_access:" + c.Runtime.WorkspaceAccess
		if workspaceLevel(g.Runtime.WorkspaceAccess) >= workspaceLevel(c.Runtime.WorkspaceAccess) {
			rep.Satisfied = append(rep.Satisfied, item)
			rep.EffectiveWorkspaceAccess = c.Runtime.WorkspaceAccess
		} else {
			rep.Missing = append(rep.Missing,
				fmt.Sprintf("%s（授权 %s）", item, g.Runtime.WorkspaceAccess))
		}
	}
	rep.Ready = len(rep.Missing) == 0
	return rep
}

// GrantReadinessChecker 准入门实装（0.5.5）：候选任职 → 实例 → 谱系 →
// 钉住版本内容 → 授权档案（无档 = 空）→ EvaluateReadiness。
type GrantReadinessChecker struct {
	store Store
}

// NewGrantReadinessChecker 构造准入门（app 装配注入，生产从此有真实 blocked）。
func NewGrantReadinessChecker(store Store) *GrantReadinessChecker {
	return &GrantReadinessChecker{store: store}
}

// Check 实现 ReadinessChecker（ADR-0022 签名：报告单源）。
func (c *GrantReadinessChecker) Check(ctx context.Context, candidate Binding) (ReadinessReport, error) {
	inst, ok, err := c.store.InstanceByID(ctx, candidate.SlotInstanceID)
	if err != nil {
		return ReadinessReport{}, err
	}
	if !ok {
		return ReadinessReport{}, ErrInstanceNotFound
	}
	spec, ok, err := c.store.SpecByVersion(ctx, inst.SlotSpecID, candidate.SlotSpecVersion)
	if err != nil {
		return ReadinessReport{}, err
	}
	if !ok {
		return ReadinessReport{}, ErrNotFound // 谱系/版本缺失（病态，无删除路径）
	}
	grant, _, err := c.store.GrantByAgent(ctx, candidate.AgentID)
	if err != nil {
		return ReadinessReport{}, err
	}
	return EvaluateReadiness(spec.SpecContent, grant), nil
}

// PutGrant 全量替换授权档案（校验单源 + agentLookup 存在性；任意生命周期
// 状态可存档——授权是数据，行为许可是 prepare/activate 的 agent_not_active）。
func (s *Service) PutGrant(ctx context.Context, agentID string, g AgentGrant) (AgentGrant, error) {
	if _, ok, err := s.agentStatusOf(ctx, agentID); err != nil {
		return AgentGrant{}, err
	} else if !ok {
		return AgentGrant{}, ErrAgentNotFound
	}
	g.AgentID = agentID
	if err := validateGrant(g); err != nil {
		return AgentGrant{}, err
	}
	g = normalizeGrant(g)
	g.UpdatedAt = s.now()
	if err := s.store.PutGrant(ctx, g); err != nil {
		return AgentGrant{}, err
	}
	return g, nil
}

// Grant 读取授权档案；未设置 ok=false（空授权安全缺省）。
func (s *Service) Grant(ctx context.Context, agentID string) (AgentGrant, bool, error) {
	return s.store.GrantByAgent(ctx, agentID)
}

// Readiness 试算（无副作用，不建任职）：版本三态与 prepare 同规则
//（缺省 0 = 谱系最新；显式须 ∈ [1, latest]）；Agent 存在但不要求 active
//（报告是能力适配，生命周期另由任职两时点校验）。
func (s *Service) Readiness(ctx context.Context, slotInstanceID, agentID string, specVersion int) (ReadinessResult, error) {
	inst, ok, err := s.store.InstanceByID(ctx, slotInstanceID)
	if err != nil {
		return ReadinessResult{}, err
	}
	if !ok {
		return ReadinessResult{}, ErrInstanceNotFound
	}
	if _, ok, err := s.agentStatusOf(ctx, agentID); err != nil {
		return ReadinessResult{}, err
	} else if !ok {
		return ReadinessResult{}, ErrAgentNotFound
	}
	latest, ok, err := s.store.Latest(ctx, inst.SlotSpecID)
	if err != nil {
		return ReadinessResult{}, err
	}
	if !ok {
		return ReadinessResult{}, ErrNotFound
	}
	version := latest.Version
	if specVersion != 0 {
		if specVersion < 1 || specVersion > latest.Version {
			return ReadinessResult{}, fmt.Errorf("%w: %d ∉ [1, %d]（%s）", ErrInvalidSpecVersion, specVersion, latest.Version, inst.SlotSpecID)
		}
		version = specVersion
	}
	spec, ok, err := s.store.SpecByVersion(ctx, inst.SlotSpecID, version)
	if err != nil {
		return ReadinessResult{}, err
	}
	if !ok {
		return ReadinessResult{}, ErrNotFound
	}
	grant, _, err := s.store.GrantByAgent(ctx, agentID)
	if err != nil {
		return ReadinessResult{}, err
	}
	return ReadinessResult{
		SlotInstanceID:  slotInstanceID,
		AgentID:         agentID,
		SlotSpecID:      inst.SlotSpecID,
		SlotSpecVersion: version,
		Report:          EvaluateReadiness(spec.SpecContent, grant),
	}, nil
}

// validateGrant 授权档案校验（与模板同口径：模式 / 上限 / 枚举 / 重复）。
func validateGrant(g AgentGrant) error {
	if len(g.Skills) > 16 {
		return fmt.Errorf("%w: skills ≤16", ErrInvalidGrant)
	}
	seen := map[string]bool{}
	for i, sk := range g.Skills {
		if !skillRefPattern.MatchString(sk.Ref) {
			return fmt.Errorf("%w: skills[%d].ref 非法（%s）", ErrInvalidGrant, i, sk.Ref)
		}
		if sk.Version < 1 {
			return fmt.Errorf("%w: skills[%d].version ≥1", ErrInvalidGrant, i)
		}
		if seen[sk.Ref] {
			return fmt.Errorf("%w: skills 重复 ref（%s）", ErrInvalidGrant, sk.Ref)
		}
		seen[sk.Ref] = true
	}
	if err := validateGrantNames("tools", g.Tools); err != nil {
		return err
	}
	if err := validateGrantNames("context", g.Context); err != nil {
		return err
	}
	if err := validateGrantNames("runtime.capabilities", g.Runtime.Capabilities); err != nil {
		return err
	}
	switch g.Runtime.WorkspaceAccess {
	case "none", "read", "read_write":
	default:
		return fmt.Errorf("%w: runtime.workspace_access 须 none|read|read_write（%q）", ErrInvalidGrant, g.Runtime.WorkspaceAccess)
	}
	return nil
}

func validateGrantNames(field string, names []string) error {
	if len(names) > 32 {
		return fmt.Errorf("%w: %s ≤32", ErrInvalidGrant, field)
	}
	seen := map[string]bool{}
	for i, n := range names {
		if !capabilityPattern.MatchString(n) {
			return fmt.Errorf("%w: %s[%d] 非法（%s）", ErrInvalidGrant, field, i, n)
		}
		if seen[n] {
			return fmt.Errorf("%w: %s 重复（%s）", ErrInvalidGrant, field, n)
		}
		seen[n] = true
	}
	return nil
}

// normalizeGrant 空切片补齐（JSON 往返稳定：null → []）。
func normalizeGrant(g AgentGrant) AgentGrant {
	if g.Skills == nil {
		g.Skills = []SkillRef{}
	}
	if g.Tools == nil {
		g.Tools = []string{}
	}
	if g.Context == nil {
		g.Context = []string{}
	}
	if g.Runtime.Capabilities == nil {
		g.Runtime.Capabilities = []string{}
	}
	return g
}
