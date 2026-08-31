// fixtures 版本门禁（M1 收口补课）：manifest 钉 CLI 版本与文件哈希——
// “钉版本”从注释级升级为机器可检：任何 fixture 改动/重捕获未同步 manifest 即红。
package codex

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
	entries, err := os.ReadDir("fixtures")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		seen[name] = true
		want, ok := m.Files[name]
		if !ok {
			t.Errorf("fixture %s 未登记于 manifest", name)
			continue
		}
		data, err := os.ReadFile(filepath.Join("fixtures", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		got := "sha256:" + hex.EncodeToString(sum[:])
		if got != want {
			t.Errorf("fixture %s 哈希漂移（重捕获须更新 manifest）：\n got %s\nwant %s", name, got, want)
		}
	}
	for name := range m.Files {
		if !seen[name] {
			t.Errorf("manifest 登记 %s 但文件不存在", name)
		}
	}
}
