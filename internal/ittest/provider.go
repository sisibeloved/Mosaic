// Package ittest 真实 CLI IT 用例共享判定：供应商侧不可用（配额耗尽/鉴权
// 失败/限流）时用例降级跳过。负责人裁定 2026-09-04："在真实场景碰到配额
// 耗尽怎么处理？应该是降级，直接断言成失败跟使用场景不符。"
//
// 产品侧降级语义已存在：引擎对评估/生成失败的座位按波跳过（不阻塞其他
// 座位、不中断房间），本包只把测试侧对齐到同一现实——供应商不可用时
// conformance 无法验证，是环境态不是代码缺陷。
//
// 判定刻意保守：只认供应商拒绝的明确措辞；适配器自身缺陷（解析失败/
// 发布门拒绝/argv 错误/超时）仍按失败断言，不得借道跳过掩盖回归。
package ittest

import (
	"strings"
	"testing"
)

// providerUnavailableMarkers 供应商侧不可用的错误措辞（小写子串匹配）。
// 实证来源（2026-09-04）：codex "You've hit your usage limit. Visit … to
// purchase more credits"；kimi "error: failed to run prompt:
// provider.auth_error: 403 You've reached your weekly (7-day) usage limit."；
// codex 网络不可达 "Reconnecting... waiting for network (Connection failed:
// error sending request)"（同晚国际路由抖动实录——与配额同类：环境态）。
// 模型版本门（2026-09-08 实证）：codex "The 'gpt-6-astra' model requires a
// newer version of Codex. Please upgrade to the latest app or CLI and try
// again."——ambient CLI 配置指向的模型被上游按 CLI 版本拒收，环境态非代码缺陷，
// 同裁定降级跳过。
// 模型运行时无最终回复（2026-09-17 实证）：mcode "Runtime completed without
// a final assistant response."（流内 error 事件携 retryable:true）——MiniMax-M3
// thinking 变体在意图评估类提示下跑了 token 却不产出最终 assistant 消息；
// 新旧提示词对照实验排除适配器/提示词侧代码态（generate 任务同日正常），
// CLI 侧安装损坏（chunks 缺模块）同日另证并已重装修复。
var providerUnavailableMarkers = []string{
	"usage limit",
	"rate limit",
	"quota",
	"auth_error",
	"insufficient credits",
	"purchase more credits",
	"subscription required",
	"waiting for network",
	"connection failed",
	"error sending request",
	"requires a newer version",
	"runtime completed without a final assistant response",
	// 登录态过期（2026-09-23 实证）：mcode 退出码 3、stdout 全空、真因只在
	// stderr（"Sign in to MiniMax to use Agent features. Run `mcode login`"）。
	// IT 的登录门只查 ~/.minimax/cli-auth 目录存在——目录在而 token 已死，
	// 过期与未登录同态（鉴权不可用），按本包纪律降级跳过。
	"sign in to minimax",
	// zcode（2026-09-23 实证 0.16.9）：provider 配置指向空/失效 provider 文件时
	// headless 报 "Model creation failed"（模型实例建不起来——provider 侧不可用，
	// 环境态）；无头面缺权限审批客户端时报 "No permission client configured"
	//（交互宿主缺失，环境态非代码缺陷——同 mcode smart 档 HOST_CAPABILITY_UNAVAILABLE
	// 一类）。IT 登录门只查双凭证文件存在——凭证在而 provider 失效同态降级。
	"model creation failed",
	"no permission client configured",
}

// ProviderUnavailable 报告 err 是否为供应商侧不可用（nil 恒 false）。
func ProviderUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range providerUnavailableMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// SkipIfProviderUnavailable err 为供应商侧不可用时跳过当前用例（t.Skipf
// 即 Goexit）并返回 true；其余情况返回 false 由调用方断言失败。
func SkipIfProviderUnavailable(t *testing.T, stage string, err error) bool {
	t.Helper()
	if !ProviderUnavailable(err) {
		return false
	}
	t.Skipf("%s: 供应商侧不可用（配额/鉴权/限流），降级跳过——产品侧行为为按座位降级，非用例失败面：%v", stage, err)
	return true
}
