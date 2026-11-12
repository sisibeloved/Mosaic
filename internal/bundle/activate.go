// activate.go —— 0.7.7 / ADR-0044：Bundle 导入激活与执行权接管（RFC-0018 §8.3）。
//
// 语义与边界：
//   - 两种模式：takeover（同身份承接——目标已有同 ID Agent，必须人工确认
//     源端已撤销执行权）/ copy（新身份复制——新 AgentID + source 记 origin，
//     RFC"复制生成新 AgentID、记录 origin_agent_id"）；模式与实例事实不匹配
//     → 409（不自动推断——显式声明是防双写第一道门）；
//   - 防同身份双写：takeover 确认门是人工声明非验证（RFC"不能靠包内一个
//     '已停机'字段证明外部执行者已经停止"）——记录声明时间，责任在声明人；
//   - 激活时实时复核（blockers 是预检快照）：重解析包（完整性顺带重验）+
//     任务同 ID 冲突 / 依赖可用性——任一存在 422；
//   - 跨域顺序写入 + origin 标记幂等（无跨域事务）：每项写入以
//     "bundle:<bundle_id>:<origin_id>" 进 SourceRefs / source 溯源，
//     attempt.json 落进行中映射——失败保持 inactive，重激活不重复写；
//   - 承诺一律非活跃承接（ready→paused；draft 留 draft）；Slot 任职不恢复
//     （0.7.9 题目）；授权与唤醒不继承（包内本就无）。
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 激活面常量。
const (
	ModeTakeover = "takeover"
	ModeCopy     = "copy"
	pauseReason  = "bundle-import" // 承诺承接暂停原因前缀（附 import_id）
	attemptName  = "attempt.json"  // 进行中映射（契约外内部文件）
)

var (
	// ErrModeMismatch 模式与实例事实不匹配（409——message 指出可用模式）。
	ErrModeMismatch = errors.New("bundle: activation mode mismatch")
	// ErrConfirmRequired takeover 未确认源端已停（400）。
	ErrConfirmRequired = errors.New("bundle: source stop confirmation required")
	// ErrActivationBlocked 实时复核不过（422——明细聚合）。
	ErrActivationBlocked = errors.New("bundle: activation blocked")
)

// ---- 视图（JSON 形状即 API 契约形状）----

// MissionMapping 任务 ID 映射（RFC §8.3"显式承接和对象映射"——一等公民）。
type MissionMapping struct {
	OriginMissionID string `json:"origin_mission_id"`
	LocalMissionID  string `json:"local_mission_id"`
	Status          string `json:"status"` // paused | draft
}

// ActivationCounts 激活写入计数（skipped = 幂等已写或口径排除）。
type ActivationCounts struct {
	MemoriesCreated    int `json:"memories_created"`
	MemoriesSkipped    int `json:"memories_skipped"`
	MissionsCreated    int `json:"missions_created"`
	MissionsSkipped    int `json:"missions_skipped"`
	CheckpointsCreated int `json:"checkpoints_created"`
	CheckpointsSkipped int `json:"checkpoints_skipped"`
}

// ActivationView 激活结果（成功响应与边车 activation 块同构）。
type ActivationView struct {
	ImportID      string `json:"import_id"`
	BundleID      string `json:"bundle_id"`
	Mode          string `json:"mode"`
	Status        string `json:"status"` // 恒 activated
	ActivatedAt   string `json:"activated_at"`
	OriginAgentID string `json:"origin_agent_id"`
	LocalAgentID  string `json:"local_agent_id"`
	// ConfirmSourceStoppedAt takeover 的人工声明时间（非验证——RFC §8.3）。
	ConfirmSourceStoppedAt string           `json:"confirm_source_stopped_at,omitempty"`
	Missions               []MissionMapping `json:"missions"`
	Counts                 ActivationCounts `json:"counts"`
}

// ActivationRequest 激活请求。
type ActivationRequest struct {
	Mode                 string // takeover | copy
	ConfirmSourceStopped bool
}

// attemptFile 激活进行中映射（bundle-imports/<id>/attempt.json——失败留痕，
// 重试复用；成功后删除）。契约外内部文件。
type attemptFile struct {
	Mode        string           `json:"mode"`
	TargetAgent string           `json:"target_agent_id"`
	ConfirmAt   string           `json:"confirm_source_stopped_at,omitempty"`
	Missions    []MissionMapping `json:"missions"`
	UpdatedAt   string           `json:"updated_at"`
}

// ---- 端口与服务 ----

// MissionRef 任务创建结果（域投影——owner 侧新事实）。
type MissionRef struct {
	MissionID       string
	ContractVersion int
	Status          string
}

// ActivationPorts 激活写入端口（全闭包投影——bundle 不 import 各域，
// 0.7.5/0.7.6 依赖方向纪律）。
type ActivationPorts struct {
	// AgentEnsureImported copy 模式幂等建身份（agentID 非空 = 重试复用映射；
	// 返回实际 agent_id）。
	AgentEnsureImported func(ctx context.Context, displayName, adapter, agentID, originRef string) (string, error)
	AgentExists         func(ctx context.Context, agentID string) (bool, error)
	// MissionCreate 建契约并原子 paused 承接（一审 #1——无 ready 窗口）；
	// origin 标记进 SourceRefs；不完备契约 draft。
	MissionCreate func(ctx context.Context, ownerAgentID string, c ContractFacts, originRef, pauseReason string) (MissionRef, error)
	// MissionPause paused 承接（幂等——draft 由调用方判态跳过）。
	MissionPause func(ctx context.Context, missionID string, version int, reason string) error
	// MissionState 同 ID 任务对照（存在性复核——0.7.6 口径复用）。
	MissionState func(ctx context.Context, missionID string) (MissionState, bool, error)
	// MissionsOfOwner owner 名下任务全量（幂等查重——SourceRefs 比对）。
	MissionsOfOwner func(ctx context.Context, ownerAgentID string) ([]MissionFact, error)
	// CheckpointImported 幂等查重（复审 #4）：任务下**任一**检查点（业务与
	// 系统两类全量覆盖——system 条不在 LatestOfMission 选取内）水位
	// bundle.origin_checkpoint == 源 checkpoint_id 即已导入。
	CheckpointImported func(ctx context.Context, missionID, originCheckpointID string) (bool, error)
	// CheckpointImport 导入检查点落库（checkpoint 域 CreateImported 面映射）：
	// 全新 checkpoint_id/run_id 域生成、Source 保留源语义（""/system）、
	// checkpoint_version 存储原子分配；targetMissionID/targetVersion 为目标
	// 新任务锚，marks 携 bundle.origin_* 溯源与幂等水位。
	CheckpointImport func(ctx context.Context, ckpt CheckpointFact, targetMissionID string, targetVersion int, marks map[string]string) error
	// MemoryCreate 写记忆（scope 重锚 + origin 标记——域生成新 mem_* ID）。
	MemoryCreate func(ctx context.Context, item MemoryFact, scopeID, originRef string) error
	// MemoryList scope 记忆全量（幂等查重——SourceRefs 比对）。
	MemoryList func(ctx context.Context, namespace, scopeID string) ([]MemoryFact, error)
	// Harnesses 本实例可执行项集（复核——0.7.6 口径复用）。
	Harnesses func(ctx context.Context) ([]string, error)
}

// ActivationService 激活服务（复用 ImportService 的记录面与互斥锁）。
type ActivationService struct {
	imp   *ImportService
	ports ActivationPorts
}

// NewActivation 构造。
func NewActivation(imp *ImportService, ports ActivationPorts) *ActivationService {
	return &ActivationService{imp: imp, ports: ports}
}

// Activate 激活编排（ADR-0044 §2/§3）：复核 → 身份 → 任务/检查点/记忆 →
// 边车回写。任一步失败保持 inactive（attempt 留痕），重激活幂等。
func (s *ActivationService) Activate(ctx context.Context, importID string, req ActivationRequest) (ActivationView, error) {
	switch req.Mode {
	case ModeTakeover, ModeCopy:
	default:
		return ActivationView{}, fmt.Errorf("%w: 模式须 takeover|copy（得 %q）", ErrInvalid, req.Mode)
	}
	s.imp.mu.Lock()
	defer s.imp.mu.Unlock()

	// 1) 读记录（白名单 + 存在性；已激活幂等返回既有结果）。
	if !importIDRe.MatchString(importID) {
		return ActivationView{}, fmt.Errorf("%w: import_id 不合法", ErrInvalid)
	}
	view, ok, err := s.imp.readSidecar(importID)
	if err != nil {
		return ActivationView{}, err
	}
	if !ok {
		return ActivationView{}, ErrNotFound
	}
	if view.Activation != nil {
		return *view.Activation, nil
	}

	// 2) 重解析包（完整性顺带重验——激活以包内容为写入事实源）。
	zipPath := filepath.Join(s.imp.dir, importDirName, importID, zipName)
	content, violations := s.imp.runGates(zipPath)
	if len(violations) > 0 {
		return ActivationView{}, fmt.Errorf("%w: 包重校验不过：%s", ErrActivationBlocked, strings.Join(violations, "；"))
	}
	originAgent := content.profile.AgentID
	originRef := "bundle:" + content.manifest.BundleID + ":" + originAgent

	// 3) 身份：模式与实例事实匹配门（防双写第一道）。
	exists, err := s.ports.AgentExists(ctx, originAgent)
	if err != nil {
		return ActivationView{}, fmt.Errorf("bundle: agent 对照: %w", err)
	}
	switch req.Mode {
	case ModeTakeover:
		if !exists {
			return ActivationView{}, fmt.Errorf("%w: 本实例无 Agent %s——应使用 copy（新身份复制）", ErrModeMismatch, originAgent)
		}
		if !req.ConfirmSourceStopped {
			return ActivationView{}, fmt.Errorf("%w: takeover 须确认源实例已撤销该 Agent 的执行权（confirm_source_stopped）", ErrConfirmRequired)
		}
	case ModeCopy:
		if exists {
			return ActivationView{}, fmt.Errorf("%w: 本实例已存在 Agent %s——应使用 takeover 并确认源端已停", ErrModeMismatch, originAgent)
		}
	}
	// attempt 复用（同模式的未遂尝试——映射不跨模式复用）。
	attempt, _ := s.readAttempt(importID)
	if attempt.Mode != req.Mode {
		attempt = attemptFile{Mode: req.Mode}
	}
	targetAgent := attempt.TargetAgent
	confirmAt := attempt.ConfirmAt
	if req.Mode == ModeTakeover {
		confirmAt = s.imp.now() // 声明时间取本次（每次 takeover 都是新的确认）
		targetAgent = originAgent
	}
	fail := func(err error) (ActivationView, error) {
		attempt.TargetAgent, attempt.ConfirmAt, attempt.UpdatedAt = targetAgent, confirmAt, s.imp.now()
		s.writeAttempt(importID, &attempt)
		return ActivationView{}, err
	}
	if targetAgent == "" { // copy 首次：建身份（幂等——重试经 attempt 复用 ID）
		if targetAgent, err = s.ports.AgentEnsureImported(ctx, content.profile.DisplayName, content.profile.Adapter, "", originRef); err != nil {
			return fail(fmt.Errorf("bundle: 建身份: %w", err))
		}
	}

	// 4) 实时复核：同 ID 任务冲突（同库重复激活防线）+ 依赖可用性。
	var blocked []string
	for _, m := range content.commitments.Missions {
		if local, ok, err := s.ports.MissionState(ctx, m.MissionID); err != nil {
			return fail(fmt.Errorf("bundle: mission 对照: %w", err))
		} else if ok {
			blocked = append(blocked, fmt.Sprintf("任务 %s 已存在于本实例（v%d）——同 ID 冲突须先治理既有任务", m.MissionID, local.ContractVersion))
		}
	}
	if s.ports.Harnesses != nil {
		local, err := s.ports.Harnesses(ctx)
		if err != nil {
			blocked = append(blocked, fmt.Sprintf("依赖对照失败：%v", err))
		} else {
			have := map[string]bool{}
			for _, h := range local {
				have[h] = true
			}
			refs := map[string]bool{}
			for _, e := range content.deps.Harness {
				if e.ExecutableID != "" {
					refs[e.ExecutableID] = true
				}
			}
			for _, e := range content.profile.Executables {
				if e.ExecutableID != "" {
					refs[e.ExecutableID] = true
				}
			}
			var missing []string
			for id := range refs {
				if !have[id] {
					missing = append(missing, id)
				}
			}
			if len(missing) > 0 {
				blocked = append(blocked, fmt.Sprintf("本实例缺少可执行项：%s——激活前须注册", strings.Join(missing, "、")))
			}
		}
	}
	if len(blocked) > 0 {
		return fail(fmt.Errorf("%w：%s", ErrActivationBlocked, strings.Join(blocked, "；")))
	}

	// 5) 任务承诺：origin 标记幂等 + 新 ID + 非活跃承接。
	missionMap := map[string]MissionMapping{} // origin → 映射（含 attempt 复用）
	localVersions := map[string]int{}         // origin → 新任务当前契约版本
	// 复审 #1：attempt 映射不直接采信——记录是写入时刻的快照（暂停失败
	// 遗留 / 此后被恢复为 ready 都可能）；逐条在承诺循环里实时核验。
	out := ActivationView{
		ImportID: importID, BundleID: content.manifest.BundleID, Mode: req.Mode,
		Status: "activated", ActivatedAt: s.imp.now(),
		OriginAgentID: originAgent, LocalAgentID: targetAgent,
		ConfirmSourceStoppedAt: confirmAt,
		Missions:               []MissionMapping{},
	}
	reason := pauseReason + ":" + importID
	var attemptByOrigin map[string]MissionMapping // attempt 映射（复审 #1：仅作 localID 候选，状态实时核验）
	for _, mm := range attempt.Missions {
		if attemptByOrigin == nil {
			attemptByOrigin = map[string]MissionMapping{}
		}
		attemptByOrigin[mm.OriginMissionID] = mm
	}
	resumeMission := func(m MissionFact) (MissionMapping, bool, error) {
		// 两条恢复路径统一（复审 #1）：attempt 候选 → origin 标记查重；
		// 命中后一律实时核验并补做暂停——查询失败/不存在即中止，不得默认
		// paused 继续（状态未知 = 承接未完成）。
		localID := ""
		if mm, ok := attemptByOrigin[m.MissionID]; ok {
			localID = mm.LocalMissionID
		} else {
			found, err := s.missionByOrigin(ctx, targetAgent, "bundle:"+content.manifest.BundleID+":"+m.MissionID)
			if err != nil {
				return MissionMapping{}, false, err
			}
			localID = found
		}
		if localID == "" {
			return MissionMapping{}, false, nil
		}
		status, version, err := s.ensureMissionPaused(ctx, localID, reason)
		if err != nil {
			return MissionMapping{}, false, err
		}
		localVersions[m.MissionID] = version
		return MissionMapping{OriginMissionID: m.MissionID, LocalMissionID: localID, Status: status}, true, nil
	}
	for _, m := range content.commitments.Missions {
		originMissionRef := "bundle:" + content.manifest.BundleID + ":" + m.MissionID
		if mm, resumed, err := resumeMission(m); err != nil {
			return fail(err)
		} else if resumed {
			missionMap[m.MissionID] = mm
			attempt.Missions = append(attempt.Missions, mm)
			out.Missions = append(out.Missions, mm)
			out.Counts.MissionsSkipped++
			continue
		}
		// 一审 #1：MissionCreate 端口为原子 paused 创建（完备契约无 ready
		// 调度窗口——普通 Create+Pause 两事务间可被调度器认领是审校反例）。
		ref, err := s.ports.MissionCreate(ctx, targetAgent, m.Contract, originMissionRef, reason)
		if err != nil {
			return fail(fmt.Errorf("bundle: 建任务 %s: %w", m.MissionID, err))
		}
		mm := MissionMapping{OriginMissionID: m.MissionID, LocalMissionID: ref.MissionID, Status: ref.Status}
		missionMap[m.MissionID] = mm
		localVersions[m.MissionID] = ref.ContractVersion
		attempt.Missions = append(attempt.Missions, mm)
		out.Missions = append(out.Missions, mm)
		out.Counts.MissionsCreated++
	}

	// 6) 检查点（逐任务至多一条——包结构保证）：目标侧全新 ID/run（域生成
	// ——同源多包各自落条不撞主键，一审 #5）；源版本事实进水位（一审 #4：
	// 检查点快照的源契约版本 vs 包内任务契约版本——漂移信号不因版本空间
	// 映射丢失）；幂等键 = bundle.origin_checkpoint 水位（一审 #5）。
	originContract := map[string]int{}
	for _, m := range content.commitments.Missions {
		originContract[m.MissionID] = m.ContractVersion
	}
	for _, c := range content.ckpts {
		mm, ok := missionMap[c.MissionID]
		if !ok { // 承诺未承接（理论不可达——承诺先行）；防御跳过
			out.Counts.CheckpointsSkipped++
			continue
		}
		imported, err := s.ports.CheckpointImported(ctx, mm.LocalMissionID, c.CheckpointID)
		if err != nil {
			return fail(fmt.Errorf("bundle: 检查点查重 %s: %w", mm.LocalMissionID, err))
		}
		if imported {
			out.Counts.CheckpointsSkipped++
			continue
		}
		targetVersion := localVersions[c.MissionID]
		if targetVersion < 1 {
			targetVersion = 1
		}
		// 三审 #1：连续迁移（A→B→C）链上未解决的漂移证据保留——包内检查点
		// 自带 bundle.origin_versions（上一跳已记的源版本事实）且快照未更新时，
		// 不得按本跳重映射后的版本重算覆盖（否则 A 的 checkpoint=v1:contract=v2
		// 被 v1:contract=v1 抹平、C 侧漂移误 false）；仅新快照（无证据）按
		// 本跳事实生成。
		originVersions := c.SourceWatermarks["bundle.origin_versions"]
		if originVersions == "" {
			originVersions = fmt.Sprintf("checkpoint=v%d:contract=v%d", c.MissionVersion, originContract[c.MissionID])
		}
		marks := map[string]string{
			"bundle.origin_bundle":     content.manifest.BundleID,
			"bundle.origin_checkpoint": c.CheckpointID,
			"bundle.origin_run":        c.RunID,
			"bundle.origin_mission":    c.MissionID,
			"bundle.origin_versions":   originVersions,
			"bundle.import":            importID,
		}
		if err := s.ports.CheckpointImport(ctx, c, mm.LocalMissionID, targetVersion, marks); err != nil {
			return fail(fmt.Errorf("bundle: 导入检查点 %s: %w", c.CheckpointID, err))
		}
		out.Counts.CheckpointsCreated++
	}

	// 7) 记忆：scope 重锚（agent→目标身份；workspace→default；mission→映射）
	// + origin 幂等；策展/时效/血缘不移植（0.7.5 导出口径）。
	for _, it := range content.memories {
		scope := it.ScopeID
		switch it.Namespace {
		case nsAgent:
			scope = targetAgent
		case nsWorkspace:
			scope = "default"
		case nsMission:
			mm, ok := missionMap[it.ScopeID]
			if !ok {
				out.Counts.MemoriesSkipped++ // 任务未承接——mission 域记忆不落
				continue
			}
			scope = mm.LocalMissionID
		default:
			return fail(fmt.Errorf("%w: 记忆命名空间未知（%s：%s）", ErrInvalid, it.MemoryID, it.Namespace))
		}
		switch it.Kind {
		case "semantic", "episodic", "procedural":
		default:
			return fail(fmt.Errorf("%w: 记忆类型未知（%s：%s）", ErrInvalid, it.MemoryID, it.Kind))
		}
		originMemRef := "bundle:" + content.manifest.BundleID + ":" + it.MemoryID
		written, err := s.memoryWritten(ctx, it.Namespace, scope, originMemRef)
		if err != nil {
			return fail(err)
		}
		if written {
			out.Counts.MemoriesSkipped++
			continue
		}
		if err := s.ports.MemoryCreate(ctx, it, scope, originMemRef); err != nil {
			return fail(fmt.Errorf("bundle: 写记忆 %s: %w", it.MemoryID, err))
		}
		out.Counts.MemoriesCreated++
	}

	// 8) 落定：边车 status=activated + activation 块；attempt 清除。
	view.Status = "activated"
	view.Activation = &out
	if err := s.imp.writeSidecar(importID, view); err != nil {
		return ActivationView{}, err
	}
	_ = os.Remove(filepath.Join(s.imp.dir, importDirName, importID, attemptName))
	return out, nil
}

// ensureMissionPaused 既有承接任务的实时核验（复审 #1）：查询失败/不存在
// → 中止（状态未知不得默认 paused 继续）；ready/running/waiting → 补做暂停
// （上次暂停失败的遗留）；paused/draft → 就绪。返回（核验后状态, 当前契约版本）。
func (s *ActivationService) ensureMissionPaused(ctx context.Context, missionID, reason string) (string, int, error) {
	st, ok, err := s.ports.MissionState(ctx, missionID)
	if err != nil {
		return "", 0, fmt.Errorf("bundle: 任务状态核验 %s: %w", missionID, err)
	}
	if !ok {
		return "", 0, fmt.Errorf("%w: 承接任务 %s 不存在于本实例（映射失效）", ErrActivationBlocked, missionID)
	}
	switch st.Status {
	case "ready", "running", "waiting":
		if err := s.ports.MissionPause(ctx, missionID, st.ContractVersion, reason); err != nil {
			return "", 0, fmt.Errorf("bundle: 补暂停任务 %s: %w", missionID, err)
		}
		return "paused", st.ContractVersion, nil
	default:
		return st.Status, st.ContractVersion, nil
	}
}

// missionByOrigin owner 名下契约 SourceRefs 含 origin 标记的任务 ID（无 = ""）。
func (s *ActivationService) missionByOrigin(ctx context.Context, owner, originRef string) (string, error) {
	missions, err := s.ports.MissionsOfOwner(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("bundle: 任务查重: %w", err)
	}
	for _, m := range missions {
		for _, r := range m.Contract.SourceRefs {
			if r == originRef {
				return m.MissionID, nil
			}
		}
	}
	return "", nil
}

// memoryWritten 同源记忆已写（SourceRefs 含标记）。
func (s *ActivationService) memoryWritten(ctx context.Context, ns, scope, originRef string) (bool, error) {
	items, err := s.ports.MemoryList(ctx, ns, scope)
	if err != nil {
		return false, fmt.Errorf("bundle: 记忆查重: %w", err)
	}
	for _, it := range items {
		for _, r := range it.SourceRefs {
			if r == originRef {
				return true, nil
			}
		}
	}
	return false, nil
}

// readAttempt 读进行中映射（无文件/坏文件 = 空零值——幂等兜底仍在 origin 标记）。
func (s *ActivationService) readAttempt(importID string) (attemptFile, error) {
	raw, err := os.ReadFile(filepath.Join(s.imp.dir, importDirName, importID, attemptName))
	if err != nil {
		return attemptFile{}, nil
	}
	var a attemptFile
	if json.Unmarshal(raw, &a) != nil {
		return attemptFile{}, nil
	}
	return a, nil
}

// writeAttempt 落进行中映射（best-effort——失败不阻断主流程）。
func (s *ActivationService) writeAttempt(importID string, a *attemptFile) {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.imp.dir, importDirName, importID, attemptName), append(raw, '\n'), 0o600)
}
