//go:build st

// ST 层：M2 真实界面（v1.7 制度化）——真实二进制服务 SPA 产物：
// index 直出（含前端挂载点与模块入口）、静态资产可达且类型正确、
// /v1 命名空间不被前端路由回退吞掉。界面行为闭环（建房→讨论→续传→回放）
// 由 TestDiscussionLoop_ST（HTTP 契约）+ 验收 runbook（浏览器实录）覆盖。
package main_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSpaServed_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	index, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET / = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	idx := string(index)
	if !strings.Contains(idx, `id="root"`) {
		t.Fatal("SPA index 缺少前端挂载点 #root")
	}
	if !strings.Contains(idx, "const MOSAIC_DEV = true;") {
		t.Fatal("-dev 时 SPA index 应注入 MOSAIC_DEV=true（M1 机制沿用）")
	}

	// 模块入口资产可达且类型正确
	re := regexp.MustCompile(`src="(/assets/[^"]+\.js)"`)
	m := re.FindStringSubmatch(idx)
	if m == nil {
		t.Fatalf("SPA index 缺少模块入口：%s", idx)
	}
	asset, err := client.Get(base + m[1])
	if err != nil {
		t.Fatalf("GET %s: %v", m[1], err)
	}
	assetBody, _ := io.ReadAll(asset.Body)
	asset.Body.Close()
	if asset.StatusCode != 200 || !strings.Contains(asset.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("资产 %s = %d %q", m[1], asset.StatusCode, asset.Header.Get("Content-Type"))
	}
	if len(assetBody) < 1000 {
		t.Fatalf("资产 %s 体积异常（%d bytes）——疑似空构建", m[1], len(assetBody))
	}

	// 前端路由回退不吞 API 命名空间
	resp4, _ := client.Get(base + "/v1/nonexistent")
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/nonexistent = %d（API 命名空间不得被 SPA 回退吞掉）", resp4.StatusCode)
	}
}
