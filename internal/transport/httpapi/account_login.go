// zcode 账号登录（独立登录档）端点：状态查询 / 异步发起 / 过程 SSE——形态同更新任务
// （0.4.1 过程可视化：POST 受理即返，SSE 重放 + 实时转发）。边界经 AccountLoginBackend
// 收敛（app 装配接入 zcode 适配器的任务面——httpapi 不依赖适配器包）。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// ErrAccountLoginUnsupported 该实例不支持账号登录（非 zcode / 非 native 运行面 /
// 未装配独立登录档）。
var ErrAccountLoginUnsupported = errors.New("httpapi: 该实例不支持账号登录")

// 登录家族（内核 login 子命令的位置参数）。
const (
	accountLoginFamilyBigmodel = "bigmodel"
	accountLoginFamilyZai      = "zai"
)

// AccountLoginStatus 独立登录档状态（契约 HarnessAccountLoginStatus）。
type AccountLoginStatus struct {
	Supported bool     `json:"supported"`
	LoggedIn  bool     `json:"logged_in"`
	Account   string   `json:"account,omitempty"`
	Model     string   `json:"model,omitempty"`
	Providers []string `json:"providers"`
}

// AccountLoginEvent 登录任务事件（SSE data 载荷）。
type AccountLoginEvent struct {
	Kind    string `json:"kind"` // url（授权链接）| line（过程行）| done（成功终态）| failed（失败终态）| idle（无任务）
	Line    string `json:"line,omitempty"`
	URL     string `json:"url,omitempty"`
	Account string `json:"account,omitempty"`
	Model   string `json:"model,omitempty"`
	Error   string `json:"error,omitempty"`
}

// AccountLoginBackend zcode 账号登录任务面。Status 的 ok=false 表示该实例不支持
// （存在性由调用方经注册表先行判定）；Start 对不支持的实例返回
// ErrAccountLoginUnsupported；Subscribe 的 ok=false 表示该实例尚无任务。
type AccountLoginBackend interface {
	AccountLoginStatus(id string) (AccountLoginStatus, bool)
	StartAccountLogin(ctx context.Context, id, family string) (started bool, err error)
	SubscribeAccountLogin(id string) (events <-chan AccountLoginEvent, cancel func(), ok bool)
}

// harnessExecutableOf 注册表取实例：未配置注册表 503、不存在 404（已写出响应时
// 返回 false）。
func (s *server) harnessExecutableOf(w http.ResponseWriter, id string) (harness.Executable, bool) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return harness.Executable{}, false
	}
	exe, ok := s.deps.Harness.Lookup(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", harness.ErrNotFound.Error())
		return harness.Executable{}, false
	}
	return exe, true
}

// GetHarnessExecutableAccountLogin GET /v1/harness/executables/{id}/account-login：
// 独立登录档状态。未接入登录面 / 非 zcode 实例同样 200（supported=false——界面据此
// 不呈现登录块）。
func (s *server) GetHarnessExecutableAccountLogin(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if _, ok := s.harnessExecutableOf(w, string(id)); !ok {
		return
	}
	st := AccountLoginStatus{Providers: []string{}}
	if s.deps.AccountLogin != nil {
		if got, ok := s.deps.AccountLogin.AccountLoginStatus(string(id)); ok {
			if got.Providers == nil {
				got.Providers = []string{}
			}
			st = got
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// RunHarnessExecutableAccountLogin POST /v1/harness/executables/{id}/account-login：
// 启动登录任务（异步执行，202 受理即返——started=本次启动 / running=同实例任务在途
// 幂等）；过程经 GET .../account-login/stream SSE 转发。请求体可空或 {"family":
// "bigmodel"|"zai"}（缺省 bigmodel）。写门三层全量适用（带体时要求 JSON）。
func (s *server) RunHarnessExecutableAccountLogin(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if _, ok := s.harnessExecutableOf(w, string(id)); !ok {
		return
	}
	if !s.guardWrite(w, r, false) { // 体可空：先过跨源门 + owner token
		return
	}
	family, ok := s.decodeAccountLoginFamily(w, r)
	if !ok {
		return
	}
	if s.deps.AccountLogin == nil {
		writeError(w, http.StatusServiceUnavailable, "account_login_unavailable", "本装配未接入账号登录")
		return
	}
	started, err := s.deps.AccountLogin.StartAccountLogin(r.Context(), string(id), family)
	if err != nil {
		if errors.Is(err, ErrAccountLoginUnsupported) {
			writeError(w, http.StatusConflict, "account_login_unsupported", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	status := "running"
	if started {
		status = "started"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": status})
}

// decodeAccountLoginFamily 请求体解析：空体 = 缺省家族；带体须 JSON、严格解码
// （未知字段 400）、家族不在支持列表 400。
func (s *server) decodeAccountLoginFamily(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type 须为 application/json")
		return "", false
	}
	if r.Body == nil {
		return "", true
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return "", false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", true
	}
	var req struct {
		Family *string `json:"family"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return "", false
	}
	if req.Family == nil {
		return "", true
	}
	family := strings.TrimSpace(*req.Family)
	switch family {
	case accountLoginFamilyBigmodel, accountLoginFamilyZai:
		return family, true
	}
	writeError(w, http.StatusBadRequest, "invalid_request",
		fmt.Sprintf("family 须为 %s 或 %s", accountLoginFamilyBigmodel, accountLoginFamilyZai))
	return "", false
}

// GetHarnessExecutableAccountLoginStream GET /v1/harness/executables/{id}/account-login/stream：
// 登录任务过程 SSE——连接即重放已积累事件（授权链接在途即报），随后实时转发，终态
// （done/failed）后关流；无任务发 idle 事件即关。只读观察面：无写门（发起登录仍走
// owner 门控的 POST）。15s 注释心跳防中间层缓冲。
func (s *server) GetHarnessExecutableAccountLoginStream(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if _, ok := s.harnessExecutableOf(w, string(id)); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming", "响应不支持流式")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(ev AccountLoginEvent) {
		fmt.Fprintf(w, "data: %s\n\n", mustMarshalJSON(ev))
		flusher.Flush()
	}
	idle := AccountLoginEvent{Kind: "idle"}
	if s.deps.AccountLogin == nil {
		send(idle)
		return
	}
	events, cancel, ok := s.deps.AccountLogin.SubscribeAccountLogin(string(id))
	if !ok {
		send(idle)
		return
	}
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			send(ev)
			if ev.Kind == "done" || ev.Kind == "failed" {
				return
			}
		}
	}
}
