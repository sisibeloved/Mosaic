// UT 层：外部视图模型——seq/tenant 不得泄露（RFC-0001 P0）。
package protocol

import (
	"encoding/json"
	"testing"
)

func TestEventViewHidesInternalFields(t *testing.T) {
	thread := "thr_1"
	env := Envelope{
		EventID:  "evt_1",
		TenantID: "ten_secret",
		RoomID:   "room_1",
		ThreadID: &thread,
		Seq:      148,
		Type:     EventMessagePosted,
		Payload:  []byte(`{"body":"hi"}`),
		Metadata: map[string]any{"internal": true},
	}
	raw, err := json.Marshal(ToEventView(env, "Y3Vyc29y"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, banned := range []string{"seq", "tenant_id", "metadata"} {
		if _, ok := doc[banned]; ok {
			t.Errorf("对外视图泄露内部字段 %q", banned)
		}
	}
	for _, required := range []string{"event_id", "position", "actor", "payload"} {
		if _, ok := doc[required]; !ok {
			t.Errorf("对外视图缺少 %q", required)
		}
	}
	if doc["position"] != "Y3Vyc29y" {
		t.Errorf("position = %v", doc["position"])
	}
}
