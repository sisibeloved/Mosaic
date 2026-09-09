// UT 层：v1.70 记忆/展示对齐端点——/memory 策展扩展、/context 平面全景、
// /receipts 回执流水（未装配 ReceiptLister → 404 不暴露面）。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

type fakeReceiptLister struct {
	receipts []contextx.Receipt
}

func (f fakeReceiptLister) ReceiptsOf(_ context.Context, _ string, limit int) ([]contextx.Receipt, error) {
	if limit > len(f.receipts) {
		limit = len(f.receipts)
	}
	return f.receipts[:limit], nil
}

func seedRoomWithMessage(t *testing.T, svc *room.Service) string {
	t.Helper()
	res, err := svc.ExecuteCommand(context.Background(), room.Actor{ParticipantID: "par_owner", Kind: "human"}, room.Command{
		CommandKind: "create_room", IdempotencyKey: "018f0000-0000-7000-8000-000000000001",
		IssuedAt: "2026-09-08T00:00:00.000Z",
		Payload:  json.RawMessage(`{"display_name":"记忆测试房","agents":[]}`),
	})
	if err != nil {
		t.Fatalf("create_room: %v", err)
	}
	_, err = svc.ExecuteCommand(context.Background(), room.Actor{ParticipantID: "par_owner", Kind: "human"}, room.Command{
		CommandKind: "post_message", IdempotencyKey: "018f0000-0000-7000-8000-000000000002",
		IssuedAt:            "2026-09-08T00:00:01.000Z",
		RoomID:              res.RoomID,
		ExpectedRoomVersion: res.RoomVersion,
		Payload:             json.RawMessage(`{"body":"本地存储选型讨论"}`),
	})
	if err != nil {
		t.Fatalf("post_message: %v", err)
	}
	return res.RoomID
}

func TestMemoryEndpointsPanoramaAndReceipts(t *testing.T) {
	store := room.NewMemStore()
	var idMu sync.Mutex
	var idN int64
	svc := room.NewService(room.Config{Store: store, Lister: store,
		Clock: func() string { return "2026-09-08T00:00:00.000Z" },
		NewID: func(p string) string {
			idMu.Lock()
			defer idMu.Unlock()
			idN++
			return p + "_mem_" + strconv.FormatInt(idN, 10)
		},
		Tenant: "ten_local"})
	ts := httptest.NewServer(New(Deps{
		SVC:    svc,
		Reader: store,
		Hub:    sse.NewHub(),
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"},
		ReceiptLister: fakeReceiptLister{receipts: []contextx.Receipt{{
			ReceiptID: "rcpt_test1", RoomID: "room_x", TaskID: "rnd_t:eval", Watermark: 7, LayerDigests: []string{"d1", "d2"}, CreatedAt: "2026-09-08T00:00:00.000Z",
		}}},
	}))
	t.Cleanup(ts.Close)
	roomID := seedRoomWithMessage(t, svc)

	// /memory：策展字段齐全（空数组非 null——前端契约）
	resp, err := http.Get(ts.URL + "/v1/rooms/" + roomID + "/memory")
	if err != nil {
		t.Fatalf("get memory: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("memory status = %d", resp.StatusCode)
	}
	var mem struct {
		Curated       []map[string]any `json:"curated"`
		CuratedBudget map[string]any   `json:"curated_budget"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mem); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	if mem.Curated == nil || mem.CuratedBudget == nil {
		t.Fatalf("策展字段应非 null: %+v", mem)
	}

	// /context：全景（近窗含刚发的消息；关键词非 null）
	resp2, err := http.Get(ts.URL + "/v1/rooms/" + roomID + "/context")
	if err != nil {
		t.Fatalf("get context: %v", err)
	}
	defer resp2.Body.Close()
	var pano struct {
		NearWindow []map[string]any `json:"near_window"`
		Keywords   []string         `json:"retrieval_keywords"`
		Budget     map[string]any   `json:"budget"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&pano); err != nil {
		t.Fatalf("decode context: %v", err)
	}
	if len(pano.NearWindow) != 1 || pano.NearWindow[0]["body"] != "本地存储选型讨论" {
		t.Fatalf("近窗投影: %+v", pano.NearWindow)
	}
	if pano.Keywords == nil || pano.Budget == nil {
		t.Fatalf("关键词/预算应非 null: %+v", pano)
	}

	// /receipts：装配的 Lister 返回流水
	resp3, err := http.Get(ts.URL + "/v1/rooms/" + roomID + "/receipts")
	if err != nil {
		t.Fatalf("get receipts: %v", err)
	}
	defer resp3.Body.Close()
	var rec struct {
		Receipts []map[string]any `json:"receipts"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&rec); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if len(rec.Receipts) != 1 || rec.Receipts[0]["task_id"] != "rnd_t:eval" {
		t.Fatalf("回执流水: %+v", rec.Receipts)
	}
}

func TestReceiptsUnavailableWithoutLister(t *testing.T) {
	ts, _, _ := newTestServer(t) // 未注入 ReceiptLister
	resp, err := http.Get(ts.URL + "/v1/rooms/room_x/receipts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未装配应 404，got %d", resp.StatusCode)
	}
}
