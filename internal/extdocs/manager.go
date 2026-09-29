// per-provider 凭据持久化（RFC-0016）：DataDir/extdocs/<provider>.json（0600
// 原子写）；provider 自持凭据结构体，经 Load/Save 泛型读写。旧 txdocs-token
// 单文件启动一次性迁移并入 tencent_docs.json（迁移后删除旧文件——幂等）。
package extdocs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Manager 凭据文件集（内存态无缓存——每次 Load 读盘：本地小文件，换来与
// 手改/多进程的一致性）。
type Manager struct {
	mu  sync.Mutex
	dir string
}

// LoadManager 装载（目录惰性创建；含 txdocs-token 单文件迁移）。
func LoadManager(dir string) (*Manager, error) {
	m := &Manager{dir: dir}
	// RFC-0015 → RFC-0016 迁移：旧 txdocs-token（裸 token 文本）并入腾讯凭据文件
	legacy := filepath.Join(filepath.Dir(dir), "txdocs-token")
	if raw, err := os.ReadFile(legacy); err == nil && len(raw) > 0 {
		token := string(trimSpaceBytes(raw))
		var oldState struct {
			LastVerifiedAt string `json:"last_verified_at,omitempty"`
		}
		_ = json.Unmarshal(readFileOrEmpty(legacy+".state.json"), &oldState)
		var tc tencentCreds
		_ = m.Load(ProviderTencent, &tc) // 已有新文件则尊重，不覆盖
		if tc.Token == "" {
			tc.Token = token
			tc.LastVerifiedAt = oldState.LastVerifiedAt
			if err := m.Save(ProviderTencent, tc); err != nil {
				return nil, fmt.Errorf("extdocs: 迁移腾讯凭据: %w", err)
			}
		}
		_ = os.Remove(legacy)
		_ = os.Remove(legacy + ".state.json")
	}
	return m, nil
}

// Load 读 provider 凭据（文件不存在 = 零值，非错误）。
func (m *Manager) Load(provider string, v any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(m.dir, provider+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("extdocs: read %s creds: %w", provider, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("extdocs: parse %s creds: %w", provider, err)
	}
	return nil
}

// Save 写 provider 凭据（0600 原子替换）。
func (m *Manager) Save(provider string, v any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("extdocs: creds dir: %w", err)
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("extdocs: marshal %s creds: %w", provider, err)
	}
	path := filepath.Join(m.dir, provider+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("extdocs: write %s creds: %w", provider, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("extdocs: swap %s creds: %w", provider, err)
	}
	return nil
}

// Clear 删除 provider 凭据文件（不存在幂等）。
func (m *Manager) Clear(provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.Remove(filepath.Join(m.dir, provider+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("extdocs: remove %s creds: %w", provider, err)
	}
	return nil
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func readFileOrEmpty(path string) []byte {
	raw, _ := os.ReadFile(path)
	return raw
}
