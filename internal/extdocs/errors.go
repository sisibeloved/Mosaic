// extdocs 域错误哨兵（httpapi 映射稳定 code extdocs_*）。
package extdocs

import "errors"

var (
	// ErrTokenMissing 凭据未配置（409 extdocs_token_missing——指路设置页）。
	ErrTokenMissing = errors.New("extdocs: 凭据未配置")
	// ErrReauthRequired 需要重新授权（409 extdocs_reauth_required——OAuth 失效/
	// 刷新失败，指路设置页重新登录）。
	ErrReauthRequired = errors.New("extdocs: 需要重新授权")
	// ErrUpstream 上游失败（502 extdocs_upstream_error；UpstreamError 携带上游码）。
	ErrUpstream = errors.New("extdocs: 上游失败")
	// ErrBadLink 非白名单链接（400 extdocs_bad_link——host 无归属 provider）。
	ErrBadLink = errors.New("extdocs: 链接不在外部文档白名单内")
	// ErrShareNotFound 共享映射不存在（404）。
	ErrShareNotFound = errors.New("extdocs: 共享映射不存在")
	// ErrAlreadyShared 已有映射且未要求覆盖（409 extdocs_already_shared）。
	ErrAlreadyShared = errors.New("extdocs: 已有共享映射")
	// ErrUnknownProvider 未知 provider 标识（404 extdocs_bad_provider）。
	ErrUnknownProvider = errors.New("extdocs: 未知 provider")
)

// UpstreamError 上游失败的结构化包装（code 进 HTTP 错误体；不含凭据）。
type UpstreamError struct {
	Code    string // 上游错误码（"unknown" = 未解析出）
	Message string // 摘要（已截断、无凭据）
}

func (e *UpstreamError) Error() string {
	if e.Code == "" {
		return "extdocs: 上游失败: " + e.Message
	}
	return "extdocs: 上游失败(" + e.Code + "): " + e.Message
}

func (e *UpstreamError) Unwrap() error { return ErrUpstream }

// AlreadySharedError ErrAlreadyShared 的携带体（现有映射随错误返回——上层 409
// 响应可展示既有 URL，前端不必再查一次列表）。
type AlreadySharedError struct {
	Existing Share
}

func (e *AlreadySharedError) Error() string {
	return "extdocs: 已有共享映射: " + e.Existing.ExternalURL
}

func (e *AlreadySharedError) Unwrap() error { return ErrAlreadyShared }
