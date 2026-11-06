// 检查点端点测试（0.7.3 / ADR-0040）：三端点 + 双锚 + 写门 + 错误族。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/checkpoint"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func newCheckpointTestServer(t *testing.T, missionOfRun string, runExists bool) *httptest.Server {
	t.Helper()
	svc := checkpoint.NewService(checkpoint.NewMemStore(), checkpoint.Anchors{
		RunMission: func(context.Context, string) (string, bool, error) {
			return missionOfRun, runExists, nil
		},
	}, func() string { return "2026-11-06T12:00:00Z" })
	handler := New(Deps{
		CheckpointSVC: svc,
		Actor:         room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken:    "tok",
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func ckptPost(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func ckptBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestCheckpointAPIT(t *testing.T) {
	ts := newCheckpointTestServer(t, "m_1", true)
	body := map[string]any{
		"mission_id": "m_1", "mission_version": 2,
		"completed_steps": []string{"采集"}, "pending_steps": []string{"清洗"},
		"source_watermarks": map[string]string{"room:rm_1": "42"},
		"progress_summary":  "进行中",
	}
	t.Run("写门 401", func(t *testing.T) {
		resp := ckptPost(t, ts.URL+"/v1/mission-runs/run_1/checkpoints", "", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("应 401，得 %d", resp.StatusCode)
		}
	})
	t.Run("落盘 + 版本递增 + 列表降序 + latest", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			resp := ckptPost(t, ts.URL+"/v1/mission-runs/run_1/checkpoints", "tok", body)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("应 201，得 %d：%v", resp.StatusCode, ckptBody(t, resp))
			}
			v := ckptBody(t, resp)
			if v["checkpoint_version"].(float64) != float64(i+1) {
				t.Fatalf("版本应 %d", i+1)
			}
			if v["completed_steps"].([]any)[0] != "采集" {
				t.Fatalf("steps 透出错")
			}
		}
		// 列表降序。
		lr, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/mission-runs/run_1/checkpoints", nil)
		if err != nil {
			t.Fatal(err)
		}
		lr.Header.Set("X-Owner-Token", "tok")
		lresp, err := http.DefaultClient.Do(lr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { lresp.Body.Close() })
		lv := ckptBody(t, lresp)
		cps := lv["checkpoints"].([]any)
		if len(cps) != 2 || cps[0].(map[string]any)["checkpoint_version"].(float64) != 2 {
			t.Fatalf("列表降序错：%v", lv)
		}
		// latest。
		gr, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mission-runs/run_1/checkpoints/latest", nil)
		gr.Header.Set("X-Owner-Token", "tok")
		gresp, err := http.DefaultClient.Do(gr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gresp.Body.Close() })
		gv := ckptBody(t, gresp)
		if gv["checkpoint_version"].(float64) != 2 {
			t.Fatalf("latest 应 v2，得 %v", gv["checkpoint_version"])
		}
	})
	t.Run("Run 不存在 404", func(t *testing.T) {
		ts2 := newCheckpointTestServer(t, "m_1", false)
		resp := ckptPost(t, ts2.URL+"/v1/mission-runs/run_x/checkpoints", "tok", body)
		if resp.StatusCode != http.StatusNotFound || ckptBody(t, resp)["error"].(map[string]any)["code"] != "run_not_found" {
			t.Fatalf("应 404 run_not_found，得 %d", resp.StatusCode)
		}
	})
	t.Run("归属不一致 409", func(t *testing.T) {
		ts3 := newCheckpointTestServer(t, "m_other", true)
		resp := ckptPost(t, ts3.URL+"/v1/mission-runs/run_1/checkpoints", "tok", body)
		if resp.StatusCode != http.StatusConflict || ckptBody(t, resp)["error"].(map[string]any)["code"] != "checkpoint_scope_mismatch" {
			t.Fatalf("应 409 checkpoint_scope_mismatch，得 %d", resp.StatusCode)
		}
	})
	t.Run("交集非空 400", func(t *testing.T) {
		bad := map[string]any{
			"mission_id": "m_1", "mission_version": 2,
			"completed_steps": []string{"采集"}, "pending_steps": []string{"采集"},
		}
		resp := ckptPost(t, ts.URL+"/v1/mission-runs/run_1/checkpoints", "tok", bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，得 %d", resp.StatusCode)
		}
	})
	t.Run("无落盘 latest 404", func(t *testing.T) {
		gr, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mission-runs/run_404ck/checkpoints/latest", nil)
		gr.Header.Set("X-Owner-Token", "tok")
		// run_404ck 锚不存在——先 404 run_not_found；用存在的 run_2 测无落盘。
		gr2, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mission-runs/run_2/checkpoints/latest", nil)
		gr2.Header.Set("X-Owner-Token", "tok")
		gresp, err := http.DefaultClient.Do(gr2)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gresp.Body.Close() })
		if gresp.StatusCode != http.StatusNotFound || ckptBody(t, gresp)["error"].(map[string]any)["code"] != "checkpoint_not_found" {
			t.Fatalf("应 404 checkpoint_not_found，得 %d", gresp.StatusCode)
		}
	})
}

func TestCheckpointUnavailableAPIT(t *testing.T) {
	handler := New(Deps{Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"}})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/mission-runs/run_1/checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("应 503，得 %d", resp.StatusCode)
	}
}
