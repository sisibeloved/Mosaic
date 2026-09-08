// UT 层：系统面端点——备份列表/创建、恢复两段式在线段、自诊断。
// 未装配备份/诊断面 → 404；写端点过三层门（token/Content-Type/Origin）。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/backup"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/settings"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

type fakeBackupDB struct{}

func (fakeBackupDB) BackupTo(_ context.Context, destPath string) error {
	return os.WriteFile(destPath, []byte("fake-snapshot"), 0o600)
}

// newSystemTestServer 装配备份面 + 诊断面 + 设置面（token 可选注入）。
func newSystemTestServer(t *testing.T, ownerToken string) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{Store: store, Lister: store,
		Clock:  func() string { return "2026-09-07T00:00:00.000Z" },
		NewID:  func(p string) string { return p + "_sys" },
		Tenant: "ten_local"})
	settingsStore, err := settings.Open(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("settings open: %v", err)
	}
	ts := httptest.NewServer(New(Deps{
		SVC:         svc,
		Reader:      store,
		Hub:         sse.NewHub(),
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken:  ownerToken,
		Backups:     &backup.Manager{DB: fakeBackupDB{}, Dir: t.TempDir()},
		Diagnostics: func() (map[string]any, error) { return map[string]any{"runtime": map[string]any{"os": "test"}}, nil },
		Settings:    settingsStore,
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestSystemEndpointsDisabledWithoutDeps(t *testing.T) {
	ts, _, _ := newTestServer(t) // 未注入 Backups/Diagnostics/Settings
	for _, path := range []string{"/v1/system/backups", "/v1/system/diagnostics", "/v1/system/settings"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s 未装配应 404，got %d", path, resp.StatusCode)
		}
	}
	resp, err := http.Post(ts.URL+"/v1/system/restore", "application/json", strings.NewReader(`{"backup_id":"bkp_x","confirm":true}`))
	if err != nil {
		t.Fatalf("post restore: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("restore 未装配应 404，got %d", resp.StatusCode)
	}
}

func TestSystemBackupListCreateRestore(t *testing.T) {
	ts := newSystemTestServer(t, "")

	// 创建
	resp, err := http.Post(ts.URL+"/v1/system/backups", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var sum backup.Summary
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || sum.BackupID == "" {
		t.Fatalf("create 应成功: %d %+v", resp.StatusCode, sum)
	}
	// 列表
	resp, err = http.Get(ts.URL + "/v1/system/backups")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct{ Backups []backup.Summary }
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Backups) != 1 || list.Backups[0].BackupID != sum.BackupID {
		t.Fatalf("list 应含刚建备份: %+v", list.Backups)
	}
	// 恢复在线段：confirm=false 拒；true 落标记
	resp, _ = http.Post(ts.URL+"/v1/system/restore", "application/json", strings.NewReader(`{"backup_id":"`+sum.BackupID+`","confirm":false}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("confirm=false 应 400，got %d", resp.StatusCode)
	}
	resp, _ = http.Post(ts.URL+"/v1/system/restore", "application/json", strings.NewReader(`{"backup_id":"`+sum.BackupID+`","confirm":true}`))
	var rr struct {
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !rr.RestartRequired {
		t.Fatalf("restore 应排程重启: %d %+v", resp.StatusCode, rr)
	}
	// 未知字段拒收（解码纪律）
	resp, _ = http.Post(ts.URL+"/v1/system/restore", "application/json", strings.NewReader(`{"backup_id":"`+sum.BackupID+`","confirm":true,"extra":1}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，got %d", resp.StatusCode)
	}
	// 未知备份 404
	resp, _ = http.Post(ts.URL+"/v1/system/restore", "application/json", strings.NewReader(`{"backup_id":"bkp_nope","confirm":true}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知备份应 404，got %d", resp.StatusCode)
	}
}

func TestSystemWriteGateOwnerToken(t *testing.T) {
	ts := newSystemTestServer(t, "tok123")
	resp, err := http.Post(ts.URL+"/v1/system/backups", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 写备份应 401，got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/system/backups", nil)
	req.Header.Set("X-Owner-Token", "tok123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("携 token 应 200，got %d", resp.StatusCode)
	}
	// 恢复同受 token 门
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/system/restore", strings.NewReader(`{"backup_id":"bkp_x","confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("restore no token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 恢复应 401，got %d", resp.StatusCode)
	}
}

func TestSystemDiagnostics(t *testing.T) {
	ts := newSystemTestServer(t, "")
	resp, err := http.Get(ts.URL + "/v1/system/diagnostics")
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	var bundle map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || bundle["runtime"] == nil {
		t.Fatalf("diagnostics 应返回 bundle: %d %+v", resp.StatusCode, bundle)
	}
}

// TestSystemSettings 设置族端点（OQ-B 首员）：缺省读取、合法更新、值域与
// 解码纪律拒绝、token 写门。
func TestSystemSettings(t *testing.T) {
	ts := newSystemTestServer(t, "")

	// 缺省
	resp, err := http.Get(ts.URL + "/v1/system/settings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var doc struct {
		RunTimeoutSeconds int `json:"run_timeout_seconds"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || doc.RunTimeoutSeconds != 600 {
		t.Fatalf("缺省应 600: %d %+v", resp.StatusCode, doc)
	}

	put := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/system/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatalf("put: %v", e)
		}
		return r
	}
	// 合法更新
	resp = put(`{"run_timeout_seconds":120}`)
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || doc.RunTimeoutSeconds != 120 {
		t.Fatalf("更新应生效: %d %+v", resp.StatusCode, doc)
	}
	// 值域拒绝
	for _, bad := range []string{`{"run_timeout_seconds":10}`, `{"run_timeout_seconds":99999}`} {
		resp = put(bad)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("越界应 400: %s → %d", bad, resp.StatusCode)
		}
	}
	// 未知字段拒绝（解码纪律）
	resp = put(`{"run_timeout_seconds":120,"extra":1}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，got %d", resp.StatusCode)
	}
	// 越界值未落盘：读回仍是 120
	resp, _ = http.Get(ts.URL + "/v1/system/settings")
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc.RunTimeoutSeconds != 120 {
		t.Fatalf("拒绝后不应改值: %+v", doc)
	}
}

// 数据目录布局契约：备份落 backups/<id>/，标记落数据目录根（启动段消费路径）。
func TestSystemBackupLayoutOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := room.NewMemStore()
	ts := httptest.NewServer(New(Deps{
		SVC: room.NewService(room.Config{Store: store, Lister: store,
			Clock:  func() string { return "2026-09-07T00:00:00.000Z" },
			NewID:  func(p string) string { return p + "_sys" },
			Tenant: "ten_local"}),
		Reader:  store,
		Hub:     sse.NewHub(),
		Actor:   room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Backups: &backup.Manager{DB: fakeBackupDB{}, Dir: dir},
	}))
	defer ts.Close()
	resp, _ := http.Post(ts.URL+"/v1/system/backups", "application/json", nil)
	var sum backup.Summary
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(dir, "backups", sum.BackupID, "manifest.json")); err != nil {
		t.Fatalf("备份目录布局不符: %v", err)
	}
}
