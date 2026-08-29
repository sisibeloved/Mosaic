//go:build st

// ST 层：真实二进制 + 真实 HTTP 的系统级验证。
// 构建标签 st；CI 独立步骤执行（go test -tags st ./...）。
// 测试现场构建 cmd/mosaic-server 二进制（需要 go 在 PATH）。
package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain 不在 PATH（ST 需要现场构建二进制）")
	}
	bin := filepath.Join(t.TempDir(), "mosaic-server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// go test 的工作目录即本包目录（cmd/mosaic-server），故构建目标为 "."。
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("构建 mosaic-server 失败: %v\n%s", err, out)
	}
	return bin
}

type logLine struct {
	Msg  string `json:"msg"`
	Addr string `json:"addr"`
}

// waitListening 从 stdout 的 JSON 日志中解析实际监听地址（main 先 Listen 再 Serve，
// 日志携带 ln.Addr() 的真实值，支持 -addr 127.0.0.1:0 随机端口）。
func waitListening(t *testing.T, stdout io.Reader) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var l logLine
			if json.Unmarshal(sc.Bytes(), &l) == nil &&
				l.Msg == "mosaic-server listening" && l.Addr != "" {
				ch <- l.Addr
				return
			}
		}
	}()
	select {
	case addr, ok := <-ch:
		if !ok || addr == "" {
			t.Fatal("进程退出前未捕获监听地址（启动失败或日志格式变化）")
		}
		return addr
	case <-time.After(15 * time.Second):
		t.Fatal("等待监听地址超时")
		return ""
	}
}

// TestHealthz_ST：真实进程启动 → 随机端口 → /healthz 返回 200 与既定 JSON。
func TestHealthz_ST(t *testing.T) {
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	addr := waitListening(t, stdout)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if want := "{\"status\":\"ok\"}\n"; string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

// TestGracefulShutdown_ST：SIGINT → 优雅退出（零码）。Windows 无法跨进程投递
// SIGINT，该路径在 Windows 上以代码审查 + UT 覆盖，此处仅 POSIX 端到端。
func TestGracefulShutdown_ST(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 进程不支持跨进程 SIGINT 投递；优雅退出由 POSIX 矩阵覆盖")
	}
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = waitListening(t, stdout)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("优雅退出应以零码结束: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("优雅退出超时")
	}
}

// TestInvalidFlagExitsNonZero_ST：非法参数快速失败且非零码退出。
func TestInvalidFlagExitsNonZero_ST(t *testing.T) {
	bin := buildServer(t)
	err := exec.Command(bin, "-definitely-not-a-flag").Run()
	if err == nil {
		t.Fatal("非法参数应以非零码退出")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("退出错误类型 = %T, want *exec.ExitError", err)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("退出码 = 0, want 非零")
	}
}

// TestNonLoopbackGuard_ST（二轮审校 #17）：owner/harness API 无认证，
// 非回环监听未显式豁免（-allow-remote）必须拒绝启动；豁免后放行进入监听。
func TestNonLoopbackGuard_ST(t *testing.T) {
	bin := buildServer(t)
	err := exec.Command(bin, "-addr", "0.0.0.0:0", "-data", t.TempDir()).Run()
	if err == nil {
		t.Fatal("非回环监听未豁免必须拒绝启动")
	}
	cmd := exec.Command(bin, "-addr", "0.0.0.0:0", "-data", t.TempDir(), "-allow-remote")
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitListening(t, stdout) // 豁免后不拒绝：进入监听
}

// TestDiscussionLoop_ST：北极星规格——命令 → 事件 → SSE 游标订阅 → 引擎轮 → 断线重连。
// 覆盖 M1 出口判据的 HTTP 形态（真实二进制 + SQLite + outbox 分发 + echo 引擎）。
func TestDiscussionLoop_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	base := "http://" + waitListening(t, stdout)
	client := &http.Client{Timeout: 10 * time.Second}

	postCommand := func(url string, body map[string]any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 1) 创建房间（幂等命令契约）
	status, created := postCommand(base+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7001", "issued_at": "2026-08-28T12:00:00Z",
		"payload": map[string]any{"display_name": "st 房"},
	})
	if status != 200 {
		t.Fatalf("create_room status=%d body=%v", status, created)
	}
	roomID, _ := created["room_id"].(string)
	if !strings.HasPrefix(roomID, "room_") {
		t.Fatalf("create_room 未返回房间 ID：%v", created)
	}

	// 2) 先开 SSE 订阅（从头），再发人类消息
	framesCh := make(chan sseFrame, 64)
	sseCtx, stopSSE := context.WithCancel(context.Background())
	defer stopSSE()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		base+"/v1/rooms/"+roomID+"/events?cursor=", nil)
	sseResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	go func() {
		defer sseResp.Body.Close()
		sc := bufio.NewScanner(sseResp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				f.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f.Data)
			case line == "":
				if f.ID != "" {
					framesCh <- f
				}
				f = sseFrame{}
			}
		}
	}()

	nextFrame := func(want string) sseFrame {
		t.Helper()
		for {
			select {
			case f := <-framesCh:
				if f.Name == want {
					return f
				}
				// 中间帧（round 等）按序放行校验
			case <-time.After(15 * time.Second):
				t.Fatalf("等待 %s 帧超时", want)
				return sseFrame{}
			}
		}
	}

	status, posted := postCommand(base+"/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7002", "issued_at": "2026-08-28T12:00:01Z",
		"payload": map[string]any{"body": "ST 环回：人类第一条消息"},
	})
	if status != 200 {
		t.Fatalf("post_message status=%d body=%v", status, posted)
	}

	// 3) SSE 依次收到：room.created → 人类 message.posted → 轮事件 → agent message.posted
	createdFrame := nextFrame("room.created")
	if createdFrame.Data["seq"] != nil || createdFrame.Data["tenant_id"] != nil {
		t.Fatal("对外 SSE 帧不得含 seq/tenant_id（RFC-0001 P0）")
	}
	humanFrame := nextFrame("message.posted")
	if actor := nestedMap(humanFrame.Data, "actor"); actor["kind"] != "human" {
		t.Fatalf("首条 message.posted 应为人类：%v", actor)
	}
	agentFrame := nextFrame("message.posted") // 第二条 = 引擎产出的 agent 发言
	actor := nestedMap(agentFrame.Data, "actor")
	if actor["kind"] != "agent" || actor["participant_id"] != "par_echo" {
		t.Fatalf("引擎应产出 agent 发言：%v", actor)
	}
	if nestedMap(agentFrame.Data, "payload")["body"] == nil {
		t.Fatal("agent 发言缺 body")
	}
	agentID := agentFrame.ID

	// 4) 断线重连：携最后游标续传——旧事件不重投
	stopSSE()
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		base+"/v1/rooms/"+roomID+"/events?cursor="+url.QueryEscape(agentID), nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("reconnect SSE: %v", err)
	}
	sc := bufio.NewScanner(resp2.Body)
	var replayed []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			break
		}
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			id := strings.TrimPrefix(line, "id: ")
			if id == agentID {
				t.Fatal("续传重投了已消费事件")
			}
			replayed = append(replayed, id)
		}
		if len(replayed) > 0 && strings.Contains(line, ": stream: open") {
			break
		}
	}
	resp2.Body.Close()

	// 5) HTTP 幂等：同命令同键重发 → replayed=true 同事件
	status, replay := postCommand(base+"/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7002", "issued_at": "2026-08-28T12:00:01Z",
		"payload": map[string]any{"body": "ST 环回：人类第一条消息"},
	})
	if status != 200 || replay["replayed"] != true || replay["event_id"] != posted["event_id"] {
		t.Fatalf("幂等重放不符：status=%d body=%v", status, replay)
	}
}

type sseFrame struct {
	ID   string
	Name string
	Data map[string]any
}

func nestedMap(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// TestHarnessEndpoint_ST：真实进程的宿主注册表面——列表可用且结构合法
// （扫描异步于启动；CI 无 CLI 时列表为空，本机应含真实发现项）。
func TestHarnessEndpoint_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	base := "http://" + waitListening(t, stdout)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + "/v1/harness/executables")
	if err != nil {
		t.Fatalf("GET harness: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var doc struct {
		Executables []map[string]any `json:"executables"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, exe := range doc.Executables {
		for _, required := range []string{"id", "adapter", "runtime", "path", "login_state"} {
			if _, ok := exe[required]; !ok {
				t.Fatalf("登记项缺 %q：%v", required, exe)
			}
		}
	}
}

// TestSnapshotEndpoint_ST：快照四元组（版本/水位/投影版本/Timeline）经真实服务可查。
func TestSnapshotEndpoint_ST(t *testing.T) {
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", t.TempDir())
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	base := "http://" + waitListening(t, stdout)

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7201", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "snap"},
	})
	roomID := created["room_id"].(string)
	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7202", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "snapshot 前的消息"},
	})

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var snap struct {
		RoomVersion       int64  `json:"room_version"`
		Watermark         string `json:"watermark"`
		ProjectionVersion int    `json:"projection_version"`
		AlgorithmVersion  int    `json:"algorithm_version"`
		Timeline          []any  `json:"timeline"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.RoomVersion < 2 || snap.Watermark == "" || snap.ProjectionVersion != 1 || snap.AlgorithmVersion != 1 {
		t.Fatalf("快照四元组不符：%+v", snap)
	}
	if len(snap.Timeline) < 1 {
		t.Fatalf("timeline = %d", len(snap.Timeline))
	}
}

// TestIndexUI_ST：内嵌 Timeline 最小 UI 可达。
func TestIndexUI_ST(t *testing.T) {
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", t.TempDir())
	stdout, _ := cmd.StdoutPipe()
	_ = cmd.Start()
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	base := "http://" + waitListening(t, stdout)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("Mosaic")) {
		t.Fatalf("UI 不可达：status=%d", resp.StatusCode)
	}
	// M1 收口补课（审校 2026-08-28）：UI 幂等键必须是合法 UUIDv7（uuidv7() + crypto.getRandomValues），
	// 且发命令前必须经快照端点校准版本（syncVersion）——旧实现 40 字符键全量 400、版本必 409。
	for _, marker := range []string{"function uuidv7()", "crypto.getRandomValues", "syncVersion", "/snapshot"} {
		if !bytes.Contains(body, []byte(marker)) {
			t.Fatalf("UI 缺少契约标记 %q（幂等键/版本同步回归）", marker)
		}
	}
}

// TestCrashRecovery_ST：崩溃注入——SIGKILL 后重启同数据目录：
// 事件日志零损坏、快照一致（版本不回退）、游标续传不重投、可继续写入。
func TestCrashRecovery_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()

	startServer := func() (*exec.Cmd, string) {
		cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		return cmd, "http://" + waitListening(t, stdout)
	}

	cmd1, base1 := startServer()
	created := postJSONST(t, base1, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7301", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "crash"},
	})
	roomID := created["room_id"].(string)
	// 人类消息触发引擎轮（echo 确定性完成）
	postJSONST(t, base1, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7302", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "crash 前的消息"},
	})
	// 等引擎轮完成（round.closed 到位）
	waitForSnapshot := func(base string) map[string]any {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
			if err == nil {
				var snap map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&snap)
				resp.Body.Close()
				tl, _ := snap["timeline"].([]any)
				if len(tl) >= 2 { // human + agent 消息都在
					return snap
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("崩溃前快照未就绪")
		return nil
	}
	preSnap := waitForSnapshot(base1)
	preVersion := int64(preSnap["room_version"].(float64))
	preWatermark := preSnap["watermark"].(string)

	// 崩溃注入：SIGKILL（无优雅退出——outbox/半途事务的最坏情形）
	if err := cmd1.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = cmd1.Process.Wait()

	// 重启同数据目录
	cmd2, base2 := startServer()
	defer func() { _ = cmd2.Process.Kill(); _, _ = cmd2.Process.Wait() }()

	postSnap := waitForSnapshot(base2)
	if int64(postSnap["room_version"].(float64)) != preVersion {
		t.Fatalf("重启后版本漂移：%v → %v（事件日志不得损坏/回退）", preVersion, postSnap["room_version"])
	}

	// 续传：携崩溃前水位订阅，只应收到重启后新事件（无重投）
	postJSONST(t, base2, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": preVersion,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7303", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "crash 后的消息"},
	})
	sseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		base2+"/v1/rooms/"+roomID+"/events?cursor="+url.QueryEscape(preWatermark), nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("resume sse: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sawNew := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			if strings.TrimPrefix(line, "id: ") == preWatermark {
				t.Fatal("续传重投了崩溃前事件")
			}
			sawNew = true
			break // 收到任一新事件即证明续传健康
		}
	}
	if !sawNew {
		t.Fatal("续传未收到重启后新事件")
	}
}

func postJSONST(t *testing.T, base, path string, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s status=%d body=%v", path, resp.StatusCode, out)
	}
	return out
}

// TestCodexProductionPath_ST（二轮审校 #3）：生产 codex 路径的端到端门禁——
// 真实 codex 座位参与完整轮（评估→生成→发布）。两种 opt-in：
//   - MOSAIC_ST_CODEX：真 codex 可执行文件（负责人真机演练与本用例同路径）；
//   - MOSAIC_ST_CODEX_STUB：桩 CLI（CI 必跑腿——真二进制+真适配器+真子进程+真发布门，
//     模型面以确定性脚本代理；复审 #5：生产路径不得在必跑 CI 里恒 skip）。
func TestCodexProductionPath_ST(t *testing.T) {
	codexPath := os.Getenv("MOSAIC_ST_CODEX")
	stub := codexPath == ""
	if stub {
		codexPath = os.Getenv("MOSAIC_ST_CODEX_STUB")
	}
	if codexPath == "" {
		t.Skip("未设 MOSAIC_ST_CODEX / MOSAIC_ST_CODEX_STUB（生产 codex 路径 ST 为显式 opt-in）")
	}
	if !stub {
		if out, err := exec.Command(codexPath, "login", "status").CombinedOutput(); err != nil || !strings.Contains(string(out), "Logged in") {
			t.Skipf("codex 未登录或 login status 不可用（skip）: %v %s", err, out)
		}
	}

	bin := buildServer(t)
	dataDir := t.TempDir()
	// 预置已启用的 manual codex 登记：启动扫描按 ID 合并（enabled 保留）
	registry := `{"executables":[{"id":"st-codex","adapter":"codex","runtime":"native","path":` +
		strings.TrimSpace(mustMarshalString(codexPath)) + `,"login_state":"logged_in","source":"manual","enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "harness-registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatalf("预置注册表: %v", err)
	}

	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dataDir)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	base := "http://" + waitListening(t, stdout)

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8001", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "codex st"},
	})
	roomID, _ := created["room_id"].(string)
	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8002", "issued_at": "2026-08-28T12:00:01.000Z",
		"payload": map[string]any{"body": "用一句话回答：1+1 等于几？", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})

	// codex 评估+生成可能耗时分钟级：轮询快照直至出现 par_codex 的 agent 消息
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
		if err == nil {
			var snap struct {
				Timeline []struct {
					ActorID   string `json:"actor_id"`
					ActorKind string `json:"actor_kind"`
					Body      string `json:"body"`
				} `json:"timeline"`
			}
			if json.NewDecoder(resp.Body).Decode(&snap) == nil {
				for _, item := range snap.Timeline {
					if item.ActorKind == "agent" && strings.HasPrefix(item.ActorID, "par_codex") && item.Body != "" {
						t.Logf("codex 生产路径闭环：par_codex 发布 %q", truncateStr(item.Body, 80))
						return
					}
				}
			}
			resp.Body.Close()
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal("codex 座位未在时限内发布消息（检查登录态/网络/预算）")
}

func mustMarshalString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
