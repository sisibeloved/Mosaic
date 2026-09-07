//go:build st

// ST 层：附件面（RFC-0013）——真实二进制两步上传全链：multipart 上传 →
// post_message 令牌定稿 → 快照含描述子 → 下载字节一致 → 删除房间级联清理。
package main_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentRoundtrip_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	base := "http://" + waitListening(t, stdout)
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	tok := ownerTokenST(t, base)

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5d01", "issued_at": "2026-09-07T00:00:01.000Z",
		"payload": map[string]any{"display_name": "附件 ST 房"},
	})
	roomID := created["room_id"].(string)

	// 1) multipart 上传
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "logs.txt")
	_, _ = fw.Write([]byte("ST 附件内容 line1\nline2"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/rooms/"+roomID+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Owner-Token", tok)
	up, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	var meta struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(up.Body).Decode(&meta)
	up.Body.Close()
	if up.StatusCode != 200 || meta.Token == "" {
		t.Fatalf("上传失败: %d %+v", up.StatusCode, meta)
	}

	// 2) post_message 令牌定稿
	posted := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5d02", "issued_at": "2026-09-07T00:00:02.000Z",
		"payload": map[string]any{"body": "附件来了", "attachments": []string{meta.Token}},
	})
	if posted["event_id"] == nil {
		t.Fatalf("带附件消息发布失败: %+v", posted)
	}

	// 3) 快照含描述子 + 4) 下载一致
	snapResp, err := http.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap struct {
		Timeline []struct {
			Attachments []struct {
				AttachmentID string `json:"attachment_id"`
			} `json:"attachments"`
		} `json:"timeline"`
	}
	_ = json.NewDecoder(snapResp.Body).Decode(&snap)
	snapResp.Body.Close()
	attID := ""
	for _, item := range snap.Timeline {
		if len(item.Attachments) > 0 {
			attID = item.Attachments[0].AttachmentID
		}
	}
	if attID == "" {
		t.Fatal("快照未找到附件描述子")
	}
	dl, err := http.Get(base + "/v1/rooms/" + roomID + "/attachments/" + attID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	body, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	if dl.StatusCode != 200 || !strings.Contains(string(body), "line1") {
		t.Fatalf("下载内容不符: %d %q", dl.StatusCode, body)
	}

	// 5) 删除房间 → 附件目录级联清理
	del := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "delete_room", "expected_room_version": 2,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5d03", "issued_at": "2026-09-07T00:00:03.000Z",
		"payload": map[string]any{"reason": "ST 清理"},
	})
	if del["event_id"] == nil {
		t.Fatalf("删除失败: %+v", del)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "attachments", "rooms", roomID)); !os.IsNotExist(err) {
		t.Fatalf("附件目录应级联清理: %v", err)
	}
}
