// HostRunner：真实探测执行面。
// native：宿主直接 exec；wsl：Windows 宿主经 wsl.exe -d <distro> -- 在发行版内执行
// （wsl.exe --list 输出为 UTF-16LE，需解码——Windows 侧已知怪癖）。
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode/utf16"
)

// HostRunner 生产实现。
type HostRunner struct{}

// NewHostRunner 构造。
func NewHostRunner() *HostRunner { return &HostRunner{} }

func (h *HostRunner) command(ctx context.Context, runtime Runtime, distro string, args []string) *exec.Cmd {
	if runtime == RuntimeWSL {
		argv := []string{"-d", distro, "--"}
		argv = append(argv, args...)
		return exec.CommandContext(ctx, "wsl.exe", argv...)
	}
	return exec.CommandContext(ctx, args[0], args[1:]...)
}

// Run 执行命令，返回合并输出与退出码；命令无法启动视为 err。
func (h *HostRunner) Run(ctx context.Context, runtime Runtime, distro string, args []string) (string, int, error) {
	cmd := h.command(ctx, runtime, distro, args)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return decodeOutput(runtime, buf.Bytes()), ee.ExitCode(), nil
		}
		return decodeOutput(runtime, buf.Bytes()), -1, err
	}
	return decodeOutput(runtime, buf.Bytes()), 0, nil
}

// LookPath 解析二进制：native 先 exec.LookPath，未命中再走登录 shell PATH 兜底
// （nvm/fnm/asdf 等版本管理器只在 shell 初始化时注入 PATH——真实用户环境的常态）；
// wsl 用 sh -lc command -v（同理覆盖发行版内的版本管理器安装）。
func (h *HostRunner) LookPath(ctx context.Context, runtime Runtime, distro, binary string) (string, bool) {
	quote := shellQuote(binary)
	if runtime == RuntimeNative {
		if path, err := exec.LookPath(binary); err == nil {
			return path, true
		}
		var args []string
		if goruntime.GOOS == "windows" {
			args = []string{"cmd", "/c", "where " + binary}
		} else {
			args = []string{"sh", "-lc", "command -v " + quote}
		}
		out, code, err := h.Run(ctx, RuntimeNative, "", args)
		if err != nil || code != 0 {
			return "", false
		}
		path := firstLine(out)
		return path, path != ""
	}
	out, code, err := h.Run(ctx, runtime, distro, []string{"sh", "-lc", "command -v " + quote})
	if err != nil || code != 0 {
		return "", false
	}
	path := strings.TrimSpace(out)
	return path, path != ""
}

// Home 目标运行时家目录。
func (h *HostRunner) Home(ctx context.Context, runtime Runtime, distro string) string {
	if runtime == RuntimeNative {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.ToSlash(home)
	}
	out, code, err := h.Run(ctx, runtime, distro, []string{"sh", "-c", "printf %s \"$HOME\""})
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// MkdirAll 在目标运行面递归建目录（复审 #2：WSL 座位的工作目录是发行版内
// Linux 路径，宿主侧 MkdirAll 建不出发行版内的目录；native 直接 os.MkdirAll）。
func (h *HostRunner) MkdirAll(ctx context.Context, runtime Runtime, distro, dir string) error {
	if runtime == RuntimeNative {
		return os.MkdirAll(dir, 0o700)
	}
	_, code, err := h.Run(ctx, runtime, distro, []string{"mkdir", "-p", dir})
	if err != nil || code != 0 {
		return fmt.Errorf("harness: mkdir -p %s: %v (code=%d)", dir, err, code)
	}
	return nil
}

// Exists 文件存在性：native os.Stat；wsl test -e。
func (h *HostRunner) Exists(ctx context.Context, runtime Runtime, distro, path string) bool {
	if runtime == RuntimeNative {
		_, err := os.Stat(path)
		return err == nil
	}
	_, code, err := h.Run(ctx, runtime, distro, []string{"test", "-e", path})
	return err == nil && code == 0
}

// Digest 摘要：native 本地 sha256；wsl sha256sum。
func (h *HostRunner) Digest(ctx context.Context, runtime Runtime, distro, path string) (string, error) {
	if runtime == RuntimeNative {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	out, code, err := h.Run(ctx, runtime, distro, []string{"sha256sum", path})
	if err != nil || code != 0 {
		return "", fmt.Errorf("harness: wsl sha256sum: %v", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("harness: wsl sha256sum 输出为空")
	}
	return "sha256:" + fields[0], nil
}

// WSLDistros 列出 WSL 发行版；非 Windows 宿主返回空。
func (h *HostRunner) WSLDistros(ctx context.Context) []string {
	if goruntime.GOOS != "windows" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "wsl.exe", "--list", "--quiet")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	text := decodeWSLOutput(out)
	var distros []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			distros = append(distros, line)
		}
	}
	return distros
}

// decodeOutput wsl.exe 路径的输出可能为 UTF-16LE，native 为 UTF-8。
func decodeOutput(runtime Runtime, raw []byte) string {
	if runtime == RuntimeWSL && looksUTF16(raw) {
		return decodeWSLOutput(raw)
	}
	return string(raw)
}

// looksUTF16 粗判：大量 NUL 字节且偶数长度。
func looksUTF16(raw []byte) bool {
	if len(raw) < 2 || len(raw)%2 != 0 {
		return false
	}
	nul := 0
	for i := 1; i < len(raw); i += 2 {
		if raw[i] == 0 {
			nul++
		}
	}
	return nul > len(raw)/4
}

// decodeWSLOutput UTF-16LE → UTF-8。
func decodeWSLOutput(raw []byte) string {
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u16 = append(u16, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(u16))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wsl.exe 会在目标 sh -c 前先做一层 shell 展开；反斜杠让变量延迟到目标 shell。
// glob 模式作为尾参数传入，外层可展开为多项，目标 shell 通过 "$@" 全量枚举。

func wslGlobArgs(pattern string) []string {
	return []string{
		"sh",
		"-c",
		`for p in "\$@"; do [ -e "\$p" ] && printf "%s\n" "\$p"; done`,
		"--",
		pattern,
	}
}

func wslRunWithDirArgs(binDir string, args []string) []string {
	wrapped := []string{
		"sh",
		"-c",
		`export PATH=` + shellQuote(binDir) + `:\$PATH; exec "\$@"`,
		"--",
	}
	return append(wrapped, args...)
}

// Glob 展开通配模式：native filepath.Glob；wsl 用 shell glob（仅保留存在项）。
func (h *HostRunner) Glob(ctx context.Context, runtime Runtime, distro, pattern string) []string {
	if runtime == RuntimeNative {
		matches, _ := filepath.Glob(pattern)
		return matches
	}
	out, code, err := h.Run(ctx, runtime, distro, wslGlobArgs(pattern))
	if err != nil || code != 0 {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// RunWithDir 在 binDir 前置 PATH 后执行：native 注入 cmd.Env；
// wsl 用 sh -c 'export PATH=…; exec "$@"' -- 参数透传。
func (h *HostRunner) RunWithDir(ctx context.Context, runtime Runtime, distro, binDir string, args []string) (string, int, error) {
	if binDir == "" {
		return h.Run(ctx, runtime, distro, args)
	}
	if runtime == RuntimeNative {
		cmd := h.command(ctx, runtime, distro, args)
		cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Start(); err != nil {
			return "", -1, err
		}
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return decodeOutput(runtime, buf.Bytes()), ee.ExitCode(), nil
			}
			return decodeOutput(runtime, buf.Bytes()), -1, err
		}
		return decodeOutput(runtime, buf.Bytes()), 0, nil
	}
	return h.Run(ctx, runtime, distro, wslRunWithDirArgs(binDir, args))
}
