//go:build st

// ST 层：系统面（M4-0 备份/恢复/自诊断）——真实二进制。
// 备份恢复两段式的端到端时序：实例 A（建数据→备份→再演进→请求恢复）→
// 进程退出 → 实例 B 同数据目录启动（启动段换库）→ 演进数据消失、备份时点
// 数据在、原库在安全副本目录（回滚保护）。
package main_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSystemBackupRestore_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()

	runInstance := func() (string, *exec.Cmd) {
		t.Helper()
		cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		base := "http://" + waitListening(t, stdout)
		return base, cmd
	}
	stop := func(cmd *exec.Cmd) {
		t.Helper()
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill: %v", err)
		}
		_, _ = cmd.Process.Wait()
	}

	// 实例 A：建房 → 备份 → 再建一房（演进）→ 请求恢复
	baseA, cmdA := runInstance()
	kept := postJSONST(t, baseA, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5b01", "issued_at": "2026-09-07T00:00:01.000Z",
		"payload": map[string]any{"display_name": "备份时点房"},
	})
	roomKept := kept["room_id"].(string)

	backup := postJSONST(t, baseA, "/v1/system/backups", map[string]any{})
	backupID, _ := backup["backup_id"].(string)
	if backupID == "" {
		t.Fatalf("备份应成功: %+v", backup)
	}

	evolved := postJSONST(t, baseA, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5b02", "issued_at": "2026-09-07T00:00:02.000Z",
		"payload": map[string]any{"display_name": "备份后新房"},
	})
	roomEvolved := evolved["room_id"].(string)

	// 诊断面：bundle 字段在
	resp, err := http.Get(baseA + "/v1/system/diagnostics")
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	var diag map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&diag)
	resp.Body.Close()
	if resp.StatusCode != 200 || diag["runtime"] == nil || diag["data"] == nil {
		t.Fatalf("diagnostics bundle 异常: %d %+v", resp.StatusCode, diag)
	}

	rr := postJSONST(t, baseA, "/v1/system/restore", map[string]any{"backup_id": backupID, "confirm": true})
	if rr["restart_required"] != true {
		t.Fatalf("restore 应要求重启: %+v", rr)
	}
	stop(cmdA)

	// 实例 B：同数据目录重启——启动段换库（备份时点数据 + 安全副本）
	baseB, cmdB := runInstance()
	defer stop(cmdB)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err = client.Get(baseB + "/v1/rooms")
	if err != nil {
		t.Fatalf("list rooms: %v", err)
	}
	var list struct {
		Rooms []struct {
			RoomID string `json:"room_id"`
		} `json:"rooms"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	ids := map[string]bool{}
	for _, r := range list.Rooms {
		ids[r.RoomID] = true
	}
	if !ids[roomKept] {
		t.Fatalf("备份时点房间应在恢复后存在: %+v", ids)
	}
	if ids[roomEvolved] {
		t.Fatal("备份后的演进房间应随恢复消失")
	}
	// 回滚保护：安全副本目录有被换下的库文件
	safety := filepath.Join(dataDir, "restore-safety")
	entries, err := os.ReadDir(safety)
	if err != nil || len(entries) == 0 {
		t.Fatalf("安全副本目录应存在且非空: %v", err)
	}
}
