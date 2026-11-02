// Package delivery DeliveryPackage 成果提交域（RFC-0018 §3.3/§10.2/§11.2 /
// ADR-0031，0.6.8）。"执行成功 / 成果提交 / 验收通过"是三个互不替代的事实
// ——本域交付第二个：对一次可交付 Run 提交结构化交付包（成果引用、验收逐项
// 对应、验证证据、未解决项、风险、动作回执、建议记忆变更），**不自动送验**
// （对 Mission 状态/版本零副作用——端口上没有任何 Mission 变更方法，结构性
// 保证；送验随 0.6.9 request_review、回房随 0.6.10）。
//
// append-only：每次提交新建一行（补证 = 新包）；Mission 的"当前交付包" =
// submitted_at 最新。提交回执落来源 Run 的事件流（delivery_submitted）。
package delivery

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// EventDeliverySubmitted 来源 Run 流的交付回执事件（payload {package_id}）。
const EventDeliverySubmitted = "delivery_submitted"

// 可交付的来源 Run 形态（run 域同名字面量——域间不互相 import）。
const (
	runStateCompleted           = "completed"
	runStateUnknown             = "unknown"
	resolutionVerifiedCompleted = "verified_completed"
	missionStatusCanceled       = "canceled"
)

// 领域错误。
var (
	// ErrInvalidDelivery 形状/上限不合法（校验单源 Validate）。
	ErrInvalidDelivery = errors.New("delivery: 交付包不合法")
	// ErrMissionNotFound Mission 不存在。
	ErrMissionNotFound = errors.New("delivery: Mission 不存在")
	// ErrRunNotFound 来源 Run 不存在。
	ErrRunNotFound = errors.New("delivery: Run 不存在")
	// ErrVersionConflict expected_version ≠ Mission 当前契约版本（验收逐项对应
	// 锚定契约版本——契约已修订则映射陈旧）。
	ErrVersionConflict = errors.New("delivery: 契约版本冲突")
	// ErrStaleGeneration 请求代次 ≠ Run 实际代次（调用方视图陈旧——拒绝，不静
	// 默以当前代次改写；§11.2"迟到只审计"的提交面兑现）。
	ErrStaleGeneration = errors.New("delivery: 代次陈旧")
	// ErrRunMismatch Run 不属该 Mission。
	ErrRunMismatch = errors.New("delivery: Run 不属该 Mission")
	// ErrRunNotDeliverable Run 无产物可交付（非 completed / 非 unknown+verified_completed）。
	ErrRunNotDeliverable = errors.New("delivery: Run 不可交付")
	// ErrMissionCanceled Mission 已取消（终态——不再受理交付；读面照常）。
	ErrMissionCanceled = errors.New("delivery: Mission 已取消")
	// ErrPackageNotFound 交付包不存在。
	ErrPackageNotFound = errors.New("delivery: 交付包不存在")
)

// MappingEntry 验收逐项对应：criterion 为契约版本内验收条文快照，evidence 为
// 对应证据/说明（可空——补证随后续提交）。
type MappingEntry struct {
	Criterion string `json:"criterion"`
	Evidence  string `json:"evidence"`
}

// Package 交付包（schema: DeliveryPackageView）。
type Package struct {
	PackageID            string         `json:"package_id"`
	MissionID            string         `json:"mission_id"`
	RunID                string         `json:"run_id"`
	Attempt              int            `json:"attempt"`
	Generation           int            `json:"generation"`
	ContractVersion      int            `json:"contract_version"`
	ArtifactRefs         []string       `json:"artifact_refs"`
	AcceptanceMapping    []MappingEntry `json:"acceptance_mapping"`
	VerificationEvidence string         `json:"verification_evidence"`
	OpenItems            []string       `json:"open_items"`
	Risks                []string       `json:"risks"`
	ActionReceipts       []string       `json:"action_receipts"`
	SuggestedMemory      []string       `json:"suggested_memory"`
	Notes                string         `json:"notes"`
	SubmittedAt          string         `json:"submitted_at"`
}

// Submission 提交请求（schema: DeliveryPackageSubmit）。
type Submission struct {
	RunID                string
	Generation           int
	ExpectedVersion      int
	ArtifactRefs         []string
	AcceptanceMapping    []MappingEntry
	VerificationEvidence string
	OpenItems            []string
	Risks                []string
	ActionReceipts       []string
	SuggestedMemory      []string
	Notes                string
}

// ---- 端口（只读事实 + Run 流回执；无 Mission 变更方法——零副作用结构性保证）----

// MissionFacts Mission 事实摘要。
type MissionFacts struct {
	MissionID       string
	ContractVersion int
	Status          string
}

// Missions 契约域只读端口。
type Missions interface {
	MissionFacts(ctx context.Context, missionID string) (MissionFacts, bool, error)
}

// RunFacts 来源 Run 事实摘要。
type RunFacts struct {
	RunID      string
	MissionID  string
	Attempt    int
	Generation int
	State      string
	Resolution string
}

// Runs 执行域只读端口（回执由存储层随包同事务落 Run 流——端口不再承担写）。
type Runs interface {
	RunFacts(ctx context.Context, runID string) (RunFacts, bool, error)
}

// Receipt 提交回执（来源 Run 流事件——与包同事务落库，0.6.8 审校 #2）。
type Receipt struct {
	RunID     string
	EventType string
	Payload   map[string]any
}

// Store 持久化端口（append-only）。
type Store interface {
	// CreatePackage 单事务：**复核 Mission 行**（contract_version == p.ContractVersion
	// 且 status != canceled——0.6.8 审校 #1：端口读与写之间的并发修订/取消不得
	// 放行）→ 写包 → 写回执（Run 流事件，同事务——审校 #2：无"包落了回执丢"
	// 的部分成功）。守卫不符分别返回 ErrVersionConflict / ErrMissionCanceled /
	// ErrMissionNotFound；任一步失败整体不落。
	CreatePackage(ctx context.Context, p Package, receipt Receipt) error
	// ListPackagesByMission 按**真实提交时间**倒序（首项即当前交付包；
	// submitted_at 为 RFC3339Nano 小数位可变——不得按字符串序，审校 #3）。
	ListPackagesByMission(ctx context.Context, missionID string) ([]Package, error)
	PackageByID(ctx context.Context, packageID string) (Package, bool, error)
}

// SubmittedTime 解析 submitted_at（RFC3339Nano）；不可解析 ok=false（调用方退回
// 字符串序兜底——两存储共用，审校 #3）。
func SubmittedTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// LaterThan 真实时间倒序比较（a 晚于 b）；任一不可解析退回字符串比较。
func LaterThan(a, b string) bool {
	ta, oka := SubmittedTime(a)
	tb, okb := SubmittedTime(b)
	if oka && okb {
		return ta.After(tb)
	}
	return a > b
}

// Option 装配选项。
type Option func(*Service)

// WithNow 注入时钟。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewPackageID 注入 ID 生成器（缺省 dlv_ + 时间有序 + 随机后缀）。
func WithNewPackageID(f func() string) Option { return func(s *Service) { s.newID = f } }

// Service 交付服务。
type Service struct {
	store    Store
	missions Missions
	runs     Runs
	now      func() string
	newID    func() string
}

// New 装配服务。
func New(store Store, missions Missions, runs Runs, opts ...Option) *Service {
	s := &Service{
		store: store, missions: missions, runs: runs,
		now:   func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newID: newPackageID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func newPackageID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "dlv_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Submit = submit_result（ADR-0031 决策 2）：形状校验 → Mission 事实预读（存在/
// 非 canceled/契约版本——早拒）→ Run 事实（存在/归属/代次/可交付）→ 存储单
// 事务（事务内复核 Mission 版本与取消 + 落包 + Run 流回执——审校 #1/#2）。对
// Mission 零副作用；完备性不在此检查（先交付再补证）。
func (s *Service) Submit(ctx context.Context, missionID string, sub Submission) (Package, error) {
	if err := Validate(sub); err != nil {
		return Package{}, err
	}
	m, ok, err := s.missions.MissionFacts(ctx, missionID)
	if err != nil {
		return Package{}, err
	}
	if !ok {
		return Package{}, ErrMissionNotFound
	}
	if m.Status == missionStatusCanceled {
		return Package{}, fmt.Errorf("%w: 不再受理交付", ErrMissionCanceled)
	}
	if m.ContractVersion != sub.ExpectedVersion {
		return Package{}, fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, sub.ExpectedVersion, m.ContractVersion)
	}
	r, ok, err := s.runs.RunFacts(ctx, sub.RunID)
	if err != nil {
		return Package{}, err
	}
	if !ok {
		return Package{}, ErrRunNotFound
	}
	if r.MissionID != missionID {
		return Package{}, fmt.Errorf("%w: %s 属 %s", ErrRunMismatch, sub.RunID, r.MissionID)
	}
	if r.Generation != sub.Generation {
		return Package{}, fmt.Errorf("%w: 请求 gen %d，实际 gen %d", ErrStaleGeneration, sub.Generation, r.Generation)
	}
	if !deliverable(r) {
		return Package{}, fmt.Errorf("%w: state=%s resolution=%q", ErrRunNotDeliverable, r.State, r.Resolution)
	}
	p := Package{
		PackageID: s.newID(), MissionID: missionID, RunID: r.RunID,
		Attempt: r.Attempt, Generation: r.Generation, ContractVersion: m.ContractVersion,
		ArtifactRefs:         normalize(sub.ArtifactRefs),
		AcceptanceMapping:    normalizeMapping(sub.AcceptanceMapping),
		VerificationEvidence: sub.VerificationEvidence,
		OpenItems:            normalize(sub.OpenItems),
		Risks:                normalize(sub.Risks),
		ActionReceipts:       normalize(sub.ActionReceipts),
		SuggestedMemory:      normalize(sub.SuggestedMemory),
		Notes:                sub.Notes,
		SubmittedAt:          s.now(),
	}
	// 单事务：事务内复核 Mission（版本/取消）→ 包 → 回执（审校 #1/#2）。端口
	// 预读只用于早拒与清晰错误；权威判定在存储事务内。
	if err := s.store.CreatePackage(ctx, p, Receipt{
		RunID: r.RunID, EventType: EventDeliverySubmitted,
		Payload: map[string]any{"package_id": p.PackageID},
	}); err != nil {
		return Package{}, err
	}
	return p, nil
}

// deliverable 可交付判定：系统观测的完成，或用户 attest 的完成（ADR-0030）。
func deliverable(r RunFacts) bool {
	if r.State == runStateCompleted {
		return true
	}
	return r.State == runStateUnknown && r.Resolution == resolutionVerifiedCompleted
}

// List Mission 交付包（倒序）；Mission 未知 ok=false。
func (s *Service) List(ctx context.Context, missionID string) ([]Package, bool, error) {
	if _, ok, err := s.missions.MissionFacts(ctx, missionID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	list, err := s.store.ListPackagesByMission(ctx, missionID)
	if err != nil {
		return nil, true, err
	}
	if list == nil {
		list = []Package{}
	}
	return list, true, nil
}

// Get 单包；未知 ok=false。
func (s *Service) Get(ctx context.Context, packageID string) (Package, bool, error) {
	return s.store.PackageByID(ctx, packageID)
}

// ---- 校验单源（ADR-0031 决策 4）----

var runIDPattern = regexp.MustCompile(`^run_[0-9A-Za-z_-]+$`)

// Validate 形状与上限（完备性不在此——先交付再补证）。
func Validate(sub Submission) error {
	if !runIDPattern.MatchString(sub.RunID) {
		return fmt.Errorf("%w: run_id 形状非法", ErrInvalidDelivery)
	}
	if sub.Generation < 1 {
		return fmt.Errorf("%w: generation 须 ≥1", ErrInvalidDelivery)
	}
	if sub.ExpectedVersion < 1 {
		return fmt.Errorf("%w: expected_version 须 ≥1", ErrInvalidDelivery)
	}
	if err := lines("artifact_refs", sub.ArtifactRefs, 16, 256); err != nil {
		return err
	}
	if len(sub.AcceptanceMapping) > 16 {
		return fmt.Errorf("%w: acceptance_mapping ≤16 项", ErrInvalidDelivery)
	}
	for i, e := range sub.AcceptanceMapping {
		if n := utf8.RuneCountInString(strings.TrimSpace(e.Criterion)); n < 1 || n > 256 {
			return fmt.Errorf("%w: acceptance_mapping[%d].criterion 须 1–256 字", ErrInvalidDelivery, i)
		}
		if utf8.RuneCountInString(e.Evidence) > 1000 {
			return fmt.Errorf("%w: acceptance_mapping[%d].evidence ≤1000 字", ErrInvalidDelivery, i)
		}
	}
	if utf8.RuneCountInString(sub.VerificationEvidence) > 4000 {
		return fmt.Errorf("%w: verification_evidence ≤4000 字", ErrInvalidDelivery)
	}
	if err := lines("open_items", sub.OpenItems, 16, 256); err != nil {
		return err
	}
	if err := lines("risks", sub.Risks, 8, 256); err != nil {
		return err
	}
	if err := lines("action_receipts", sub.ActionReceipts, 16, 128); err != nil {
		return err
	}
	if err := lines("suggested_memory", sub.SuggestedMemory, 8, 256); err != nil {
		return err
	}
	if utf8.RuneCountInString(sub.Notes) > 1000 {
		return fmt.Errorf("%w: notes ≤1000 字", ErrInvalidDelivery)
	}
	return nil
}

func lines(field string, items []string, maxItems, maxLen int) error {
	if len(items) > maxItems {
		return fmt.Errorf("%w: %s ≤%d 项", ErrInvalidDelivery, field, maxItems)
	}
	for i, it := range items {
		if n := utf8.RuneCountInString(strings.TrimSpace(it)); n < 1 || n > maxLen {
			return fmt.Errorf("%w: %s[%d] 须 1–%d 字", ErrInvalidDelivery, field, i, maxLen)
		}
	}
	return nil
}

// normalize 空切片补齐 []（JSON 往返稳定）。
func normalize(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func normalizeMapping(items []MappingEntry) []MappingEntry {
	if items == nil {
		return []MappingEntry{}
	}
	return items
}
