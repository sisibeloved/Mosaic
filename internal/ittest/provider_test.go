// UT：供应商侧不可用判定——实证措辞命中、适配器自身缺陷不命中（保守面）。
package ittest

import (
	"errors"
	"testing"
)

func TestProviderUnavailableMarkers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 恒可用", nil, false},
		{"codex 日配额（2026-09-04 实证）", errors.New("codex: turn failed: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 8:25 PM."), true},
		{"kimi 周配额 auth_error（同日实证）", errors.New("kimi: kimi 退出码 1：error: failed to run prompt: provider.auth_error: 403 You've reached your weekly (7-day) usage limit."), true},
		{"限流", errors.New("provider: rate limit exceeded"), true},
		{"配额泛措辞", errors.New("quota exceeded"), true},
		{"网络不可达（codex 实证）", errors.New("codex: turn failed: Reconnecting... waiting for network (Connection failed: error sending request)"), true},
		{"解析失败不命中", errors.New("kimi: 无 assistant 输出"), false},
		{"超时不命中", errors.New("codex: turn failed: context deadline exceeded"), false},
		{"发布门拒绝不命中", errors.New("agent: 正文为空"), false},
	}
	for _, c := range cases {
		if got := ProviderUnavailable(c.err); got != c.want {
			t.Errorf("%s: ProviderUnavailable = %v（want %v）", c.name, got, c.want)
		}
	}
}
