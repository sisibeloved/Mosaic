// 跨平台共用面：白名单键与宿主环境净化。Windows 变体见 windows.go，其余平台 other.go。
package wslenv

import "strings"

// Keys 网络配置白名单（代理与 CA；与适配器 codexEnv/kimiEnv 透传口径一致）。
var Keys = []string{
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
}

func isNetKey(key string) bool {
	for _, k := range Keys {
		if key == k {
			return true
		}
	}
	return false
}

// Sanitize 从宿主构建的环境列表中剥除网络键——WSL 执行面不得携带宿主侧值
// （loopback 语义不同；发行版侧值由 NetEnv 提供）。
func Sanitize(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && isNetKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// MergeForWSL WSL 执行面环境合并（UT 钉住契约——漏注入发行版侧网络配置即
// 真机 180s 超时拖死整轮，2026-09-01 复现）：剥宿主网络键，拼发行版侧值。
func MergeForWSL(env, distroNet []string) []string {
	return append(Sanitize(env), distroNet...)
}

// parseEnvAllowlist 从 `env` 输出解析白名单键（跨平台纯函数，Windows NetEnv 使用）。
func parseEnvAllowlist(output string) []string {
	var env []string
	for _, line := range strings.Split(output, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && isNetKey(key) && val != "" {
			env = append(env, key+"="+val)
		}
	}
	return env
}
