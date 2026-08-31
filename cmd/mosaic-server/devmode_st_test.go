//go:build st

// ST 层（M1 v1.8）：开发者模式系统级验证——真实二进制 + -dev 开关 +
// 完整轮次后的可复盘性（状态端点 / 事件检视 / debug 日志链）。
package main_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// devLogCapture 持续收集进程 stdout 日志（waitListening 找到地址即停，
// dev 测试需要全程日志以验证链路易查性）。
type devLogCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *devLogCapture) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// startLoggedServer 启动真实二进制并全程收集日志；返回进程、基址、日志收集器。
func startLoggedServer(t *testing.T, bin, dataDir string, extra ...string) (*exec.Cmd, string, *devLogCapture) {
	t.Helper()
	args := append([]string{"-addr", "127.0.0.1:0", "-data", dataDir}, extra...)
	cmd := exec.Command(bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	cap := &devLogCapture{}
	addrCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			cap.mu.Lock()
			cap.lines = append(cap.lines, line)
			cap.mu.Unlock()
			var l logLine
			if json.Unmarshal([]byte(line), &l) == nil && l.Msg == "mosaic-server listening" && l.Addr != "" {
				select {
				case addrCh <- l.Addr:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-addrCh:
		return cmd, "http://" + addr, cap
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatal("等待监听地址超时")
		return nil, "", nil
	}
}

// TestDevMode_ST：M1 v1.8 出口判据——开发者模式下可复盘一次完整轮次链路
// （命令→intent→grant→生成→发布）。三通道验证：只读状态端点、事件流检视
// （权威信封含因果 id）、stdout debug 日志（trace id 与轮环节 ids）。
func TestDevMode_ST(t *testing.T) {
	bin := buildServer(t)

	// 1) 无 -dev：调试端点不得注册（404，不暴露面）
	cmd0, base0, _ := startLoggedServer(t, bin, t.TempDir())
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base0 + "/v1/debug/rooms/room_x/state")
	if err != nil {
		t.Fatalf("GET debug state: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("无 -dev 时调试端点 status=%d, want 404", resp.StatusCode)
	}
	_ = cmd0.Process.Kill()
	_, _ = cmd0.Process.Wait()

	// 2) 有 -dev：完整轮次 → 三通道复盘
	cmd, base, logs := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9001", "issued_at": "2026-08-30T09:30:00.000Z",
		"payload": map[string]any{"display_name": "dev"},
	})
	roomID := created["room_id"].(string)
	posted := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9002", "issued_at": "2026-08-30T09:30:01.000Z",
		"payload": map[string]any{"body": "dev 模式复盘", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})
	stimulusID := posted["event_id"].(string)

	// 轮询状态端点直至 echo 轮完成且分发排空（引擎装配晚于宿主扫描，留足窗口）。
	// 完成判据必须覆盖"轮闭环 + 发言入账 + outbox 排空"——epoch=1 在 round.opened
	// 落库即成立，此刻 round.closed/message.posted 可能尚未追加、outbox 未排空，
	// 只看 epoch 会在快/慢机器间漂移（全套件负载下实证过一次 backlog≠0 误报）。
	roundDone := func(s map[string]any) bool {
		if s["epoch"] != float64(1) {
			return false
		}
		b, _ := s["budget"].(map[string]any)
		if b["utterances"] != float64(1) {
			return false
		}
		ob, _ := s["outbox"].(map[string]any)
		return ob["backlog"] == float64(0)
	}
	var state map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/v1/debug/rooms/" + roomID + "/state")
		if err == nil && resp.StatusCode == 200 {
			_ = json.NewDecoder(resp.Body).Decode(&state)
		}
		if resp != nil {
			resp.Body.Close()
		}
		if roundDone(state) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !roundDone(state) {
		t.Fatalf("echo 轮未在时限内完成并排空：state=%v", state)
	}
	// 状态面：版本/暂停/座位/预算水位/outbox 积压
	if state["paused"] != false {
		t.Fatalf("paused = %v", state["paused"])
	}
	seats, _ := state["seats"].([]any)
	foundEcho := false
	for _, s := range seats {
		if s.(map[string]any)["participant_id"] == "par_echo" {
			foundEcho = true
		}
	}
	if !foundEcho {
		t.Fatalf("座位快照缺 par_echo：%v", seats)
	}
	budget, _ := state["budget"].(map[string]any)
	if budget["rounds"] != float64(1) || budget["utterances"] != float64(1) {
		t.Fatalf("预算水位 = %v（rounds/utterances 应各为 1）", budget)
	}
	outboxDoc, _ := state["outbox"].(map[string]any)
	if outboxDoc["backlog"] != float64(0) {
		t.Fatalf("轮完成后 outbox 应排空：%v", outboxDoc)
	}

	// 事件检视：完整轮次链路（round.opened → intent.recorded → floor.granted →
	// message.posted(agent) → round.closed），因果 id 串起命令→轮→发布。
	resp2, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/debug/rooms/" + roomID + "/events")
	if err != nil {
		t.Fatalf("debug events: %v", err)
	}
	var eventsDoc struct {
		Events []struct {
			Cursor   string          `json:"cursor"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&eventsDoc); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	resp2.Body.Close()
	var types []string
	openedCausation, grantEpoch := "", int64(0)
	agentPosted := false
	for _, item := range eventsDoc.Events {
		var env struct {
			Type        string  `json:"type"`
			Seq         int64   `json:"seq"`
			CausationID *string `json:"causation_id"`
			Actor       struct {
				Kind          string `json:"kind"`
				ParticipantID string `json:"participant_id"`
			} `json:"actor"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(item.Envelope, &env); err != nil {
			t.Fatalf("envelope decode: %v", err)
		}
		if env.Seq <= 0 {
			t.Fatal("调试面权威信封必须含内部 seq")
		}
		types = append(types, env.Type)
		switch env.Type {
		case "round.opened":
			if env.CausationID != nil {
				openedCausation = *env.CausationID
			}
		case "floor.granted":
			var p struct {
				Epoch int64 `json:"epoch"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			grantEpoch = p.Epoch
		case "message.posted":
			if env.Actor.Kind == "agent" {
				agentPosted = true
			}
		}
	}
	for _, want := range []string{"room.created", "message.posted", "round.opened", "intent.recorded", "floor.granted", "round.closed"} {
		found := false
		for _, typ := range types {
			if typ == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("事件链缺 %q：%v", want, types)
		}
	}
	if openedCausation != stimulusID {
		t.Fatalf("round.opened causation = %q, want 刺激事件 %q（命令→轮链路断）", openedCausation, stimulusID)
	}
	if grantEpoch != 1 || !agentPosted {
		t.Fatalf("grant epoch=%d agentPosted=%v（grant→发布链路断）", grantEpoch, agentPosted)
	}

	// debug 日志链：命令提交（trace_id）→ 分发 → 轮开始 → 发言发布
	for _, marker := range []string{
		"httpapi: command committed", "trace_id",
		"outbox: 分发条目",
		"engine: 轮开始", "engine: agent 发言已发布",
	} {
		if !logs.contains(marker) {
			t.Fatalf("debug 日志缺 %q（链路复盘手段不完整）", marker)
		}
	}

	// UI 调试面板注入
	uiResp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	uiBody, _ := io.ReadAll(uiResp.Body)
	uiResp.Body.Close()
	if !bytes.Contains(uiBody, []byte("const MOSAIC_DEV = true;")) {
		t.Fatal("-dev 时 UI 应注入 MOSAIC_DEV=true（调试面板开关）")
	}
}
