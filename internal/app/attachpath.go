// M4-0 附件路径注入（RFC-0013 OQ-1 负责人裁定 2026-09-07："保存到临时文件夹
// 传 PATH 给 Agent"——落地为注入附件本体绝对路径，不复制临时文件）。
//
// 安全口径：v1.52 负责人裁定三家 CLI 全权限运行（codex danger-full-access /
// mcode --permission full / kimi 无头内置语义），agent 作为同用户进程本就能读
// 数据目录（M1 的"位置隔离非强制隔离"登记不变）——路径注入不新增暴露面，
// 只把"能读"变成"知道去读"。
package app

import (
	"path/filepath"
	"strings"
)

// attachmentPathHint 附件路径提示行：绝对路径 + WSL 视图（agent 座位在
// 发行版内，Windows 盘符路径须转 /mnt/<drive>/ 形态才可达）。
func attachmentPathHint(dataDir, storagePath string) string {
	abs := filepath.Join(dataDir, filepath.FromSlash(storagePath))
	hint := "文件路径：" + abs
	if wsl, ok := winPathToWSL(abs); ok {
		hint += "（WSL 内：" + wsl + "）"
	}
	return hint + "——可用你的文件读取工具按路径查看（图像能否被查看取决于你 CLI 的工具能力）"
}

// winPathToWSL Windows 盘符路径 → WSL /mnt 视图（C:\Users\x → /mnt/c/Users/x）。
// 非盘符开头（UNC/相对/POSIX）返回 false——提示行只附可用形态。
func winPathToWSL(p string) (string, bool) {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return "", false
	}
	drive := p[0]
	if drive >= 'A' && drive <= 'Z' {
		drive += 'a' - 'A'
	} else if drive < 'a' || drive > 'z' {
		return "", false
	}
	return "/mnt/" + string(drive) + strings.ReplaceAll(p[2:], "\\", "/"), true
}
