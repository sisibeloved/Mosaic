// Package monitor：M4-5 监控变化检测与有效唤醒（计划 §M4-5；RFC-0012 附录 I）。
//
// 显式监控源（脚本/URL）周期取快照、哈希比对：无变化记录 no-change 并跳过
// 该源的模型调用；有变化经 M4-1 任务执行通道（run_task）把"差异 + 最新快照"
// 交给指派 Agent，结果按 M4-4 稳定身份投回目标房间。
//
// 水位三分（观察/处理/送达）与失败分类（源失败、无变化、处理失败、投递失败）
// 分开记录——"先记哈希、模型失败后永远漏报"的病理由独立水位与重试路径堵死；
// 去重（no-change 跳过）与重试（未送达重发）互不干扰。
//
// 唤醒纪律：监控调度独立于群聊反应波与主动开口——房间无新消息不抑制监控
// 触发；监控去重规则不套用到普通讨论（结构性隔离：本包不触碰引擎波路径）。
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SourceKind 监控源类型。
const (
	KindScript = "script" // 本地脚本（绝对路径 + 参数，无 shell——同用户信任面）
	KindURL    = "url"    // HTTP GET
)

// 界限。
const (
	minInterval  = 10 * time.Second
	maxInterval  = 24 * time.Hour
	contentCap   = 64 << 10 // 存储的快照上限（差异计算输入）
	diffCap      = 8000     // 注入指令的差异上限（rune——run_task instruction ≤4000 的安全余量内截断）
	instrCap     = 3800     // 指令总长上限（≤4000 契约）
	monitorIDPfx = "mon_"
)

// Source 用户登记的监控源（投递目标：房间 + 指派 Agent——M4-4 稳定身份寻址）。
type Source struct {
	MonitorID   string   `json:"monitor_id"`
	Kind        string   `json:"kind"`           // script | url
	Target      string   `json:"target"`         // 脚本绝对路径 | URL
	Args        []string `json:"args,omitempty"` // 脚本参数
	RoomID      string   `json:"room_id"`        // 结果投递房间
	Assignee    string   `json:"assignee"`       // 受指派 Agent（par_*）
	IntervalSec int      `json:"interval_sec"`
	Instruction string   `json:"instruction,omitempty"` // 指令模板（空 = 缺省；{{diff}} 占位）
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"created_at"`
}

// State 每源运行态（水位与失败分类）。
type State struct {
	MonitorID         string `json:"monitor_id"`
	LastObserved      string `json:"last_observed,omitempty"`      // 观察水位：最近一次成功取得的快照哈希
	LastProcessed     string `json:"last_processed,omitempty"`     // 处理水位：最近交给 run 的哈希
	LastDelivered     string `json:"last_delivered,omitempty"`     // 送达水位：结果已回房的哈希
	LastContent       string `json:"last_content,omitempty"`       // 观察快照本体（contentCap 截断）
	ProcessingContent string `json:"processing_content,omitempty"` // 处理水位伴随：交给在途 run 的快照
	DeliveredContent  string `json:"delivered_content,omitempty"`  // 送达水位伴随：结果已回房的快照（重试/接力的差异基线）
	PendingHash       string `json:"pending_hash,omitempty"`       // 待处理水位：run 在途期间到达的新变化（不丢不重）
	PendingContent    string `json:"pending_content,omitempty"`
	InFlightRun       string `json:"in_flight_run,omitempty"`   // 在途 run_id
	LastError         string `json:"last_error,omitempty"`      // 源失败（取快照失败——与无变化/处理失败分开）
	ProcessError      string `json:"process_error,omitempty"`   // 处理/投递失败（run failed/canceled/unknown 或启动失败）
	NoChangeCount     int64  `json:"no_change_count,omitempty"` // 连续无变化次数（去重证据）
	LastCheckedAt     string `json:"last_checked_at,omitempty"`
	LastDeliveredAt   string `json:"last_delivered_at,omitempty"`
}

// Launcher 发起执行（M4-1 run_task 命令面；装配层注入——版本重试在注入侧）。
type Launcher interface {
	Launch(ctx context.Context, roomID, assignee, instruction string) (runID string, err error)
}

// RunStatus 查询 run 终态（"" = 未知/在读面缺失）。
type RunStatus func(ctx context.Context, roomID, runID string) string

// Fetcher 取快照（script/url 实现注入；UT 用 fake）。
type Fetcher func(ctx context.Context, src Source) (string, error)

// Manager 监控调度器（JSON 持久化——沿 registry/settings 形态）。
type Manager struct {
	mu      sync.Mutex
	path    string
	sources []Source
	states  map[string]*State

	Launcher Launcher
	Status   RunStatus
	Fetch    Fetcher
	Logger   *slog.Logger
	Now      func() time.Time
	NewID    func(prefix string) string
}

type document struct {
	Sources []Source          `json:"sources"`
	States  map[string]*State `json:"states"`
}

// Load 装载或初始化。
func Load(path string) (*Manager, error) {
	m := &Manager{path: path, states: map[string]*State{}, Now: time.Now,
		NewID:  func(p string) string { return p + fmt.Sprintf("%x", time.Now().UnixNano()) },
		Logger: slog.Default()}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("monitor: read: %w", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("monitor: parse: %w", err)
	}
	m.sources = doc.Sources
	for _, s := range doc.Sources {
		if st := doc.States[s.MonitorID]; st != nil {
			m.states[s.MonitorID] = st
		}
	}
	return m, nil
}

func (m *Manager) saveLocked() error {
	doc := document{Sources: m.sources, States: m.states}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// View 源 + 运行态合并视图（列表端点）。
type View struct {
	Source
	State *State `json:"state"`
}

// List 全量视图。
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.sources))
	for _, s := range m.sources {
		st := m.states[s.MonitorID]
		if st == nil {
			st = &State{MonitorID: s.MonitorID}
		}
		out = append(out, View{Source: s, State: st})
	}
	return out
}

// ErrInvalidSource 域错误。
var ErrInvalidSource = fmt.Errorf("monitor: 登记项不合法")

// Validate 登记校验。
func (s Source) Validate() error {
	if s.Kind != KindScript && s.Kind != KindURL {
		return fmt.Errorf("%w: kind 须为 script|url", ErrInvalidSource)
	}
	if strings.TrimSpace(s.Target) == "" {
		return fmt.Errorf("%w: target 必填", ErrInvalidSource)
	}
	if s.Kind == KindScript && !filepath.IsAbs(s.Target) {
		return fmt.Errorf("%w: 脚本须为绝对路径（无 shell——路径 + 参数直接执行）", ErrInvalidSource)
	}
	if s.Kind == KindURL && !strings.HasPrefix(s.Target, "http://") && !strings.HasPrefix(s.Target, "https://") {
		return fmt.Errorf("%w: URL 须为 http(s)", ErrInvalidSource)
	}
	if s.RoomID == "" || s.Assignee == "" {
		return fmt.Errorf("%w: room_id/assignee 必填（投递目标）", ErrInvalidSource)
	}
	d := time.Duration(s.IntervalSec) * time.Second
	if d < minInterval || d > maxInterval {
		return fmt.Errorf("%w: interval 须在 %d..%d 秒", ErrInvalidSource, int(minInterval.Seconds()), int(maxInterval.Seconds()))
	}
	if n := len([]rune(s.Instruction)); n > 2000 {
		return fmt.Errorf("%w: instruction 模板 ≤2000 字", ErrInvalidSource)
	}
	return nil
}

// Add 登记（enabled 缺省 true）。
func (m *Manager) Add(src Source) (View, error) {
	if err := src.Validate(); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	src.MonitorID = m.NewID(monitorIDPfx) // mon_<id>
	src.CreatedAt = m.Now().UTC().Format(time.RFC3339)
	if src.Args == nil {
		src.Args = []string{}
	}
	m.sources = append(m.sources, src)
	m.states[src.MonitorID] = &State{MonitorID: src.MonitorID}
	if err := m.saveLocked(); err != nil {
		return View{}, fmt.Errorf("monitor: save: %w", err)
	}
	return View{Source: src, State: m.states[src.MonitorID]}, nil
}

// Remove 删除源（运行态一并清理；已投递的历史在房间事件流里，不受影响）。
func (m *Manager) Remove(monitorID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.sources {
		if m.sources[i].MonitorID == monitorID {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			delete(m.states, monitorID)
			return m.saveLocked()
		}
	}
	return fmt.Errorf("monitor: %w: %s", ErrInvalidSource, "不存在 "+monitorID)
}

// SetEnabled 启停（停 = 调度跳过；水位保留——恢复后从上次观察续）。
func (m *Manager) SetEnabled(monitorID string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.sources {
		if m.sources[i].MonitorID == monitorID {
			m.sources[i].Enabled = enabled
			return m.saveLocked()
		}
	}
	return fmt.Errorf("monitor: %w: %s", ErrInvalidSource, "不存在 "+monitorID)
}

// Tick 调度刻：核对在途 run 终态 + 到期源检查。装配层周期调用（如 10s）。
func (m *Manager) Tick(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	for i := range m.sources {
		src := m.sources[i]
		st := m.states[src.MonitorID]
		if st == nil {
			continue
		}
		// 在途 run 终态核对（送达/失败水位推进——独立于检查到期）
		if st.InFlightRun != "" && m.Status != nil {
			switch m.Status(ctx, src.RoomID, st.InFlightRun) {
			case "completed":
				st.LastDelivered = st.LastProcessed
				st.DeliveredContent = st.ProcessingContent
				st.LastDeliveredAt = now.UTC().Format(time.RFC3339)
				st.InFlightRun = ""
				st.ProcessError = ""
				// 待处理水位：在途期间到达的新变化接力（不丢变化/不重复启动；
				// 差异基线 = 已送达快照）
				if st.PendingHash != "" && st.PendingHash != st.LastDelivered {
					m.launchLocked(ctx, src, st, st.PendingHash, st.PendingContent, st.DeliveredContent, now)
					st.PendingHash, st.PendingContent = "", ""
				}
			case "failed", "canceled", "unknown":
				st.ProcessError = "执行未送达（run " + m.Status(ctx, src.RoomID, st.InFlightRun) + "）"
				st.InFlightRun = "" // 重试交由检查刻：观察哈希 ≠ 送达哈希即重发
			default:
			}
		}
		if !src.Enabled {
			continue
		}
		due := st.LastCheckedAt == ""
		if !due {
			last, err := time.Parse(time.RFC3339, st.LastCheckedAt)
			due = err != nil || now.Sub(last) >= time.Duration(src.IntervalSec)*time.Second
		}
		if due {
			m.checkLocked(ctx, src, st, now)
		}
	}
	if err := m.saveLocked(); err != nil && m.Logger != nil {
		m.Logger.Warn("monitor 状态落盘失败", "err", err)
	}
}

// CheckNow 手动触发一次检查（忽略间隔；停用源不调度——Enabled 门一致生效）。
func (m *Manager) CheckNow(ctx context.Context, monitorID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.sources {
		if m.sources[i].MonitorID != monitorID {
			continue
		}
		if !m.sources[i].Enabled {
			return nil
		}
		st := m.states[monitorID]
		if st == nil {
			return fmt.Errorf("monitor: %w: 无运行态", ErrInvalidSource)
		}
		m.checkLocked(ctx, m.sources[i], st, m.Now())
		return m.saveLocked()
	}
	return fmt.Errorf("monitor: %w: %s", ErrInvalidSource, "不存在 "+monitorID)
}

// checkLocked 单源检查：取快照 → 哈希 → 去重/变化/重试分派。
func (m *Manager) checkLocked(ctx context.Context, src Source, st *State, now time.Time) {
	st.LastCheckedAt = now.UTC().Format(time.RFC3339)
	if m.Fetch == nil {
		return
	}
	content, err := m.Fetch(ctx, src)
	if err != nil {
		// 源失败：独立记录，不动水位（恢复后与上次观察比对——期间的变化不漏）
		st.LastError = truncateStr(err.Error(), 300)
		return
	}
	if len(content) > contentCap {
		content = content[:contentCap]
	}
	st.LastError = ""
	h := hashOf(content)
	observedBefore := st.LastObserved
	if h == st.LastObserved {
		if st.LastObserved != st.LastDelivered && st.InFlightRun == "" && st.LastObserved != "" {
			// 处理失败/未送达的重试（与去水分开：观察哈希仍领先于送达水位；差异基线 = 已送达快照）
			st.ProcessError = "重试中（上次未送达）"
			m.launchLocked(ctx, src, st, h, content, st.DeliveredContent, now)
		} else {
			st.NoChangeCount++ // 无变化：跳过模型调用（去重）
		}
		return
	}
	// 变化（或首次观察——从无到有视为变化，立即处理）；差异基线 = 更新前的观察快照
	old := st.LastContent
	st.LastObserved, st.LastContent = h, content
	if st.InFlightRun != "" {
		st.PendingHash, st.PendingContent = h, content // 待处理水位：在途 run 完成后接力
		return
	}
	m.launchLocked(ctx, src, st, h, content, old, now)
	_ = observedBefore
}

// launchLocked 组指令并发起 run（处理水位 = 交给 run 的哈希；oldContent = 差异基线）。
func (m *Manager) launchLocked(ctx context.Context, src Source, st *State, h, content, oldContent string, now time.Time) {
	if m.Launcher == nil {
		return
	}
	instr := m.instruction(src, oldContent, content)
	runID, err := m.Launcher.Launch(ctx, src.RoomID, src.Assignee, instr)
	if err != nil {
		st.ProcessError = "发起执行失败：" + truncateStr(err.Error(), 240)
		return
	}
	st.InFlightRun = runID
	st.LastProcessed = h
	st.ProcessingContent = content
	st.ProcessError = ""
	_ = now
}

// instruction 指令组装：模板缺省 + 差异注入（差异 = 旧/新快照的行级 diff）。
func (m *Manager) instruction(src Source, oldContent, newContent string) string {
	diff := DiffLines(oldContent, newContent)
	if n := len([]rune(diff)); n > diffCap {
		diff = string([]rune(diff)[:diffCap]) + "\n…（差异过长截断）"
	}
	tmpl := src.Instruction
	if tmpl == "" {
		tmpl = "监控源 {{monitor}} 的内容发生变化。请分析以下差异与最新快照，用不超过 200 字给出：变化要点、是否需要行动、建议。\n\n<差异>\n{{diff}}\n</差异>"
	}
	out := strings.ReplaceAll(tmpl, "{{monitor}}", src.MonitorID+" ("+src.Kind+")")
	out = strings.ReplaceAll(out, "{{diff}}", diff)
	if n := len([]rune(out)); n > instrCap {
		out = string([]rune(out)[:instrCap]) + "…"
	}
	return out
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
