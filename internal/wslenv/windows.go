//go:build windows

package wslenv

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/winhide"
)

var cache sync.Map // distro → []string（成功读取才缓存）

// NetEnv 读取发行版登录环境的网络配置（白名单键），按 distro 缓存。
// 读取走 bash -l（用户代理配置多在 shell profile）；失败/超时不缓存、返回 nil
// （下次再试——探测失败不阻塞执行面，代价是退回无代理直连）。
func NetEnv(distro string) []string {
	if distro == "" {
		return nil
	}
	if v, ok := cache.Load(distro); ok {
		return v.([]string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "bash", "-l", "-c", "env")
	var out bytes.Buffer
	cmd.Stdout = &out
	winhide.Hide(cmd) // 不闪控制台窗口（桌面壳）
	if err := cmd.Start(); err != nil {
		return nil
	}
	if err := cmd.Wait(); err != nil {
		return nil
	}
	env := parseEnvAllowlist(out.String())
	if env != nil {
		cache.Store(distro, env)
	}
	return env
}
