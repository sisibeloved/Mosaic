// UT 层：附件面端点——multipart 上传（写门/大小上限/文件名净化）、下载
// （404 面）、post_message 令牌定稿（token 进、描述子出、单次消费）。
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/attach"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// newAttachTestServer 装配附件面（svc 命令面可发消息）。token 可选。
func newAttachTestServer(t *testing.T, ownerToken string) (*httptest.Server, *attach.Store) {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	att := &attach.Store{Root: t.TempDir()}
	svc := room.NewService(room.Config{
		Store:  store,
		Lister: store,
		Clock:  func() string { return "2026-09-07T00:00:00.000Z" },
		NewID: func(p string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s_att_%08d", p, n)
		},
		Tenant:      "ten_local",
		Attachments: att,
	})
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken:  ownerToken,
		Attachments: att,
	}))
	t.Cleanup(ts.Close)
	return ts, att
}

// postJSONToken 携 owner token 的命令 POST（本文件的服务器启用 token 门）。
func postJSONToken(t *testing.T, url, token string, body any) (*http.Response, apigen.CommandResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out apigen.CommandResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func uploadMultipart(t *testing.T, url, token, filename, contentType string, content []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAttachmentUploadLifecycle(t *testing.T) {
	ts, _ := newAttachTestServer(t, "tok1")

	// 无 token → 401（写门）
	resp := uploadMultipart(t, ts.URL+"/v1/rooms/r1/attachments", "", "a.txt", "text/plain", []byte("hi"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 上传应 401，got %d", resp.StatusCode)
	}

	// 正常上传
	resp = uploadMultipart(t, ts.URL+"/v1/rooms/r1/attachments", "tok1", "../../secret.txt", "text/plain", []byte("file-body"))
	var meta attach.UploadMeta
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	if resp.StatusCode != http.StatusOK || meta.Token == "" || meta.Name != "secret.txt" {
		t.Fatalf("上传应成功且文件名净化: %d %+v", resp.StatusCode, meta)
	}

	// post_message 携带令牌 → 描述子定稿（经命令面）
	createdResp, createdOut := postJSONToken(t, ts.URL+"/v1/rooms", "tok1", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5c01", "issued_at": "2026-09-07T00:00:01.000Z",
		"payload": map[string]any{"display_name": "附件房"},
	})
	if createdResp.StatusCode != http.StatusOK || createdOut.RoomId == "" {
		t.Fatalf("建房失败: %d", createdResp.StatusCode)
	}
	roomID := createdOut.RoomId
	up := uploadMultipart(t, ts.URL+"/v1/rooms/"+roomID+"/attachments", "tok1", "note.md", "text/markdown", []byte("# 标题\n正文"))
	var roomMeta attach.UploadMeta
	_ = json.NewDecoder(up.Body).Decode(&roomMeta)
	if up.StatusCode != http.StatusOK {
		t.Fatalf("房内上传失败: %d", up.StatusCode)
	}
	cmdResp, cmdOut := postJSONToken(t, ts.URL+"/v1/rooms/"+roomID+"/commands", "tok1", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5c02", "issued_at": "2026-09-07T00:00:02.000Z",
		"payload": map[string]any{"body": "看这个文件", "attachments": []string{roomMeta.Token}},
	})
	if cmdResp.StatusCode != http.StatusOK || cmdOut.EventId == "" {
		t.Fatalf("带附件消息应发布成功: %d %+v", cmdResp.StatusCode, cmdOut)
	}
	// 令牌单次消费：再用同令牌 → 拒
	reuseResp, _ := postJSONToken(t, ts.URL+"/v1/rooms/"+roomID+"/commands", "tok1", map[string]any{
		"command_kind": "post_message", "expected_room_version": 2,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5c03", "issued_at": "2026-09-07T00:00:03.000Z",
		"payload": map[string]any{"body": "再发一次", "attachments": []string{roomMeta.Token}},
	})
	if reuseResp.StatusCode == http.StatusOK {
		t.Fatal("已消费令牌应拒绝")
	}

	// 快照 Timeline 含描述子
	snapResp, err := http.Get(ts.URL + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer snapResp.Body.Close()
	raw, _ := io.ReadAll(snapResp.Body)
	if !strings.Contains(string(raw), `"attachment_id"`) || !strings.Contains(string(raw), "note.md") {
		t.Fatalf("快照应含附件描述子: %s", string(raw)[:min(300, len(raw))])
	}

	// 下载（描述子 id 自快照提取）
	var snap struct {
		Timeline []struct {
			Attachments []struct {
				AttachmentID string `json:"attachment_id"`
			} `json:"attachments"`
		} `json:"timeline"`
	}
	_ = json.Unmarshal(raw, &snap)
	attID := ""
	for _, item := range snap.Timeline {
		if len(item.Attachments) > 0 {
			attID = item.Attachments[0].AttachmentID
		}
	}
	if attID == "" {
		t.Fatal("快照未找到附件")
	}
	dl, err := http.Get(ts.URL + "/v1/rooms/" + roomID + "/attachments/" + attID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer dl.Body.Close()
	body, _ := io.ReadAll(dl.Body)
	if dl.StatusCode != http.StatusOK || string(body) != "# 标题\n正文" {
		t.Fatalf("下载内容应一致: %d %q", dl.StatusCode, body)
	}
	// 未知附件 404
	nf, _ := http.Get(ts.URL + "/v1/rooms/" + roomID + "/attachments/att_nope")
	nf.Body.Close()
	if nf.StatusCode != http.StatusNotFound {
		t.Fatalf("未知附件应 404，got %d", nf.StatusCode)
	}
}

func TestAttachmentUploadTooLarge(t *testing.T) {
	ts, _ := newAttachTestServer(t, "")
	big := bytes.Repeat([]byte("a"), attach.MaxFileBytes+128)
	resp := uploadMultipart(t, ts.URL+"/v1/rooms/r1/attachments", "", "big.bin", "", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，got %d", resp.StatusCode)
	}
}

func TestAttachmentDisabled404(t *testing.T) {
	ts, _, _ := newTestServer(t) // 未注入 Attachments
	resp := uploadMultipart(t, ts.URL+"/v1/rooms/r1/attachments", "", "a.txt", "text/plain", []byte("x"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未装配备份面应 404，got %d", resp.StatusCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
