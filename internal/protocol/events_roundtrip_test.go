// round-trip 兼容性门禁（ADR-0007）：手写版本化 struct 与权威 Schema 必须逐键一致。
// fixture(JSON) → Envelope+typed payload → 重新序列化 → 与原文档深度相等。
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnvelopeRoundTripAgainstFixtures(t *testing.T) {
	validDir := filepath.Join("..", "..", "api", "room-protocol", "fixtures", "valid")
	entries, err := os.ReadDir(validDir)
	if err != nil {
		t.Fatalf("read valid fixtures: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" || name[:8] != "envelope" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(validDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Errorf("%s: unmarshal Envelope: %v", name, err)
			continue
		}
		if typed := env.DecodePayload(); typed != nil {
			// 用类型化 payload 原地替换 RawMessage，验证 struct 覆盖了 Schema 全部键
			encoded, err := json.Marshal(typed)
			if err != nil {
				t.Fatalf("%s: marshal typed payload: %v", name, err)
			}
			env.Payload = encoded
		}
		out, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("%s: marshal Envelope: %v", name, err)
		}
		var want, got any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("%s: redecode raw: %v", name, err)
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: redecode out: %v", name, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: round-trip 不一致\nwant: %s\ngot:  %s", name, raw, out)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没有任何 envelope fixture 参与 round-trip 校验")
	}
}
