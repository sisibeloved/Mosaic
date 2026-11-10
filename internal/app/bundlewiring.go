// bundlewiring.go —— 0.7.5 / ADR-0042：Portable Agent Bundle 导出的端口
// 组合（bundle 不 import 各域——投影闭包在此映射，依赖方向同 execctx.Ports）。
package app

import (
	"context"
	"errors"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/checkpoint"
	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// bundlePorts 生产装配的 Ports 组合。
func bundlePorts(identitySvc *personalagent.Service, slotSvc *slot.Service,
	missionSvc *mission.Service, ckptSvc *checkpoint.Service, memSvc *memory.Service) bundle.Ports {
	return bundle.Ports{
		Agent: func(ctx context.Context, agentID string) (bundle.AgentFacts, bool, error) {
			v, ok, err := identitySvc.Get(ctx, agentID)
			if err != nil || !ok {
				return bundle.AgentFacts{}, ok, err
			}
			f := bundle.AgentFacts{
				AgentID: v.AgentID, DisplayName: v.DisplayName, Adapter: v.Adapter,
				Status: v.Status, Source: v.Source, CreatedAt: v.CreatedAt,
				// 二审 #4：别名三元组整体投影（BotID 切路径段 + 派生 ID 重算）
				//——原始路径型 BotID/嵌入用户名的派生 ID 不随包。
				Aliases: portableAliases(v.Adapter, bundle.AliasFacts{
					BotID:          v.Aliases.BotID,
					ProfileIDs:     v.Aliases.ProfileIDs,
					ParticipantIDs: v.Aliases.ParticipantIDs,
				}),
			}
			for _, e := range v.Executables {
				// 一审 #4：注册表 ExeID 含安装路径（adapter@runtime[:distro]:path
				// ——暴露本机用户名/目录结构）。投影为可移植标识 adapter@runtime[:distro]
				//（去路径段；runtime/distro/channel 已有独立字段，无信息损失）。
				f.Executables = append(f.Executables, bundle.ExecutableRef{
					ExecutableID: portableExeID(v.Adapter, e.Runtime, e.Distro), Runtime: e.Runtime,
					Distro: e.Distro, Channel: e.Channel, Enabled: e.Enabled,
				})
			}
			return f, true, nil
		},
		Slots: func(ctx context.Context, agentID string) ([]bundle.SlotFact, error) {
			// 现任职责 = active 任职；模板内容以任职钉版为准（bindingFactsAdapter
			// 同序：任职 → 实例 → 钉版本行，不随模板最新版漂移）。
			bindings, err := slotSvc.ListBindings(ctx, "")
			if err != nil {
				return nil, err
			}
			var out []bundle.SlotFact
			for _, b := range bindings {
				if b.AgentID != agentID || b.Status != "active" {
					continue
				}
				inst, ok, err := slotSvc.GetInstance(ctx, b.SlotInstanceID)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				versions, ok, err := slotSvc.Versions(ctx, inst.SlotInstance.SlotSpecID)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				for _, spec := range versions {
					if spec.Version != b.SlotSpecVersion {
						continue
					}
					out = append(out, bundle.SlotFact{
						BindingID: b.BindingID, SlotInstanceID: b.SlotInstanceID,
						SlotSpecID: inst.SlotInstance.SlotSpecID, SlotSpecVersion: b.SlotSpecVersion,
						Status: b.Status, Spec: bundleSpecOf(spec.SpecContent),
					})
					break
				}
			}
			return out, nil
		},
		Missions: func(ctx context.Context, agentID string) ([]bundle.MissionFact, error) {
			sums, err := missionSvc.ListMissions(ctx)
			if err != nil {
				return nil, err
			}
			var out []bundle.MissionFact
			for _, sm := range sums {
				if sm.OwnerAgentID != agentID {
					continue
				}
				m, ok, err := missionSvc.Get(ctx, sm.MissionID)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue // 摘要与详情竞态——保守跳过（导出是合并视图，ADR §5）
				}
				// 一审 #5：Get 后复核 owner（详情时刻事实）——摘要读取后任务可能
				// 已交接给他人；此处跳过把竞态窗口收窄到"Get 之后"（域级复核
				// 兜底——bundle.Export 对非所属任务自动跳过/显式拒绝）。
				if m.OwnerAgentID != agentID {
					continue
				}
				out = append(out, bundleMissionOf(m))
			}
			return out, nil
		},
		Checkpoint: func(ctx context.Context, missionID string) (bundle.CheckpointFact, bool, error) {
			cp, err := ckptSvc.LatestForResume(ctx, missionID)
			if err != nil {
				if errors.Is(err, checkpoint.ErrNotFound) {
					return bundle.CheckpointFact{}, false, nil
				}
				return bundle.CheckpointFact{}, false, err
			}
			return bundle.CheckpointFact{
				CheckpointID: cp.CheckpointID, RunID: cp.RunID, MissionID: cp.MissionID,
				MissionVersion: cp.MissionVersion, CheckpointVersion: cp.CheckpointVersion,
				Source:         cp.Source,
				CompletedSteps: cp.CompletedSteps, PendingSteps: cp.PendingSteps,
				ArtifactRefs: cp.ArtifactRefs, ActionReceipts: cp.ActionReceipts,
				UncertainActions: cp.UncertainActions, WaitingConditions: cp.WaitingConditions,
				SourceWatermarks: cp.SourceWatermarks, ProgressSummary: cp.ProgressSummary,
				CreatedAt: cp.CreatedAt,
			}, true, nil
		},
		Memories: func(ctx context.Context, ns, scopeID string) ([]bundle.MemoryFact, error) {
			// 全量拉取（三状态各游标走页；IncludeExpired——时效口径在 bundle
			// 域单点执行，此处给原始事实）。他域 scope 结构性不达（调用方按
			// 授权口径圈定 scope 集合后才调本闭包）。
			var out []bundle.MemoryFact
			for _, st := range []memory.Status{memory.StatusActive, memory.StatusProposed, memory.StatusSuperseded} {
				before := ""
				for {
					res, err := memSvc.List(ctx, memory.ListFilter{
						Namespace: memory.Namespace(ns), ScopeID: scopeID, Status: st,
						Limit: 100, Before: before, IncludeExpired: true,
					})
					if err != nil {
						return nil, err
					}
					for _, it := range res.Items {
						out = append(out, bundleMemoryOf(it))
					}
					if res.NextCursor == "" {
						break
					}
					before = res.NextCursor
				}
			}
			return out, nil
		},
	}
}

// portableExeID 可执行项的 Bundle 投影标识（一审 #4）：注册表 ExeID 为
// adapter@runtime[:distro]:path（含安装路径——暴露本机用户名/目录结构），
// 导出面统一去路径段，形如 codex@wsl:openEuler-24.03；runtime/distro/
// channel 已有独立字段，无信息损失。
func portableExeID(adapter, runtime, distro string) string {
	host := runtime
	if distro != "" {
		host += ":" + distro
	}
	return adapter + "@" + host
}

// portableAliases 身份别名三元组的 Bundle 投影（二审 #4）：BotIDOf 回退 =
// ExeID（含路径），且 profileKey 仅把 / 换 _——用户名仍嵌入派生 ID。投影：
// BotID 切掉 ":/" 起的路径段（无路径特征的原样透传）；ProfileID/
// ParticipantID 由脱敏后 BotID 经 personalagent 单源派生重算——包内别名
// 体系自洽（bot ↔ prof ↔ par 派生关系保持；与源实例原始派生的映射归
// 0.7.6 导入侧按 agent_id 主键处理）。
func portableAliases(adapter string, a bundle.AliasFacts) bundle.AliasFacts {
	out := bundle.AliasFacts{ProfileIDs: []string{}, ParticipantIDs: []string{}}
	if a.BotID == "" {
		return out
	}
	bot := a.BotID
	if i := strings.LastIndex(bot, ":/"); i > 0 {
		// 路径型（BotIDOf 回退 = ExeID）：切路径段，且原派生 ID 已被路径
		// 污染（profileKey 仅换 / 不去用户名）——由脱敏后 BotID 单源重算。
		bot = bot[:i]
		out.BotID = bot
		if len(a.ProfileIDs) > 0 {
			out.ProfileIDs = []string{personalagent.ProfileIDOf(adapter, bot)}
		}
		if len(a.ParticipantIDs) > 0 {
			out.ParticipantIDs = []string{personalagent.ParticipantIDOf(adapter, bot)}
		}
		return out
	}
	// 非路径型：原始事实本就可移植——原样透传（重算反而丢源实例自有派生）。
	out.BotID = a.BotID
	out.ProfileIDs = append(out.ProfileIDs, a.ProfileIDs...)
	out.ParticipantIDs = append(out.ParticipantIDs, a.ParticipantIDs...)
	return out
}

// bundleSpecOf slot.SpecContent → 中性投影。
func bundleSpecOf(c slot.SpecContent) bundle.SlotSpecFacts {
	out := bundle.SlotSpecFacts{
		Name: c.Name, Responsibility: c.Responsibility, Deliverables: c.Deliverables,
	}
	for _, s := range c.Skills {
		out.Skills = append(out.Skills, bundle.SkillRef{Ref: s.Ref, Version: s.Version})
	}
	if c.Tools != nil {
		out.Tools = &bundle.CapabilityFacts{Required: c.Tools.Required, Optional: c.Tools.Optional}
	}
	if c.Context != nil {
		out.Context = &bundle.CapabilityFacts{Required: c.Context.Required, Optional: c.Context.Optional}
	}
	if c.Delegation != nil {
		out.Delegation = &bundle.DelegationFacts{Allowed: c.Delegation.Allowed, MaxDepth: c.Delegation.MaxDepth}
	}
	if c.Runtime != nil {
		out.Runtime = &bundle.RuntimeReqFacts{Required: c.Runtime.Required, WorkspaceAccess: c.Runtime.WorkspaceAccess}
	}
	if c.Handoff != nil {
		out.Handoff = &bundle.HandoffFacts{Required: c.Handoff.Required}
	}
	return out
}

// bundleMissionOf mission.Mission → 中性投影（契约快照 + 原对象映射）。
func bundleMissionOf(m mission.Mission) bundle.MissionFact {
	out := bundle.MissionFact{
		MissionID: m.MissionID, ContractVersion: m.ContractVersion, Status: m.Status,
		OwnerAgentID: m.OwnerAgentID, Requester: m.Requester,
		Contract: bundle.ContractFacts{
			Objective: m.Content.Objective, Scope: m.Content.Scope,
			Constraints: m.Content.Constraints, AcceptanceCriteria: m.Content.AcceptanceCriteria,
			DeliverableSpecs: m.Content.DeliverableSpecs, SourceRefs: m.Content.SourceRefs,
			ReturnTargets: m.Content.ReturnTargets,
			Budget: bundle.BudgetFacts{
				Concurrency: m.Content.Budget.Concurrency, MaxRuns: m.Content.Budget.MaxRuns,
				MaxWallClockSeconds: m.Content.Budget.MaxWallClockSeconds,
				MaxRetries:          m.Content.Budget.MaxRetries,
				MaxDelegationDepth:  m.Content.Budget.MaxDelegationDepth,
			},
			Delegation: bundle.DelegationFacts{
				Allowed: m.Content.Delegation.Allowed, MaxDepth: m.Content.Delegation.MaxDepth,
			},
			Reviewer: m.Content.Reviewer,
		},
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
	if m.RoomSource != nil {
		out.RoomSource = &bundle.RoomSourceFacts{RoomID: m.RoomSource.RoomID, TaskID: m.RoomSource.TaskID}
	}
	return out
}

// bundleMemoryOf memory.Item → 中性投影（敏感性/状态/时效/血缘全字段）。
func bundleMemoryOf(it memory.Item) bundle.MemoryFact {
	out := bundle.MemoryFact{
		MemoryID: it.MemoryID, Version: it.Version, Namespace: string(it.Namespace),
		ScopeID: it.ScopeID, Kind: string(it.Kind), Title: it.Title, Content: it.Content,
		SourceRefs: it.SourceRefs, ObservedAt: it.ObservedAt,
		ValidFrom: it.ValidFrom, ValidTo: it.ValidTo, Confidence: it.Confidence,
		Status: string(it.Status), Visibility: it.Visibility, Sensitivity: string(it.Sensitivity),
		Supersedes: it.Supersedes, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
	}
	for _, e := range it.EditHistory {
		out.EditHistory = append(out.EditHistory, bundle.EditRecordFact{
			Version: e.Version, Cause: e.Cause, Note: e.Note,
			EditedBy: e.EditedBy, OccurredAt: e.OccurredAt,
		})
	}
	return out
}
