// 结构化输出 fixture 门禁（RFC-0002 §3.5.1 三件套之三：五类块正反用例）——
// valid 全过 / invalid 全拒；fixture 与 ValidateBlock 同演进，新增块类型先补 fixture。
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

type fixtureDoc struct {
	Block string         `json:"block"`
	Data  map[string]any `json:"data"`
}

func loadFixture(t *testing.T, path string) fixtureDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var doc fixtureDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture %s 不是合法 JSON（fixture 自身须合法）: %v", path, err)
	}
	return doc
}

func TestBlockFixturesGate(t *testing.T) {
	valid, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil || len(valid) == 0 {
		t.Fatalf("valid fixtures 缺失: %v", err)
	}
	// 五类块各有至少一个 valid fixture（覆盖完整性硬门）
	seenBlocks := map[string]bool{}
	for _, f := range valid {
		doc := loadFixture(t, f)
		if err := agent.ValidateBlock(doc.Block, doc.Data); err != nil {
			t.Errorf("valid fixture %s 被拒: %v", f, err)
		}
		seenBlocks[doc.Block] = true
	}
	for _, block := range []string{
		agent.BlockTurnIntent, agent.BlockAttentionAssessment, agent.BlockPublicDraft,
		agent.BlockGroundedSummary, agent.BlockClosureIntent,
	} {
		if !seenBlocks[block] {
			t.Errorf("valid fixtures 缺块类型 %q 的覆盖", block)
		}
	}

	invalid, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil || len(invalid) == 0 {
		t.Fatalf("invalid fixtures 缺失: %v", err)
	}
	for _, f := range invalid {
		doc := loadFixture(t, f)
		if err := agent.ValidateBlock(doc.Block, doc.Data); err == nil {
			t.Errorf("invalid fixture %s 被错误接受（校验器须拒绝）", f)
		}
	}
}
