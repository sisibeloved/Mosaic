// 语境平面全景（v1.70 展示对齐，负责人原则："模型看得到的，开发者也能看得到"）：
// 记忆 tab 不再只挂胶囊投影——与引擎组装同源的纯函数快照：近窗原文、按需召回
// 命中（含关键词）、恒常平面（策展条目 + 胶囊 + 容量水位）、tasklist、检索结果
// 提示。零收束也有内容；与注入面共用同一投影函数（不双轨）。
package room

import (
	"encoding/json"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ContextPanorama 全景快照（GET /v1/rooms/{id}/context 响应体）。
type ContextPanorama struct {
	RoomID     string               `json:"room_id"`
	Watermark  string               `json:"watermark"`
	NearWindow []PanoramaMessage    `json:"near_window"`
	Retrieved  []contextx.RetrievedItem `json:"retrieved"`
	Keywords   []string             `json:"retrieval_keywords"`
	Curated    []CuratedEntry       `json:"curated_memory"`
	Capsules   []MemoryCapsuleView  `json:"capsules"`
	Budget     CuratedBudgetStat    `json:"budget"`
	Tasklist   []TaskBriefView      `json:"tasklist"`
}

// PanoramaMessage 近窗消息（原文；actor/kind/时间供人类阅读）。
type PanoramaMessage struct {
	EventID    string `json:"event_id"`
	Actor      string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	Body       string `json:"body"`
	OccurredAt string `json:"occurred_at"`
}

// TaskBriefView tasklist 的人类阅读投影（含 owner/requester/波龄）。
type TaskBriefView struct {
	TaskBrief
}

// ContextPanoramaOf 事件流 → 最新波语境的全景快照（纯函数；锚点取最新消息，
// 与生成组装同口径——RecentWindow 10、召回 top5、恒常平面合并视图）。
func ContextPanoramaOf(roomID string, events []StoredEvent) ContextPanorama {
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i].Envelope
	}
	p := ContextPanorama{RoomID: roomID, NearWindow: []PanoramaMessage{}, Retrieved: []contextx.RetrievedItem{},
		Keywords: []string{}, Curated: []CuratedEntry{}, Capsules: []MemoryCapsuleView{}, Tasklist: []TaskBriefView{}}
	if len(events) == 0 {
		return p
	}
	// 近窗：最新 10 条 message.posted（最新在前）
	for i := len(events) - 1; i >= 0 && len(p.NearWindow) < 10; i-- {
		env := events[i].Envelope
		if env.Type != protocol.EventMessagePosted {
			continue
		}
		var mp struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(env.Payload, &mp) != nil {
			continue
		}
		p.NearWindow = append(p.NearWindow, PanoramaMessage{
			EventID: env.EventID, Actor: env.Actor.ParticipantID, ActorKind: env.Actor.Kind,
			Body: mp.Body, OccurredAt: env.OccurredAt,
		})
	}
	// 锚点 = 最新消息；按需召回与关键词（与 retrievedProjection 同口径）
	var anchor protocol.Envelope
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Envelope.Type == protocol.EventMessagePosted {
			anchor = events[i].Envelope
			break
		}
	}
	if anchor.EventID != "" {
		var ap struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(anchor.Payload, &ap)
		p.Keywords = ExtractKeywords(ap.Body)
		p.Retrieved = retrievedProjection(envs, anchor, 10)
		if p.Retrieved == nil {
			p.Retrieved = []contextx.RetrievedItem{}
		}
	}
	plane := ConstantPlaneOf(envs)
	p.Curated = plane.Entries
	if p.Curated == nil {
		p.Curated = []CuratedEntry{}
	}
	p.Capsules = MemoryCapsulesOf(events)
	p.Budget = plane.Stat
	for _, b := range PendingTaskBriefsOf(envs) {
		p.Tasklist = append(p.Tasklist, TaskBriefView{TaskBrief: b})
	}
	p.Watermark = events[len(events)-1].Envelope.EventID
	return p
}
