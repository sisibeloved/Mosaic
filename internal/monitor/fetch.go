// monitor 包文件：快照取回（脚本/URL）与行级差异。
package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// 取回界限。
const (
	fetchTimeout = 30 * time.Second
	fetchCap     = 1 << 20 // 1MiB 快照读取上限
)

// FetchScript 脚本快照：绝对路径 + 参数直接执行（无 shell——同用户信任面，
// v1.52 全权限裁定下的既定边界）；超时与输出上限约束失控。
func FetchScript(ctx context.Context, src Source) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, src.Target, src.Args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("脚本退出非零：%s", truncateStr(string(ee.Stderr), 200))
		}
		return "", fmt.Errorf("脚本执行失败：%w", err)
	}
	if len(out) > fetchCap {
		out = out[:fetchCap]
	}
	return string(out), nil
}

// FetchURL URL 快照：GET、超时、1MiB 上限、2xx 才算成功（重定向遵默认策略——
// 本地单用户登记自己的源）。
func FetchURL(ctx context.Context, src Source) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, src.Target, nil)
	if err != nil {
		return "", fmt.Errorf("URL 非法：%w", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchCap+1))
	if err != nil {
		return "", fmt.Errorf("读取失败：%w", err)
	}
	if len(body) > fetchCap {
		body = body[:fetchCap]
	}
	return string(body), nil
}

// FetchOf 按源类型分派的取回器。
func FetchOf(ctx context.Context, src Source) (string, error) {
	if src.Kind == KindURL {
		return FetchURL(ctx, src)
	}
	return FetchScript(ctx, src)
}

// DiffLines 行级差异（朴素 LCS，≤500 行输入——快照通常远小；超出退化为
// 头尾摘要）。输出统一 diff 形态：- 旧行 / + 新行，公共行省略。
func DiffLines(oldContent, newContent string) string {
	if oldContent == "" {
		return "+（首次观察，无旧快照）\n" + truncateStr(newContent, 2000)
	}
	old := splitLines(oldContent)
	new := splitLines(newContent)
	if len(old) > 500 || len(new) > 500 {
		var b strings.Builder
		fmt.Fprintf(&b, "-（旧快照 %d 行，头 20 行）\n", len(old))
		for _, l := range old[:min(20, len(old))] {
			fmt.Fprintf(&b, "-%s\n", l)
		}
		fmt.Fprintf(&b, "+（新快照 %d 行，头 20 行）\n", len(new))
		for _, l := range new[:min(20, len(new))] {
			fmt.Fprintf(&b, "+%s\n", l)
		}
		return b.String()
	}
	// LCS 表（len+1)^2
	lcs := make([][]int, len(old)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(new)+1)
	}
	for i := len(old) - 1; i >= 0; i-- {
		for j := len(new) - 1; j >= 0; j-- {
			if old[i] == new[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < len(old) && j < len(new) {
		switch {
		case old[i] == new[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b, "-%s\n", old[i])
			i++
		default:
			fmt.Fprintf(&b, "+%s\n", new[j])
			j++
		}
	}
	for ; i < len(old); i++ {
		fmt.Fprintf(&b, "-%s\n", old[i])
	}
	for ; j < len(new); j++ {
		fmt.Fprintf(&b, "+%s\n", new[j])
	}
	out := b.String()
	if out == "" {
		out = "（哈希变化但行级无差异——可能行尾/空白）"
	}
	return out
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
