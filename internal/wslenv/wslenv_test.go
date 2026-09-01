// UT：WSL 执行面网络配置契约（真机复现 2026-09-01：宿主（Windows）无代理变量、
// 发行版内有——漏注入发行版侧值即意图评估 180s 超时拖死整轮）。
package wslenv

import (
	"reflect"
	"testing"
)

func TestSanitizeDropsNetKeysOnly(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/u", "CODEX_HOME=/home/u/.codex",
		"https_proxy=http://127.0.0.1:7890", "NO_PROXY=localhost",
	}
	got := Sanitize(env)
	want := []string{"PATH=/usr/bin", "HOME=/home/u", "CODEX_HOME=/home/u/.codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sanitize = %v, want %v", got, want)
	}
}

func TestMergeForWSLDistroSideWins(t *testing.T) {
	host := []string{
		"PATH=/bin", "HOME=/home/u",
		"https_proxy=http://host-side:1", "SSL_CERT_FILE=/host/ca.pem",
	}
	distro := []string{"https_proxy=http://127.0.0.1:7890", "HTTP_PROXY=http://127.0.0.1:7890"}
	got := MergeForWSL(host, distro)
	want := []string{
		"PATH=/bin", "HOME=/home/u",
		"https_proxy=http://127.0.0.1:7890", "HTTP_PROXY=http://127.0.0.1:7890",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeForWSL = %v, want %v（宿主网络键必须剥除、发行版侧值注入）", got, want)
	}
}

func TestParseEnvAllowlist(t *testing.T) {
	out := "PATH=/usr/bin\nhttps_proxy=http://127.0.0.1:7890\nno_proxy=\nbroken-line\nALL_PROXY=socks5h://127.0.0.1:7890\n"
	got := parseEnvAllowlist(out)
	want := []string{"https_proxy=http://127.0.0.1:7890", "ALL_PROXY=socks5h://127.0.0.1:7890"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseEnvAllowlist = %v, want %v（白名单外忽略、空值忽略、残行忽略）", got, want)
	}
}
