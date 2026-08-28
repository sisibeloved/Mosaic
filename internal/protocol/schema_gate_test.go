// 严格 JSON Schema 校验门禁（ADR-0007 生成链落地件）。
// 规则：valid/ 全量必须通过（信封 + 命中事件族时叠加 payload 校验）；
// invalid/ 反例必须被拒绝；每个事件 Schema 必须被至少一个 valid fixture 覆盖。
package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var protoRoot = filepath.Join("..", "..", "api", "room-protocol")

type schemaSet struct {
	envelope *jsonschema.Schema
	command  *jsonschema.Schema
	events   map[string]*jsonschema.Schema // 事件 type → payload Schema
}

func compileSchemas(t *testing.T) schemaSet {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat() // date-time 等格式断言为硬校验
	set := schemaSet{events: map[string]*jsonschema.Schema{}}
	var err error
	if set.envelope, err = c.Compile(filepath.Join(protoRoot, "envelope.schema.json")); err != nil {
		t.Fatalf("compile envelope schema: %v", err)
	}
	if set.command, err = c.Compile(filepath.Join(protoRoot, "command.schema.json")); err != nil {
		t.Fatalf("compile command schema: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(protoRoot, "events"))
	if err != nil {
		t.Fatalf("read events dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".schema.json") {
			continue
		}
		sch, err := c.Compile(filepath.Join(protoRoot, "events", name))
		if err != nil {
			t.Fatalf("compile event schema %s: %v", name, err)
		}
		set.events[strings.TrimSuffix(name, ".schema.json")] = sch
	}
	if len(set.events) == 0 {
		t.Fatal("events/ 下没有任何事件 payload Schema")
	}
	return set
}

func loadFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: invalid JSON: %v", path, err)
	}
	return doc
}

// validateDoc 校验单个文档；返回 nil/错误。envelope 命中事件族时叠加 payload 校验。
func validateDoc(set schemaSet, name string, doc map[string]any) error {
	switch {
	case strings.HasPrefix(name, "envelope"):
		if err := set.envelope.Validate(doc); err != nil {
			return fmt.Errorf("envelope: %w", err)
		}
		eventType, _ := doc["type"].(string)
		if sch, ok := set.events[eventType]; ok {
			if err := sch.Validate(doc["payload"]); err != nil {
				return fmt.Errorf("payload(%s): %w", eventType, err)
			}
		}
		return nil
	case strings.HasPrefix(name, "command"):
		return set.command.Validate(doc)
	default:
		return fmt.Errorf("fixture 命名必须以 envelope 或 command 开头")
	}
}

func TestStrictSchemaValidationGate(t *testing.T) {
	set := compileSchemas(t)

	// 反例覆盖面登记：删除/改名任何一个都必须同步更新本门禁。
	requiredInvalid := []string{
		"envelope-missing-actor.json",               // 信封必填字段缺失
		"envelope-seq-zero.json",                    // seq ≥ 1
		"envelope-event-id-bad-prefix.json",         // event_id 前缀 pattern
		"envelope-round-opened-bad-mode.json",       // payload 枚举（mode）
		"envelope-intent-recorded-bad-band.json",    // payload 枚举（score_band，反 Goodhart）
		"envelope-floor-revoked-bad-reason.json",    // payload 枚举（reason）
		"envelope-floor-granted-unknown-field.json", // additionalProperties: false（严格写）
		"command-bad-idempotency-key.json",          // 幂等键 UUIDv7 pattern
		"command-missing-payload.json",              // 命令必填字段
	}

	validDir := filepath.Join(protoRoot, "fixtures", "valid")
	entries, err := os.ReadDir(validDir)
	if err != nil {
		t.Fatalf("read valid fixtures: %v", err)
	}
	coveredEvents := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		doc := loadFixture(t, filepath.Join(validDir, name))
		if err := validateDoc(set, name, doc); err != nil {
			t.Errorf("valid/%s 必须通过校验: %v", name, err)
		}
		if eventType, _ := doc["type"].(string); eventType != "" {
			if _, ok := set.events[eventType]; ok {
				coveredEvents[eventType] = true
			}
		}
	}
	for eventType := range set.events {
		if !coveredEvents[eventType] {
			t.Errorf("事件 %s 的 payload Schema 缺少 valid fixture 覆盖", eventType)
		}
	}

	invalidDir := filepath.Join(protoRoot, "fixtures", "invalid")
	invalidEntries, err := os.ReadDir(invalidDir)
	if err != nil {
		t.Fatalf("read invalid fixtures: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range invalidEntries {
		name := e.Name()
		doc := loadFixture(t, filepath.Join(invalidDir, name))
		if err := validateDoc(set, name, doc); err == nil {
			t.Errorf("invalid/%s 必须被拒绝，却通过了校验", name)
		}
		seen[name] = true
	}
	for _, name := range requiredInvalid {
		if !seen[name] {
			t.Errorf("invalid/ 缺少登记的反例 %s", name)
		}
	}
}
