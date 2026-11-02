package room

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attach"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

type deliverAssetAdapter struct {
	source string
	data   []byte
}

func (a *deliverAssetAdapter) Name() string                     { return "deliver_asset_stub" }
func (a *deliverAssetAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (a *deliverAssetAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return &deliverAssetSession{adapter: a}, nil
}
func (a *deliverAssetAdapter) ReadDeliverAsset(_ context.Context, source string) (io.ReadCloser, error) {
	if source != a.source {
		return nil, fmt.Errorf("unexpected source")
	}
	return io.NopCloser(bytes.NewReader(a.data)), nil
}

type deliverAssetSession struct{ adapter *deliverAssetAdapter }

func (s *deliverAssetSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return deliverAssetHandle{task: task, adapter: s.adapter}, nil
}
func (s *deliverAssetSession) Cancel(string) {}
func (s *deliverAssetSession) Close()        {}

type deliverAssetHandle struct {
	task    agent.Task
	adapter *deliverAssetAdapter
}

func (deliverAssetHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (deliverAssetHandle) Cancel()                           {}
func (h deliverAssetHandle) Result() (agent.Result, error) {
	switch h.task.Kind {
	case agent.KindEvaluateIntent:
		action := "speak"
		recent, _ := h.task.Context.Inline["recent"].([]map[string]any)
		if len(recent) > 0 {
			if kind, _ := recent[len(recent)-1]["kind"].(string); kind != "human" {
				action = "silent"
			}
		}
		return agent.Result{Block: agent.BlockTurnIntent, Data: map[string]any{
			"action": action, "type": "extend", "public_rationale": "asset stub",
			"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		}}, nil
	case agent.KindGenerate:
		return agent.Result{Block: agent.BlockPublicDraft, Data: map[string]any{
			"body": "报告已完成。", "declared_relations": []any{},
			"deliver_assets": []agent.DeliverAsset{{Type: "file", Source: h.adapter.source, Caption: "5 轮提问与最终答案"}},
		}}, nil
	}
	return agent.Result{Block: "unsupported"}, nil
}

func TestChatAgentDeliverAssetBecomesManagedAttachment(t *testing.T) {
	store := NewMemStore()
	adapter := &deliverAssetAdapter{source: "/work/session-digest.md", data: []byte("digest\x00bytes")}
	sup := agent.NewSupervisor()
	if err := sup.RegisterFor("prof_asset", adapter); err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()
	attachments := &attach.Store{Root: t.TempDir()}
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, AgentAttachments: attachments,
		Seats:  []AgentSeat{{ParticipantID: "par_asset", Profile: agent.Profile{ProfileID: "prof_asset", Adapter: "deliver_asset_stub"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now, NewID: counterNewID(), Tenant: "ten_local",
	})
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_assets")
	deliverHuman(t, store, eng, "room_assets")
	waitRoundsClosed(t, store, "room_assets", 2)

	var payload struct {
		Body        string                 `json:"body"`
		Attachments []attach.Descriptor    `json:"attachments"`
		Extra       map[string]interface{} `json:"deliver_assets"`
	}
	found := false
	for _, ev := range store.RoomEvents("room_assets") {
		if ev.Type != protocol.EventMessagePosted || ev.Actor.Kind != "agent" {
			continue
		}
		found = true
		if strings.Contains(string(ev.Payload), "deliver_assets") || strings.Contains(string(ev.Payload), "deliver-assets") {
			t.Fatalf("内部交付协议不得进入消息事件：%s", ev.Payload)
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if !found || payload.Body != "报告已完成。" || len(payload.Attachments) != 1 {
		t.Fatalf("消息或附件缺失：%+v", payload)
	}
	descriptor := payload.Attachments[0]
	if descriptor.Name != "session-digest.md" || descriptor.Caption != "5 轮提问与最终答案" || descriptor.SizeBytes != int64(len(adapter.data)) {
		t.Fatalf("附件描述子不符：%+v", descriptor)
	}
	raw, _, err := attachments.Open("room_assets", descriptor.AttachmentID)
	if err != nil || !bytes.Equal(raw, adapter.data) {
		t.Fatalf("附件下载字节不符：%v %v", raw, err)
	}
	events := store.RoomEvents("room_assets")
	history := make([]StoredEvent, len(events))
	for i := range events {
		history[i].Envelope = events[i]
	}
	snapshot := ProjectSnapshot("room_assets", history)
	if len(snapshot.Timeline) != 2 || len(snapshot.Timeline[1].Attachments) != 1 || snapshot.Timeline[1].Attachments[0].Caption != descriptor.Caption {
		t.Fatalf("快照附件投影不符：%+v", snapshot.Timeline)
	}
}

func TestMaterializeDeliverAssetFailureKeepsHonestBody(t *testing.T) {
	adapter := &deliverAssetAdapter{source: "/work/expected.md", data: []byte("ok")}
	sup := agent.NewSupervisor()
	if err := sup.RegisterFor("prof_asset", adapter); err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Agents: sup, AgentAttachments: &attach.Store{Root: t.TempDir()},
		Seats: []AgentSeat{{ParticipantID: "par_asset", Profile: agent.Profile{ProfileID: "prof_asset", Adapter: "deliver_asset_stub"}}},
	})
	draft := agent.Result{Block: agent.BlockPublicDraft, Data: map[string]any{
		"body": "报告已完成。", "deliver_assets": []agent.DeliverAsset{{Type: "file", Source: "/etc/passwd"}},
	}}
	eng.materializeDeliverAssets(context.Background(), "room_assets", "par_asset", &draft)
	body, _ := draft.Data["body"].(string)
	if !strings.Contains(body, "报告已完成。") || !strings.Contains(body, "未能导入") {
		t.Fatalf("失败应保留正文并明确标记：%q", body)
	}
	if _, exists := draft.Data["deliver_assets"]; exists {
		t.Fatal("内部侧车不得残留")
	}
	if _, exists := draft.Data["attachments"]; exists {
		t.Fatal("读取失败不得伪装附件已导入")
	}
}
