// fixtures 版本门禁（与 codex/kimi 适配器同制）：manifest 钉 CLI 版本与文件哈希——
// 任何 fixture 改动/重捕获未同步 manifest 即红。
package minimax

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureManifestPinsVersionAndHashes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("fixtures", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest 缺失（重捕获后必须更新）: %v", err)
	}
	var m struct {
		CLIVersion string            `json:"cli_version"`
		Files      map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest 非法: %v", err)
	}
	if m.CLIVersion == "" || len(m.Files) == 0 {
		t.Fatalf("manifest 必须含 cli_version 与 files：%+v", m)
	}
	entries, err := os.ReadDir(filepath.Join("fixtures"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if name == "manifest.json" || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".jsonl") {
			t.Fatalf("fixtures 只收 .jsonl 捕获（%s）", name)
		}
		want, ok := m.Files[name]
		if !ok {
			t.Fatalf("fixture %s 未登记 manifest（重捕获后必须更新哈希）", name)
		}
		data, err := os.ReadFile(filepath.Join("fixtures", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("fixture %s 哈希漂移（got %s want %s）——重捕获必须更新 manifest", name, got, want)
		}
		seen[name] = true
	}
	for name := range m.Files {
		if !seen[name] {
			t.Fatalf("manifest 登记 %s 但文件缺失", name)
		}
	}
}
