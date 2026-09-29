// Package extdocs：外部云文档集成（RFC-0016 多 provider 泛化；取代 RFC-0015
// 的 txdocs 单 provider 域）。
//
// 文档级映射、非块级同步：共享 = 导出 markdown 快照到外部云文档（腾讯智能
// 文档 / 飞书导入 docx），记录 doc_id ↔ 远端 URL 映射；反向 = 外部文档链接
// 在发送时定稿为描述子快照（external_refs），卡片展示发送时刻的事实。
//
// Provider 插件面：每家外部服务一个实现（tencent.go / feishu.go），凭据与
// 上游协议实现自持；Service 只做注册表 + 共享事务编排 + host 分派。出网
// 白名单 = 各 provider 的 HostMatches（resolve 分派与 external_refs 校验
// 同源——改一处必看两处）。
package extdocs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// Provider 稳定标识（映射表 provider 键 / API 路径段）。
const (
	ProviderTencent = "tencent_docs"
	ProviderFeishu  = "feishu_docs"
)

// Provider 外部文档服务插件面。
type Provider interface {
	// ID 稳定标识（映射表 provider 键 / API 路径段）。
	ID() string
	// DisplayName 展示名（设置页/共享菜单）。
	DisplayName() string
	// RefKind external_refs.kind 值（room-protocol 封闭枚举成员）。
	RefKind() string
	// HostMatches 链接归属判定（host 精确或后缀匹配）。
	HostMatches(host string) bool
	// Verify 探活（凭据有效性；业务失败以 ErrUpstream/ErrReauthRequired 语义返回）。
	Verify(ctx context.Context) error
	// ShareMarkdown 创建远端文档（markdown 快照），返回远端 URL。
	ShareMarkdown(ctx context.Context, title, markdown string) (string, error)
	// ResolveLink 链接 → 卡片描述子（host 已匹配本 provider；title 尽力可为空）。
	ResolveLink(ctx context.Context, rawURL string) (LinkInfo, error)
}

// Share 文档级共享映射（快照语义：记录创建时点，远端内容不回同步）。
type Share struct {
	DocID       string `json:"doc_id"`
	Provider    string `json:"provider"`
	ExternalURL string `json:"external_url"`
	SharedBy    string `json:"shared_by"`
	SharedAt    string `json:"shared_at"` // RFC3339
}

// ShareStore 映射存储端口（sqlite 实现；docID/provider 空串 = 全量）。
type ShareStore interface {
	ListExternalShares(ctx context.Context, docID, provider string) ([]Share, error)
	GetExternalShare(ctx context.Context, docID, provider string) (Share, error)
	UpsertExternalShare(ctx context.Context, sh Share) error
	DeleteExternalShare(ctx context.Context, docID, provider string) error
}

// LinkInfo resolve 产物（ExternalRef 的构造材料）。
type LinkInfo struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	DocType string `json:"doc_type"`
}

// Service 注册表 + 共享事务编排 + 链接分派。
type Service struct {
	providers map[string]Provider
	order     []string
	store     ShareStore
	Clock     func() time.Time
	Logger    *slog.Logger
}

// NewService 构造（store 可为 nil——共享/列表面不可用，verify/resolve 仍可用）。
func NewService(store ShareStore) *Service {
	return &Service{providers: map[string]Provider{}, store: store, Clock: time.Now, Logger: slog.Default()}
}

// Register 登记 provider（重复 ID 覆盖，序保持首登）。
func (s *Service) Register(p Provider) {
	if _, ok := s.providers[p.ID()]; !ok {
		s.order = append(s.order, p.ID())
	}
	s.providers[p.ID()] = p
}

// Provider 按 ID 取（不存在 false）。
func (s *Service) Provider(id string) (Provider, bool) {
	p, ok := s.providers[id]
	return p, ok
}

// Providers 全部 provider（登记序）。
func (s *Service) Providers() []Provider {
	out := make([]Provider, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.providers[id])
	}
	return out
}

// ProviderByHost 按 host 分派（无匹配 false）。
func (s *Service) ProviderByHost(host string) (Provider, bool) {
	for _, id := range s.order {
		if s.providers[id].HostMatches(host) {
			return s.providers[id], true
		}
	}
	return nil, false
}

// ShareDoc 共享事务（编排层只递标题/markdown——导出由 httpapi 用 DocSVC 完成）：
// 已有映射且未 force → ErrAlreadyShared（携带现有映射）；上游创建 → 记录映射。
// 上游成功但映射落库失败时远端文档已存在（快照语义可容忍——重试 force 即可）。
func (s *Service) ShareDoc(ctx context.Context, providerID, docID, title, markdown, sharedBy string, force bool) (Share, error) {
	p, ok := s.Provider(providerID)
	if !ok {
		return Share{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	if s.store == nil {
		return Share{}, ErrShareNotFound
	}
	if !force {
		existing, err := s.store.GetExternalShare(ctx, docID, providerID)
		switch {
		case err == nil:
			return Share{}, &AlreadySharedError{Existing: existing}
		case !errors.Is(err, ErrShareNotFound):
			return Share{}, fmt.Errorf("extdocs: 查询现有映射: %w", err)
		}
	}
	externalURL, err := p.ShareMarkdown(ctx, title, markdown)
	if err != nil {
		return Share{}, err
	}
	sh := Share{
		DocID:       docID,
		Provider:    providerID,
		ExternalURL: externalURL,
		SharedBy:    sharedBy,
		SharedAt:    s.Clock().Format(time.RFC3339),
	}
	if err := s.store.UpsertExternalShare(ctx, sh); err != nil {
		return Share{}, fmt.Errorf("extdocs: 记录共享映射: %w", err)
	}
	s.Logger.Info("extdocs: 文档已共享到外部云文档", "provider", providerID, "doc_id", docID, "url", externalURL)
	return sh, nil
}

// ListShares 映射列表（空串过滤 = 全量）。
func (s *Service) ListShares(ctx context.Context, docID, providerID string) ([]Share, error) {
	if s.store == nil {
		return nil, ErrShareNotFound
	}
	return s.store.ListExternalShares(ctx, docID, providerID)
}

// DeleteShare 解除映射（不动远端文档）。
func (s *Service) DeleteShare(ctx context.Context, providerID, docID string) error {
	if s.store == nil {
		return ErrShareNotFound
	}
	if _, ok := s.Provider(providerID); !ok {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	return s.store.DeleteExternalShare(ctx, docID, providerID)
}

// ResolveLink 链接 → 描述子：https + host 白名单分派（无归属即 ErrBadLink，
// SSRF 防线——不出网）；归属 provider 做类型推断与 title 尽力拉取。
func (s *Service) ResolveLink(ctx context.Context, rawURL string) (LinkInfo, Provider, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return LinkInfo{}, nil, ErrBadLink
	}
	p, ok := s.ProviderByHost(u.Hostname())
	if !ok {
		return LinkInfo{}, nil, ErrBadLink
	}
	info, err := p.ResolveLink(ctx, u.String())
	if err != nil {
		return LinkInfo{}, nil, err
	}
	return info, p, nil
}
