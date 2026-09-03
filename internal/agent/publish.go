package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// 发布安全门（二轮审校 #4；复审 #6/#9；四轮复审 #7）：不可信模型输出在进入事件日志前
// 必过的边界——原属 adapter/codex 私有，adapter/kimi 晋级（M2 C 轨）时上移为端口级共享，
// 两家适配器与后续适配器共用同一套门（DLP 规则库单一事实源）。

// SanitizeBody 剔除控制字符（保留换行/制表）并去首尾空白。
func SanitizeBody(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// secretPatterns DLP 秘密形状（RFC-0002 §3.1：草稿/发布需过 secret scan）。
// M1 起为形状启发式代理门；规则库与误报反馈随 M2 secret-scan 项演进。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/-]{12,}={0,2}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

// RedactSecrets 秘密形状子串整体替换为 [REDACTED]（发布正文不得携带凭据形状）。
func RedactSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// SanitizeRelations 关系引用白名单化：只保留经过正文同款安全门的字符串项
// （控制字符剔除 + DLP + 单项 200 runes），非字符串项丢弃，至多 8 项——
// 模型不得经 declared_relations 侧漏私货。
func SanitizeRelations(v any) []any {
	rels, _ := v.([]any)
	cleaned := make([]any, 0, len(rels))
	for _, item := range rels {
		s, ok := item.(string)
		if !ok {
			continue // 非字符串项（对象/数字等）不得混入事件载荷
		}
		s = RedactSecrets(SanitizeBody(s))
		if s == "" {
			continue
		}
		if runes := []rune(s); len(runes) > 200 {
			s = string(runes[:200])
		}
		cleaned = append(cleaned, s)
		if len(cleaned) >= 8 {
			break
		}
	}
	return cleaned
}

// PublishGate 发布边界总成（generate 任务产出进事件日志前必过）：
// 正文控制字符剔除 → DLP 剔除 → 去首尾空白 → 空正文判失败。v1.38（负责人裁定）
// 移除 rune 截断：token 成本只做统计，绝不影响主线能力——原文照发；超常输出由
// 任务超时（生成时长上限）天然有界，防复读/注入的兜底也是时长而非长度。
// 显式标注；declared_relations 逐项过同一套门。
func PublishGate(body string, relations any) (string, []any, error) {
	body = RedactSecrets(SanitizeBody(body))
	if body == "" {
		return "", nil, fmt.Errorf("agent: 发布正文为空（拒绝发布）")
	}
	return body, SanitizeRelations(relations), nil
}
