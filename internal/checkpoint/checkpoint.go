// Package checkpoint Run 检查点（RFC-0018 §7.3 / ADR-0040，0.7.3）：可恢复
// 业务状态——独立于 ContextReceipt（那是"交付了什么"，这是"工作到哪了"）。
// 双锚（Run 在册 + mission 归属一致）；checkpoint_version 存储原子分配；
// completed/pending 交集为空（"文字摘要不能将 pending 偷换成 completed"的
// 机器可判子集）。0.7.3 为显式写入面——执行面自动挂点随 0.7.4 续办接线。
package checkpoint

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 错误族（httpapi 映射：Invalid→400、RunNotFound→404、ScopeMismatch→404/409、
// NotFound→404）。
var (
	ErrInvalid       = errors.New("checkpoint: invalid")
	ErrRunNotFound   = errors.New("checkpoint: run not found")
	ErrScopeMismatch = errors.New("checkpoint: mission mismatch with run")
	ErrNotFound      = errors.New("checkpoint: not found")
)

// Checkpoint 可恢复业务状态（§7.3 字段全量）。
type Checkpoint struct {
	CheckpointID      string
	RunID             string
	MissionID         string
	MissionVersion    int
	CheckpointVersion int
	WorkItemID        string
	CompletedSteps    []string
	PendingSteps      []string
	ArtifactRefs      []string
	ActionReceipts    []string
	UncertainActions  []string
	WaitingConditions []string
	SourceWatermarks  map[string]string
	ProgressSummary   string
	// Source 落盘来源（一审 #1）："" = 显式业务快照（执行面/人工）；
	// "system" = Run 终态自动摘要——不参与 LatestOfMission 选取（不遮蔽
	// 业务快照；历史仍可查）。
	Source    string
	CreatedAt string
}

// CreateCmd 落盘命令（显式写入面）。
type CreateCmd struct {
	RunID             string // 路径锚
	MissionID         string
	MissionVersion    int
	WorkItemID        string
	CompletedSteps    []string
	PendingSteps      []string
	ArtifactRefs      []string
	ActionReceipts    []string
	UncertainActions  []string
	WaitingConditions []string
	SourceWatermarks  map[string]string
	ProgressSummary   string
	Source            string
}

// Anchors 双锚闭包（app 装配组合——checkpoint 不 import run 域）。
// RunMission 返回 (missionID, exists, error)——存在性与归属一次取回。
type Anchors struct {
	RunMission func(ctx context.Context, runID string) (string, bool, error)
}

// Store 存储端口（checkpoint_version 事务内原子分配；列表降序）。
type Store interface {
	CreateCheckpoint(ctx context.Context, cp Checkpoint) (Checkpoint, error)
	CheckpointsOfRun(ctx context.Context, runID string) ([]Checkpoint, error) // 降序（最新在前）
	// LatestOfMission 按任务取最新**业务快照**检查点（跨 Run；一审 #1：
	// 排除 system 终态摘要——不遮蔽业务字段；ok=false = 无业务快照）。
	LatestOfMission(ctx context.Context, missionID string) (Checkpoint, bool, error)
	// LatestSystemOfMission 最新系统终态摘要（二审 #1 回退面）。
	LatestSystemOfMission(ctx context.Context, missionID string) (Checkpoint, bool, error)
	// CheckpointsOfMission 任务级全量（任意 source 含 system，降序）——
	// 0.7.7 复审 #4：导入幂等查重按水位比对须覆盖业务与系统两类。
	CheckpointsOfMission(ctx context.Context, missionID string) ([]Checkpoint, error)
}

const (
	maxStepRunes    = 300
	maxListItems    = 64
	maxSummaryRunes = 500
	maxWorkItemLen  = 128
)

func invalidErr(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

func validateList(name string, items []string) error {
	if len(items) > maxListItems {
		return invalidErr(name + " 至多 64 条")
	}
	for _, v := range items {
		v = strings.TrimSpace(v)
		if v == "" || utf8.RuneCountInString(v) > maxStepRunes {
			return invalidErr(name + " 每条须为 1–300 字符")
		}
	}
	return nil
}

// validate 域校验单源：交集空 + 各列表长度 + 摘要/工作项长度 + 水位键值。
func (c CreateCmd) validate() error {
	if strings.TrimSpace(c.MissionID) == "" || len(c.MissionID) > 128 {
		return invalidErr("mission_id 须为 1–128 字符")
	}
	if c.MissionVersion < 1 {
		return invalidErr("mission_version 须 ≥1")
	}
	if utf8.RuneCountInString(c.WorkItemID) > maxWorkItemLen {
		return invalidErr("work_item_id 至多 128 字符")
	}
	for name, list := range map[string][]string{
		"completed_steps": c.CompletedSteps, "pending_steps": c.PendingSteps,
		"artifact_refs": c.ArtifactRefs, "action_receipts": c.ActionReceipts,
		"uncertain_actions": c.UncertainActions, "waiting_conditions": c.WaitingConditions,
	} {
		if err := validateList(name, list); err != nil {
			return err
		}
	}
	// "pending 不得偷换 completed"的机器可判子集：同一步骤不得同时出现在两列。
	done := map[string]bool{}
	for _, s := range c.CompletedSteps {
		done[strings.TrimSpace(s)] = true
	}
	for _, s := range c.PendingSteps {
		if done[strings.TrimSpace(s)] {
			return invalidErr("步骤同时在 completed 与 pending：" + s)
		}
	}
	if utf8.RuneCountInString(c.ProgressSummary) > maxSummaryRunes {
		return invalidErr("progress_summary 至多 500 字")
	}
	// 一审 #1：source 枚举（system 仅供运行域终态摘要适配器使用）。
	switch c.Source {
	case "", "system":
	default:
		return invalidErr("source 只接受空（业务快照）或 system（终态摘要）")
	}
	for k, v := range c.SourceWatermarks {
		if k == "" || utf8.RuneCountInString(k) > 64 || utf8.RuneCountInString(v) > 128 {
			return invalidErr("source_watermarks 键值须为 1–64/1–128 字符")
		}
	}
	return nil
}

// Service 检查点服务（锚门 + 校验 + 版本单源生成）。
type Service struct {
	store   Store
	anchors Anchors
	now     func() string
}

func NewService(store Store, anchors Anchors, now func() string) *Service {
	if now == nil {
		now = func() string { return time.Now().UTC().Format(time.RFC3339) }
	}
	return &Service{store: store, anchors: anchors, now: now}
}

// Create 落盘（双锚门 → 校验 → 存储；checkpoint_version 由存储原子分配）。
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Checkpoint, error) {
	if err := cmd.validate(); err != nil {
		return Checkpoint{}, err
	}
	if s.anchors.RunMission != nil {
		missionID, ok, err := s.anchors.RunMission(ctx, cmd.RunID)
		if err != nil {
			return Checkpoint{}, err
		}
		if !ok {
			return Checkpoint{}, ErrRunNotFound
		}
		if missionID != strings.TrimSpace(cmd.MissionID) {
			return Checkpoint{}, ErrScopeMismatch
		}
	}
	cp := Checkpoint{
		CheckpointID:      "ckp_" + newToken(),
		RunID:             cmd.RunID,
		MissionID:         strings.TrimSpace(cmd.MissionID),
		MissionVersion:    cmd.MissionVersion,
		WorkItemID:        strings.TrimSpace(cmd.WorkItemID),
		CompletedSteps:    append([]string{}, cTrim(cmd.CompletedSteps)...),
		PendingSteps:      append([]string{}, cTrim(cmd.PendingSteps)...),
		ArtifactRefs:      append([]string{}, cTrim(cmd.ArtifactRefs)...),
		ActionReceipts:    append([]string{}, cTrim(cmd.ActionReceipts)...),
		UncertainActions:  append([]string{}, cTrim(cmd.UncertainActions)...),
		WaitingConditions: append([]string{}, cTrim(cmd.WaitingConditions)...),
		SourceWatermarks:  copyWatermarks(cmd.SourceWatermarks),
		ProgressSummary:   cmd.ProgressSummary,
		Source:            cmd.Source,
		CreatedAt:         s.now(),
	}
	return s.store.CreateCheckpoint(ctx, cp)
}

// CreateImported Bundle 激活的检查点写入面（0.7.7 / ADR-0044；一审 #4/#5/#6
// 重写）：目标侧**全新 checkpoint_id 与 run_id**（域生成——同源任务的两次
// 导出可携同一源 checkpoint_id，沿用主键会在第二包撞 UNIQUE；源标识经
// SourceWatermarks 由调用方注入 bundle.origin_* 供溯源与幂等）；**Source
// 保留包内源语义**（""=业务快照 / "system"=终态摘要——0.7.4
// LatestForResume"业务优先、无业务回退最新 system"口径不因导入改变，
// 导入的旧 system 摘要不遮蔽目标新摘要）；内容约束与普通创建同源校验
// （步骤交集空/列表长度/摘要/水位键值——免的只是源 run 存在性锚）；
// checkpoint_version 由存储按新 run 原子分配。幂等由调用方按
// bundle.origin_checkpoint 水位先查后写。
func (s *Service) CreateImported(ctx context.Context, cmd CreateCmd) (Checkpoint, error) {
	if err := cmd.validate(); err != nil {
		return Checkpoint{}, err
	}
	cp := Checkpoint{
		CheckpointID:      "ckp_" + newToken(),
		RunID:             "run_" + newToken(), // 目标占位 run（源 run 进 watermarks 溯源）
		MissionID:         strings.TrimSpace(cmd.MissionID),
		MissionVersion:    cmd.MissionVersion,
		WorkItemID:        strings.TrimSpace(cmd.WorkItemID),
		CompletedSteps:    append([]string{}, cTrim(cmd.CompletedSteps)...),
		PendingSteps:      append([]string{}, cTrim(cmd.PendingSteps)...),
		ArtifactRefs:      append([]string{}, cTrim(cmd.ArtifactRefs)...),
		ActionReceipts:    append([]string{}, cTrim(cmd.ActionReceipts)...),
		UncertainActions:  append([]string{}, cTrim(cmd.UncertainActions)...),
		WaitingConditions: append([]string{}, cTrim(cmd.WaitingConditions)...),
		SourceWatermarks:  copyWatermarks(cmd.SourceWatermarks),
		ProgressSummary:   cmd.ProgressSummary,
		Source:            cmd.Source, // 保留源语义（""/system）
		CreatedAt:         s.now(),
	}
	return s.store.CreateCheckpoint(ctx, cp)
}

// ListOfRun 列表（降序——最新在前；run 未知空列表由锚语义决定——本面先查锚）。
func (s *Service) ListOfRun(ctx context.Context, runID string) ([]Checkpoint, error) {
	if s.anchors.RunMission != nil {
		if _, ok, err := s.anchors.RunMission(ctx, runID); err != nil {
			return nil, err
		} else if !ok {
			return nil, ErrRunNotFound
		}
	}
	return s.store.CheckpointsOfRun(ctx, runID)
}

// LatestOfMission 任务级最新（跨 Run——0.7.4 续办装配入口；无落盘 ErrNotFound）。
func (s *Service) LatestOfMission(ctx context.Context, missionID string) (Checkpoint, error) {
	cp, ok, err := s.store.LatestOfMission(ctx, missionID)
	if err != nil {
		return Checkpoint{}, err
	}
	if !ok {
		return Checkpoint{}, ErrNotFound
	}
	return cp, nil
}

// CheckpointsOfMission 任务级全量（任意 source 含 system，降序）——导入
// 幂等查重面（复审 #4：system 条不在 LatestOfMission 选取内，按水位比对
// 须全量覆盖）。
func (s *Service) CheckpointsOfMission(ctx context.Context, missionID string) ([]Checkpoint, error) {
	return s.store.CheckpointsOfMission(ctx, missionID)
}

// LatestForResume 续办装配入口（二审 #1）：**业务快照优先**——无业务快照
// 时回退最新系统终态摘要（停止/完成/失败事实仍可达新会话；不遮蔽业务字段
// 的一审 #1 语义保持——有业务条时系统条永不入选）。两者皆无 ErrNotFound。
func (s *Service) LatestForResume(ctx context.Context, missionID string) (Checkpoint, error) {
	cp, ok, err := s.store.LatestOfMission(ctx, missionID) // 排除 system
	if err != nil {
		return Checkpoint{}, err
	}
	if ok {
		return cp, nil
	}
	// 回退：最新系统摘要。
	sys, hasSys, err := s.store.LatestSystemOfMission(ctx, missionID)
	if err != nil {
		return Checkpoint{}, err
	}
	if hasSys {
		return sys, nil
	}
	return Checkpoint{}, ErrNotFound
}

// Latest 最新一条（无落盘 ErrNotFound——恢复面区分"无检查点"与"有"）。
func (s *Service) Latest(ctx context.Context, runID string) (Checkpoint, error) {
	items, err := s.ListOfRun(ctx, runID)
	if err != nil {
		return Checkpoint{}, err
	}
	if len(items) == 0 {
		return Checkpoint{}, ErrNotFound
	}
	return items[0], nil
}

func cTrim(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func copyWatermarks(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// newToken 生成式后缀（与 dispatch/memory 同族）。
func newToken() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}
