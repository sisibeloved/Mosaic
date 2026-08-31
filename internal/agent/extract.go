package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractJSON 从模型文本提取 JSON 对象：容忍围栏与前后散文；无 JSON 报错。
// 端口级共享（原 codexadapter 私有，M2 C 轨上移供 kimiadapter 等 CLI 适配器共用）：
// 各适配器的结构化输出都走"提示词约束 + 本地提取校验"路线时，提取语义必须一致。
func ExtractJSON(text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if s := stripFence(text); s != "" {
		text = s
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("agent: 文本中无 JSON 对象")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &data); err != nil {
		return nil, fmt.Errorf("agent: JSON 解析失败: %w", err)
	}
	return data, nil
}

func stripFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return ""
	}
	lines := strings.Split(text, "\n")
	var body []string
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "```" {
			continue
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}
