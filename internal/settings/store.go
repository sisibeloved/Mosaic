// Package settings：OQ-B 设置族（RFC-0012 开放问题）——引擎可调常量的
// 持久化用户设置。首员 run_timeout_seconds（M4-1 任务执行时长上限，缺省
// 600s）；ResponseCap/对话环阈值/反应窗时长同族，随设置面演进逐个迁入。
//
// 形态取舍：本地单用户、设置面低频——数据目录内单个 JSON 文件（0600、
// 临时文件 + rename 原子替换）足够，不引入额外存储。读取失败 fail safe 回
// 缺省值（不阻断启动）；字段缺失视为未设置（缺省生效），与"未来版本新增
// 字段"的前向兼容语义一致。
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 界限：run_timeout 下限须长于零（防误配即刻超时）；上限防失控长挂。
// 缺省 10min 与引擎 defaultRunTimeout 一致（RFC-0002 M4-1 补编）。
const (
	DefaultRunTimeoutSeconds = 600
	MinRunTimeoutSeconds     = 60
	MaxRunTimeoutSeconds     = 7200
)

// Document 设置文档（全量小文档：读写都整份走，无局部补丁语义）。
type Document struct {
	RunTimeoutSeconds int    `json:"run_timeout_seconds"`
	ReplyOrPassMode   string `json:"reply_or_pass_mode,omitempty"` // auto（缺省）| off（M4-3 A/B 控制组）
}

// WithDefaults 未设置字段（零值）回缺省。
func (d Document) WithDefaults() Document {
	if d.RunTimeoutSeconds <= 0 {
		d.RunTimeoutSeconds = DefaultRunTimeoutSeconds
	}
	if d.ReplyOrPassMode != ReplyOrPassOff {
		d.ReplyOrPassMode = ReplyOrPassAuto
	}
	return d
}

// ReplyOrPassMode 取值。
const (
	ReplyOrPassAuto = "auto"
	ReplyOrPassOff  = "off"
)

// ReplyOrPassEnabled ROP 门（引擎活读面；off = 资格座位回退两阶段）。
func (s *Store) ReplyOrPassEnabled() bool {
	return s.Snapshot().ReplyOrPassMode != ReplyOrPassOff
}

// ValidateRange 值域校验（写入前；读取路径不校验——损坏文件 fail safe 回缺省）。
func ValidateRunTimeoutSeconds(v int) error {
	if v < MinRunTimeoutSeconds || v > MaxRunTimeoutSeconds {
		return fmt.Errorf("run_timeout_seconds 须在 %d..%d 秒", MinRunTimeoutSeconds, MaxRunTimeoutSeconds)
	}
	return nil
}

// Store 设置存储：启动加载一次，此后内存态服务读；写路径互斥串行 + 落盘。
type Store struct {
	path string
	mu   sync.Mutex
	doc  Document
}

// Default 缺省内存存储（不落盘）——装配层 fail safe 用（文件损坏时回缺省继续）。
func Default() *Store {
	return &Store{doc: Document{}.WithDefaults()}
}

// Open 加载或初始化设置文件。文件不存在 = 全缺省（首次启动，惰性创建——
// 首次保存才落盘）；损坏/不可读 = 返回错误由调用方决定（装配层 fail safe
// 回缺省继续启动，诊断面可见）。
func Open(path string) (*Store, error) {
	s := &Store{path: path, doc: Document{}.WithDefaults()}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("settings: parse: %w", err)
	}
	s.doc = doc.WithDefaults()
	return s, nil
}

// Snapshot 当前设置（含缺省回填）。
func (s *Store) Snapshot() Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc
}

// RunTimeout 任务执行时长上限（引擎活读面——M4-1：设置变更无须重启）。
func (s *Store) RunTimeout() time.Duration {
	return time.Duration(s.Snapshot().RunTimeoutSeconds) * time.Second
}

// UpdateRunTimeoutSeconds 校验并持久化（原子替换写）。
func (s *Store) UpdateRunTimeoutSeconds(v int) error {
	if err := ValidateRunTimeoutSeconds(v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.doc
	doc.RunTimeoutSeconds = v
	if err := writeAtomic(s.path, doc); err != nil {
		return err
	}
	s.doc = doc
	return nil
}

// ValidateReplyOrPassMode 值域校验。
func ValidateReplyOrPassMode(v string) error {
	if v != ReplyOrPassAuto && v != ReplyOrPassOff {
		return fmt.Errorf("reply_or_pass_mode 须为 auto|off")
	}
	return nil
}

// UpdateReplyOrPassMode 校验并持久化。
func (s *Store) UpdateReplyOrPassMode(v string) error {
	if err := ValidateReplyOrPassMode(v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.doc
	doc.ReplyOrPassMode = v
	if err := writeAtomic(s.path, doc); err != nil {
		return err
	}
	s.doc = doc
	return nil
}

// writeAtomic 临时文件 + rename（同目录保证原子性；0600 与数据目录纪律一致）。
func writeAtomic(path string, doc Document) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return fmt.Errorf("settings: temp: %w", err)
	}
	defer os.Remove(tmp.Name()) // rename 成功后是 no-op
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: write: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("settings: rename: %w", err)
	}
	return nil
}
