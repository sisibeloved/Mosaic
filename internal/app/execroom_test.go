package app

// ADR-0025 / 0.6.2：execRoomEvents 适配——跨页累积映射保序、可见性原样
// 透传（授权过滤在 execctx 域内执行）、body 只从 message.posted 提取。
import (
	"context"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func TestExecRoomEventsCrossPage(t *testing.T) {
	e1 := envelope("evt_1", protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		map[string]any{"body": "第一页消息"})
	e1.Envelope.Seq = 1
	e1.Envelope.Visibility = protocol.Visibility{Kind: "participants", Participants: []string{"par_owner"}}
	e2 := envelope("evt_2", protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
		map[string]any{"body": "第二页消息"})
	e2.Envelope.Seq = 2
	adapter := execRoomEvents{reader: pagedReader{events: []room.StoredEvent{e1, e2}}}

	got, err := adapter.RoomEvents(context.Background(), "rm_cross")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("跨页累积应 2 条，得到 %d", len(got))
	}
	if got[0].EventID != "evt_1" || got[0].Seq != 1 || got[0].Visibility != "participants" {
		t.Fatalf("前页映射错误: %+v", got[0])
	}
	if got[0].Body != "第一页消息" || got[1].Body != "第二页消息" {
		t.Fatalf("body 提取错误: %+v", got)
	}
	if got[1].Visibility != "public" || got[1].ActorKind != "agent" {
		t.Fatalf("后页映射错误: %+v", got[1])
	}
}
