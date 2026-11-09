// bundlewiring_test.go —— 0.7.5 一审 #4：可执行项标识的路径脱敏投影。
package app

import (
	"strings"
	"testing"
)

func TestPortableExeID(t *testing.T) {
	cases := []struct {
		adapter, runtime, distro, want string
	}{
		// 注册表实形 exeID = "codex@wsl:openEuler-24.03:/home/u/.local/bin/codex"
		// ——投影只留 adapter@runtime[:distro]，路径段（用户名/目录）不随包。
		{"codex", "wsl", "openEuler-24.03", "codex@wsl:openEuler-24.03"},
		{"kimi", "native", "", "kimi@native"},
		{"zcode", "wsl", "", "zcode@wsl"},
	}
	for _, tc := range cases {
		got := portableExeID(tc.adapter, tc.runtime, tc.distro)
		if got != tc.want {
			t.Fatalf("portableExeID(%q,%q,%q) = %q want %q", tc.adapter, tc.runtime, tc.distro, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, ":/") {
			t.Fatalf("投影含路径分隔符: %q", got)
		}
	}
}
