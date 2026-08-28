// Package protocol 守护 api/room-protocol 的 Schema 与 fixture 一致性。
// 本文件为结构冒烟（命名/必填/UUIDv7）；严格校验门禁见 schema_gate_test.go（ADR-0007）。
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	eventTypePattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)
	uuidv7Pattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	envelopeRequired = []string{
		"event_id", "tenant_id", "room_id", "seq", "type", "schema_version",
		"occurred_at", "actor", "visibility", "payload",
	}
	commandRequired = []string{
		"command_kind", "expected_room_version", "idempotency_key", "issued_at", "payload",
	}
)

func TestValidFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "api", "room-protocol", "fixtures", "valid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, entry := range entries {
		name := entry.Name()
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: invalid JSON: %v", name, err)
			continue
		}
		switch {
		case len(name) >= 8 && name[:8] == "envelope":
			checkRequired(t, name, doc, envelopeRequired)
			if s, ok := doc["type"].(string); !ok || !eventTypePattern.MatchString(s) {
				t.Errorf("%s: type %q 不符合多段点分 lower_snake_case", name, doc["type"])
			}
			if seq, ok := doc["seq"].(float64); !ok || seq < 1 {
				t.Errorf("%s: seq 必须为 ≥1 的整数", name)
			}
		case len(name) >= 7 && name[:7] == "command":
			checkRequired(t, name, doc, commandRequired)
			if k, ok := doc["idempotency_key"].(string); !ok || !uuidv7Pattern.MatchString(k) {
				t.Errorf("%s: idempotency_key %q 不是 UUIDv7", name, doc["idempotency_key"])
			}
		default:
			t.Errorf("%s: fixture 命名必须以 envelope 或 command 开头", name)
		}
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	dir := filepath.Join("..", "..", "api", "room-protocol")
	for _, name := range []string{"envelope.schema.json", "command.schema.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: invalid JSON: %v", name, err)
			continue
		}
		if s, _ := doc["$schema"].(string); s == "" {
			t.Errorf("%s: 缺少 $schema 声明", name)
		}
	}
}

func checkRequired(t *testing.T, name string, doc map[string]any, required []string) {
	t.Helper()
	for _, key := range required {
		if _, ok := doc[key]; !ok {
			t.Errorf("%s: 缺少必填字段 %q", name, key)
		}
	}
}
