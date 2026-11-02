// Package review 送验/裁定编排（0.6.9 / ADR-0032）：以交付包为锚的必需交付项
// 检查（AT-10——"request_review 缺必需项时拒绝，补齐后进入 reviewing"）+
// mission 域验收状态机委托。"执行成功 / 成果提交 / 验收通过"三事实中的第三件
// 在此接通：request_review（必需项检查 → reviewing）与 review_mission
// （accepted 终态 | returned 回 ready）。
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// 领域错误。
var (
	// ErrPackageNotFound 交付包不存在。
	ErrPackageNotFound = errors.New("review: 交付包不存在")
	// ErrPackageMismatch 包不属该 Mission。
	ErrPackageMismatch = errors.New("review: 交付包不属该 Mission")
	// ErrStalePackage 包对应旧契约版本（契约已修订——须补证新包再送验）。
	ErrStalePackage = errors.New("review: 交付包对应旧契约版本")
	// ErrMissingRequiredItems 必需交付项缺失（AT-10——补证后重试）。
	ErrMissingRequiredItems = errors.New("review: 必需交付项缺失")
	// ErrRunsUnsettled 存在在途或未处置 unknown 的 Run（0.6.9 审校 #2：待核验
	// 完成再送验）。
	ErrRunsUnsettled = errors.New("review: 存在未决 Run")
)

// MissingRequiredItemsError 携缺项清单（"artifact_refs" 或 "acceptance:<条文>"）。
type MissingRequiredItemsError struct {
	Items []string
}

func (e *MissingRequiredItemsError) Error() string {
	return fmt.Sprintf("%v：%v", ErrMissingRequiredItems, e.Items)
}

func (e *MissingRequiredItemsError) Is(target error) bool { return target == ErrMissingRequiredItems }

// PackageFacts 交付包事实摘要（0.6.9 送验检查所需）。
type PackageFacts struct {
	PackageID         string
	MissionID         string
	ContractVersion   int
	ArtifactRefs      []string
	AcceptanceMapping []MappingEntry
}

// MappingEntry 验收逐项对应（与 delivery.MappingEntry 同构——域间不互相 import）。
type MappingEntry struct {
	Criterion string
	Evidence  string
}

// Packages 交付域只读端口。
type Packages interface {
	PackageFacts(ctx context.Context, packageID string) (PackageFacts, bool, error)
}

// Runs 执行域只读端口（0.6.9 审校 #2）：Mission 的在途（pending/running）与
// 未处置 unknown Run 计数——送验前置拦截（存储事务内另有权威复核）。
type Runs interface {
	Unsettled(ctx context.Context, missionID string) (inflight int, unresolvedUnknown int, err error)
}

// Service 验收编排服务（组合 mission 状态机、delivery 包事实与 run 残留事实）。
type Service struct {
	missions *mission.Service
	packages Packages
	runs     Runs
}

// New 装配服务。
func New(missions *mission.Service, packages Packages, runs Runs) *Service {
	return &Service{missions: missions, packages: packages, runs: runs}
}

// RequestReview = request_review（ADR-0032 决策 2）：包存在/归属/契约版本 →
// 必需交付项检查 → mission CAS（ready → reviewing + 锚）。Mission 域错误原样
// 透传（version_conflict / invalid_transition / mission_not_found）。
func (s *Service) RequestReview(ctx context.Context, missionID string, expectedVersion int, packageID string) (mission.Mission, error) {
	m, ok, err := s.missions.Get(ctx, missionID)
	if err != nil {
		return mission.Mission{}, err
	}
	if !ok {
		return mission.Mission{}, mission.ErrMissionNotFound
	}
	// 审校 #12：调用方版本须等于**读到并用于完备性检查**的版本——预填下一版本
	// 号与修订交错时，不得以旧标准校验后按新版本 CAS 放行；经校验的版本再作
	// 状态转换的 CAS 前提（事务内契约版本检查保留，后续修订只能导致冲突）。
	if m.ContractVersion != expectedVersion {
		return mission.Mission{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", mission.ErrVersionConflict, expectedVersion, m.ContractVersion)
	}
	// 审校 #2：在途或未处置 unknown Run 存在时不得送验（结果未核验的执行不能
	// 随任务被接受）——先核验/处置再送验。
	if s.runs != nil {
		inflight, unresolved, err := s.runs.Unsettled(ctx, missionID)
		if err != nil {
			return mission.Mission{}, err
		}
		if inflight > 0 || unresolved > 0 {
			return mission.Mission{}, fmt.Errorf("%w：在途 %d、未处置 unknown %d——核验处置后再送验", ErrRunsUnsettled, inflight, unresolved)
		}
	}
	p, ok, err := s.packages.PackageFacts(ctx, packageID)
	if err != nil {
		return mission.Mission{}, err
	}
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w：%s", ErrPackageNotFound, packageID)
	}
	if p.MissionID != missionID {
		return mission.Mission{}, fmt.Errorf("%w：%s 属 %s", ErrPackageMismatch, packageID, p.MissionID)
	}
	if p.ContractVersion != m.ContractVersion {
		return mission.Mission{}, fmt.Errorf("%w：包 v%d，契约 v%d（补证新包后重试）", ErrStalePackage, p.ContractVersion, m.ContractVersion)
	}
	if missing := missingRequired(m, p); len(missing) > 0 {
		return mission.Mission{}, &MissingRequiredItemsError{Items: missing}
	}
	return s.missions.RequestReview(ctx, missionID, expectedVersion, packageID)
}

// Review = review_mission（纯 mission 域委托：裁定校验/验收权限/轮次守卫 CAS 均在域内）。
func (s *Service) Review(ctx context.Context, missionID string, expectedVersion, reviewRound int, d mission.ReviewDecision) (mission.Mission, error) {
	return s.missions.RecordReview(ctx, missionID, expectedVersion, reviewRound, d)
}

// missingRequired 必需交付项（本版口径，ADR-0032 决策 2）：成果引用非空 + 当前
// 契约每条验收标准在包的 acceptance_mapping 中有对应条目且证据非空。
func missingRequired(m mission.Mission, p PackageFacts) []string {
	var missing []string
	if len(p.ArtifactRefs) == 0 {
		missing = append(missing, "artifact_refs")
	}
	for _, c := range m.AcceptanceCriteria {
		covered := false
		for _, e := range p.AcceptanceMapping {
			// 审校 #1：纯空白证据不算证据（TrimSpace 后非空）
			if e.Criterion == c && strings.TrimSpace(e.Evidence) != "" {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, "acceptance:"+c)
		}
	}
	return missing
}
