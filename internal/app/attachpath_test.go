// UT：附件路径注入——盘符转换与提示行组合（OQ-1 裁定 2026-09-07）。
package app

import (
	"strings"
	"testing"
)

func TestWinPathToWSL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`C:\Users\luchen\AppData\Roaming\mosaic\attachments\rooms\r\a`, "/mnt/c/Users/luchen/AppData/Roaming/mosaic/attachments/rooms/r/a", true},
		{`D:\data\file.png`, "/mnt/d/data/file.png", true},
		{`c:\lower\case`, "/mnt/c/lower/case", true}, // 小写盘符归一
		{`/home/luchen/file`, "", false},             // POSIX
		{`\\server\share`, "", false},                // UNC
		{`relative\path`, "", false},                 // 相对
		{`1:\not-drive`, "", false},                  // 非字母盘符
	}
	for _, c := range cases {
		got, ok := winPathToWSL(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("winPathToWSL(%q) = %q,%v（want %q,%v）", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAttachmentPathHint(t *testing.T) {
	// Windows 盘符数据目录：绝对路径 + WSL 视图双形态（断言平台无关——
	// filepath.Join 在 POSIX 测试环境下用 / 拼接，盘符路径仍可识别转换）
	hint := attachmentPathHint(`C:\Users\u\AppData\Roaming\mosaic`, "attachments/rooms/room_x/att_abc")
	if !strings.Contains(hint, "文件路径：") || !strings.Contains(hint, "att_abc") {
		t.Errorf("应含绝对路径: %q", hint)
	}
	if !strings.Contains(hint, "/mnt/c/") || !strings.Contains(hint, "/attachments/rooms/room_x/att_abc") {
		t.Errorf("应含 WSL 视图: %q", hint)
	}
	if !strings.Contains(hint, "文件读取工具") {
		t.Errorf("应含工具提示: %q", hint)
	}

	// POSIX 数据目录（WSL 侧运行 mosaic-server 的形态）：只绝对路径，无双形态
	hint2 := attachmentPathHint("/home/u/.local/mosaic", "attachments/rooms/r/a")
	if !strings.Contains(hint2, "/home/u/.local/mosaic/attachments/rooms/r/a") || strings.Contains(hint2, "/mnt/") {
		t.Errorf("POSIX 目录不应附 WSL 视图: %q", hint2)
	}
}
