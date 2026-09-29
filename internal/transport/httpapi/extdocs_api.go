// 外部云文档集成 HTTP 端点（RFC-0016 多 provider）：providers/settings/verify/
// shares/resolve + 飞书 OAuth 引导。命令上行镜像文档面纪律——guardWrite 三件套、
// MaxBytesReader + DisallowUnknownFields、错误以稳定 code（extdocs_*）映射；
// 读端点只读不设 owner token。凭据值永不回传（GET 只回状态标志）。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// extdocsAvailable 装配门（nil ExtDocs = 测试装配未装配——404 不暴露面）。
func (s *server) extdocsAvailable(w http.ResponseWriter) bool {
	if s.deps.ExtDocs == nil {
		writeError(w, http.StatusNotFound, "extdocs_unavailable", "外部文档面未装配")
		return false
	}
	return true
}

// extdocsProvider provider 解析（未知 → 404 extdocs_bad_provider）。
func (s *server) extdocsProvider(w http.ResponseWriter, id string) (extdocs.Provider, bool) {
	p, ok := s.deps.ExtDocs.Provider(id)
	if !ok {
		writeError(w, http.StatusNotFound, "extdocs_bad_provider", "未知 provider："+id)
		return nil, false
	}
	return p, true
}

// ListExtDocProviders provider 状态清单（GET /v1/extdocs/providers）。
func (s *server) ListExtDocProviders(w http.ResponseWriter, r *http.Request) {
	if !s.extdocsAvailable(w) {
		return
	}
	out := apigen.ExtDocProviderList{Providers: []apigen.ExtDocProviderView{}}
	for _, p := range s.deps.ExtDocs.Providers() {
		v := apigen.ExtDocProviderView{
			Provider:    apigen.ExtDocProviderViewProvider(p.ID()),
			DisplayName: p.DisplayName(),
			RefKind:     p.RefKind(),
			Configured:  false,
		}
		st := extDocProviderStatus(p)
		v.Configured = st.configured
		if st.lastVerified != nil {
			v.LastVerifiedAt = st.lastVerified
		}
		out.Providers = append(out.Providers, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// extDocProviderStatus provider 状态的统一取用（settings 写响应复用）。
type extDocStatusView struct {
	configured    bool
	appConfigured bool
	authorized    bool
	expiresAt     *time.Time
	lastVerified  *time.Time
}

func extDocProviderStatus(p extdocs.Provider) extDocStatusView {
	out := extDocStatusView{}
	rfc3339Ptr := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			return &at
		}
		return nil
	}
	switch tp := p.(type) {
	case *extdocs.TencentProvider:
		st := tp.Status()
		out.configured = st.Configured
		out.lastVerified = rfc3339Ptr(st.LastVerifiedAt)
	case *extdocs.FeishuProvider:
		st := tp.Status()
		out.appConfigured = st.AppConfigured
		out.authorized = st.Authorized
		out.configured = st.AppConfigured && st.Authorized
		if !st.ExpiresAt.IsZero() {
			t := st.ExpiresAt
			out.expiresAt = &t
		}
		out.lastVerified = rfc3339Ptr(st.LastVerifiedAt)
	}
	return out
}

// GetExtDocSettings 凭据状态（GET /v1/extdocs/{provider}/settings；无值）。
func (s *server) GetExtDocSettings(w http.ResponseWriter, r *http.Request, provider apigen.GetExtDocSettingsParamsProvider) {
	if !s.extdocsAvailable(w) {
		return
	}
	p, ok := s.extdocsProvider(w, string(provider))
	if !ok {
		return
	}
	s.writeExtDocSettings(w, p)
}

// UpdateExtDocSettings 写入/清除凭据（PUT）。载荷按 provider 分化。
func (s *server) UpdateExtDocSettings(w http.ResponseWriter, r *http.Request, provider apigen.UpdateExtDocSettingsParamsProvider) {
	if !s.extdocsAvailable(w) {
		return
	}
	p, ok := s.extdocsProvider(w, string(provider))
	if !ok {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.ExtDocSettingsUpdate
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷 JSON 不合法："+err.Error())
		return
	}
	var err error
	switch tp := p.(type) {
	case *extdocs.TencentProvider:
		token := ""
		if req.Token != nil {
			token = *req.Token
		}
		err = tp.UpdateToken(token)
	case *extdocs.FeishuProvider:
		appID, appSecret := "", ""
		if req.AppId != nil {
			appID = *req.AppId
		}
		if req.AppSecret != nil {
			appSecret = *req.AppSecret
		}
		err = tp.UpdateApp(appID, appSecret)
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "该 provider 不接受凭据写入")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.debugf("httpapi: extdocs credentials updated", "provider", provider)
	s.writeExtDocSettings(w, p)
}

func (s *server) writeExtDocSettings(w http.ResponseWriter, p extdocs.Provider) {
	st := extDocProviderStatus(p)
	out := apigen.ExtDocSettingsDoc{
		Provider:       apigen.ExtDocSettingsDocProvider(p.ID()),
		Configured:     st.configured,
		LastVerifiedAt: st.lastVerified,
	}
	// feishu 专属字段只在 feishu 视图携带（tencent 恒缺省）
	if _, isFeishu := p.(*extdocs.FeishuProvider); isFeishu {
		out.AppConfigured = &st.appConfigured
		out.Authorized = &st.authorized
		out.ExpiresAt = st.expiresAt
	}
	writeJSON(w, http.StatusOK, out)
}

// VerifyExtDoc 验证凭据（POST /v1/extdocs/{provider}/verify）：业务失败也是
// 200——ok=false 携带上游码；只有传输/协议层失败才 502。
func (s *server) VerifyExtDoc(w http.ResponseWriter, r *http.Request, provider apigen.VerifyExtDocParamsProvider) {
	if !s.extdocsAvailable(w) {
		return
	}
	p, ok := s.extdocsProvider(w, string(provider))
	if !ok {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	if err := p.Verify(r.Context()); err != nil {
		s.writeExtDocUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ExtDocVerifyResult{Ok: true})
}

// writeExtDocUpstreamError 上游错误分流：具名上游码 = 业务失败 200 ok=false；
// unknown = 传输/协议失败 502；凭据缺失/需重授权 409。
func (s *server) writeExtDocUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, extdocs.ErrTokenMissing):
		writeError(w, http.StatusConflict, "extdocs_token_missing", "凭据未配置——先在设置页保存")
	case errors.Is(err, extdocs.ErrReauthRequired):
		writeError(w, http.StatusConflict, "extdocs_reauth_required", err.Error())
	case errors.Is(err, extdocs.ErrUpstream):
		var ue *extdocs.UpstreamError
		if errors.As(err, &ue) && ue.Code != "unknown" && ue.Code != "" {
			msg := ue.Message
			writeJSON(w, http.StatusOK, apigen.ExtDocVerifyResult{
				Ok: false, UpstreamCode: &ue.Code, Message: &msg,
			})
			return
		}
		writeError(w, http.StatusBadGateway, "extdocs_upstream_error", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// ListExtDocShares 映射列表（GET /v1/extdocs/shares?doc_id=&provider=）。
func (s *server) ListExtDocShares(w http.ResponseWriter, r *http.Request, params apigen.ListExtDocSharesParams) {
	if !s.extdocsAvailable(w) {
		return
	}
	docID, providerID := "", ""
	if params.DocId != nil {
		docID = *params.DocId
	}
	if params.Provider != nil {
		providerID = string(*params.Provider)
	}
	shares, err := s.deps.ExtDocs.ListShares(r.Context(), docID, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apigen.ExtDocShareList{Shares: extDocShareViews(shares)})
}

func extDocShareViews(shares []extdocs.Share) []apigen.ExtDocShare {
	out := make([]apigen.ExtDocShare, 0, len(shares))
	for _, sh := range shares {
		at, _ := time.Parse(time.RFC3339, sh.SharedAt)
		out = append(out, apigen.ExtDocShare{
			DocId:       sh.DocID,
			Provider:    apigen.ExtDocShareProvider(sh.Provider),
			ExternalUrl: sh.ExternalURL,
			SharedBy:    sh.SharedBy,
			SharedAt:    at,
		})
	}
	return out
}

// CreateExtDocShare 共享文档（POST /v1/extdocs/shares {doc_id, provider, force}）。
// 编排：已有映射且未 force → 409（附现有映射）；导出 markdown（DocSVC）→
// provider 创建 → 记录映射。
func (s *server) CreateExtDocShare(w http.ResponseWriter, r *http.Request) {
	if !s.extdocsAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	traceID := ensureTraceID(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.ExtDocShareRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷 JSON 不合法："+err.Error())
		return
	}
	if _, ok := s.extdocsProvider(w, string(req.Provider)); !ok {
		return
	}
	if s.deps.DocSVC == nil {
		writeError(w, http.StatusNotFound, "docs_unavailable", "文档面未装配")
		return
	}
	markdown, state, err := s.deps.DocSVC.ExportDoc(r.Context(), req.DocId)
	if err != nil {
		s.writeExtDocShareDomainError(w, err)
		return
	}
	sh, err := s.deps.ExtDocs.ShareDoc(r.Context(), string(req.Provider), req.DocId, state.Title, markdown,
		s.deps.Actor.ParticipantID, req.Force != nil && *req.Force)
	if err != nil {
		s.debugf("httpapi: extdocs share rejected", "trace_id", traceID, "doc", req.DocId, "provider", req.Provider, "err", err)
		s.writeExtDocShareDomainError(w, err)
		return
	}
	s.debugf("httpapi: extdocs share committed", "trace_id", traceID, "doc", req.DocId, "provider", req.Provider)
	writeJSON(w, http.StatusCreated, extDocShareViews([]extdocs.Share{sh})[0])
}

// writeExtDocShareDomainError 共享路径域错误映射（文档面 404 与 extdocs 码分流）。
func (s *server) writeExtDocShareDomainError(w http.ResponseWriter, err error) {
	var as *extdocs.AlreadySharedError
	switch {
	case errors.As(err, &as):
		body := struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Share apigen.ExtDocShare `json:"share"`
		}{}
		body.Error.Code = "extdocs_already_shared"
		body.Error.Message = "已有共享映射（force=true 重新创建）"
		body.Share = extDocShareViews([]extdocs.Share{as.Existing})[0]
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, doc.ErrDocNotFound):
		writeError(w, http.StatusNotFound, "extdocs_doc_not_found", err.Error())
	case errors.Is(err, extdocs.ErrUnknownProvider):
		writeError(w, http.StatusNotFound, "extdocs_bad_provider", err.Error())
	case errors.Is(err, extdocs.ErrTokenMissing):
		writeError(w, http.StatusConflict, "extdocs_token_missing", "凭据未配置——先在设置页保存")
	case errors.Is(err, extdocs.ErrReauthRequired):
		writeError(w, http.StatusConflict, "extdocs_reauth_required", err.Error())
	case errors.Is(err, extdocs.ErrUpstream):
		writeError(w, http.StatusBadGateway, "extdocs_upstream_error", err.Error())
	default:
		if s.deps.Logger != nil {
			s.deps.Logger.Error("httpapi: extdocs share failed", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// DeleteExtDocShare 解除映射（DELETE /v1/extdocs/shares/{doc_id}?provider=）。
func (s *server) DeleteExtDocShare(w http.ResponseWriter, r *http.Request, docID apigen.DocID, params apigen.DeleteExtDocShareParams) {
	if !s.extdocsAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	if _, ok := s.extdocsProvider(w, string(params.Provider)); !ok {
		return
	}
	if err := s.deps.ExtDocs.DeleteShare(r.Context(), string(params.Provider), docID); err != nil {
		if errors.Is(err, extdocs.ErrShareNotFound) {
			writeError(w, http.StatusNotFound, "extdocs_share_not_found", "共享映射不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ResolveExtDocLink 链接 → 卡片描述子（POST /v1/extdocs/resolve）。拉取失败也
// 是 200（title 空，卡片降级）——只有链接不合法才 400。
func (s *server) ResolveExtDocLink(w http.ResponseWriter, r *http.Request) {
	if !s.extdocsAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.ExtDocLinkResolveRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷 JSON 不合法："+err.Error())
		return
	}
	info, p, err := s.deps.ExtDocs.ResolveLink(r.Context(), req.Url)
	if err != nil {
		if errors.Is(err, extdocs.ErrBadLink) {
			writeError(w, http.StatusBadRequest, "extdocs_bad_link",
				"链接不在外部文档白名单内（须为已配置 provider 的 https 链接）")
			return
		}
		s.writeExtDocUpstreamError(w, err)
		return
	}
	kind := apigen.ExtDocLinkInfoKind(p.RefKind())
	outTitle := info.Title
	writeJSON(w, http.StatusOK, apigen.ExtDocLinkInfo{
		Url:      info.URL,
		Kind:     &kind,
		Provider: apigen.ExtDocLinkInfoProvider(p.ID()),
		Title:    &outTitle,
		DocType:  info.DocType,
	})
}

// GetFeishuAuthorizeURL 飞书授权引导（GET /v1/extdocs/feishu/authorize）。
// 响应前挂起回环回调监听——授权页跳转落回 localhost:3000 即自动换取 token
// （auto_captured=true）；端口被占退回手动粘 code（false）。
func (s *server) GetFeishuAuthorizeURL(w http.ResponseWriter, r *http.Request) {
	if !s.extdocsAvailable(w) {
		return
	}
	p, ok := s.extdocsProvider(w, extdocs.ProviderFeishu)
	if !ok {
		return
	}
	fp, isFeishu := p.(*extdocs.FeishuProvider)
	if !isFeishu {
		writeError(w, http.StatusInternalServerError, "internal", "provider 类型异常")
		return
	}
	if st := fp.Status(); !st.AppConfigured {
		writeError(w, http.StatusConflict, "extdocs_token_missing", "先保存飞书 app_id/app_secret")
		return
	}
	auto := fp.ArmCallback()
	s.debugf("httpapi: feishu authorize", "auto_captured", auto)
	writeJSON(w, http.StatusOK, apigen.FeishuAuthorizeInfo{
		Url:          fp.AuthorizeURL(),
		RedirectUri:  fp.RedirectURIValue(),
		Scopes:       strings.Fields(extdocs.FeishuScopes),
		AutoCaptured: &auto,
	})
}

// FeishuOAuthCallback 授权码换取 token（POST /v1/extdocs/feishu/callback）。
func (s *server) FeishuOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.extdocsAvailable(w) {
		return
	}
	p, ok := s.extdocsProvider(w, extdocs.ProviderFeishu)
	if !ok {
		return
	}
	fp, isFeishu := p.(*extdocs.FeishuProvider)
	if !isFeishu {
		writeError(w, http.StatusInternalServerError, "internal", "provider 类型异常")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.FeishuCallbackRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷 JSON 不合法："+err.Error())
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "code 不能为空")
		return
	}
	if err := fp.ExchangeCode(r.Context(), req.Code); err != nil {
		s.writeExtDocUpstreamError(w, err)
		return
	}
	s.writeExtDocSettings(w, p)
}
