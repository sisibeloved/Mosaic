# 平台实证笔记（platform-notes）

- 目的：**实证坑位入档**（交付规划 §5.2 要求 M1 建立）——只收录"本机/CI 实测踩过"的平台事实，不收录推测；每条附验证场景与结论。
- 治理：新坑位随修复/发现追加；条目过时（上游修复或方案废弃）划掉并注明，不删除。
- 上游：[Harness 调研](../design/research/2026-08-25-harness-survey.md)（CLI 侧实证）、[ADR-0008](../design/adr/ADR-0008-personal-storage-sqlite.md)（SQLite 机制映射）。

## Windows

| # | 坑位 | 实证场景 | 结论/对策 |
|---|---|---|---|
| W-1 | `wsl.exe` 自身诊断输出（如 `-l` 列表）是 UTF-16LE，经命令透传的 stdout 是原样字节 | M1 切片 D：WSL 发行版扫描 | 列表类输出按 UTF-16LE 解码；命令透传输出直接按字节消费（JSONL 无损） |
| W-2 | 登录 shell 的 PATH 注入跨 `wsl.exe` 边界不可靠；nvm/fnm/volta 把 CLI 装在版本化目录 | M1 切片 D：codex 发现 | 发现走**文件系统事实**（KnownDirGlobs glob），不依赖 shell 初始化 |
| W-3 | Windows 侧击杀 `wsl.exe` 不必然终止发行版内进程 | M1 收口：wslExecer 设计 | 任务级超时内自行退出为主路径；Job Object 归 M2 进程管理 |
| W-4 | WSL2 内 `127.0.0.1` 不是 Windows 宿主回环——代理透传在 NAT 模式 WSL 内失效可能 | M1 收口：代理环境白名单 | 代理变量照传（网络配置非凭据，OQ-20 允许）；mirrored 网络模式或宿主 IP 由用户网络环境决定，不代填 |
| W-5 | Windows 无 POSIX 进程组；超时击杀只能杀直接子进程 | M1 切片 E：codex exec | `WaitDelay=10s` 放弃残留 IO 防 Wait 永挂；Job Object 归 M2（`sysproc_windows.go` 空实现为登记点） |

## macOS

| # | 坑位 | 实证场景 | 结论/对策 |
|---|---|---|---|
| M-1 | CI 双核 runner 上信号注册晚于 listening 日志 → ST 投递 SIGINT 走默认动作非零退出 | M1 切片 B：优雅退出 ST（f6d0672） | `signal.NotifyContext` 必须在一切监听/日志之前注册 |
| M-2 | macOS amd64 曾缺 CI 覆盖（§5.1 要求 arm64+amd64）；补 `macos-13`（Intel）leg 后**实测排队 30min+ 无 runner**——GitHub 托管 Intel macOS runner 已退役（macos-13 是最后一档 Intel 镜像） | M1 收口：CI matrix 实验（run 33234508894，三腿全绿、macos-13 恒 queued） | darwin/amd64 兼容改由 ubuntu 腿**交叉编译门禁**把守（GOOS=darwin GOARCH=amd64/arm64 + windows 双架构 build）；Intel mac 真机运行验证无上游 runner 可用，若需真机只能自托管（登记不阻塞主线） |

## Linux（WSL/dev 基线）

| # | 坑位 | 实证场景 | 结论/对策 |
|---|---|---|---|
| L-1 | codex 的孙进程在超时击杀直接子进程后仍握管道，`Wait()` 永挂 | M1 切片 E：超时路径 | POSIX `Setpgid` + `cmd.Cancel=kill(-pid, SIGKILL)` 整组击杀 + `WaitDelay=10s` |
| L-2 | `codex login status` 的输出走 stderr | M1 切片 E：登录探测 | 探测命令 stdout+stderr 合并缓冲后匹配 |
| L-3 | codex（node 脚本）需要同目录 node；`exec.Command` 以父 PATH 解析裸命令名 | M1 切片 E | 用全路径 + 子进程 `PATH` 前置 bin 目录 |
| L-4 | modernc SQLite 的 per-connection pragma 经 `db.Exec` 只命中当时连接，并发立刻 SQLITE_BUSY | M0 spike：8×5 并发追加 | pragma 全部走 DSN（`_pragma=busy_timeout(5000)` 等） |
| L-5 | 跨 `wsl.exe` 的交互式 shell（heredoc/单字符变量）在 Git Bash 侧被吞/展开 | 开发流程（本仓库） | 脚本走文件传递，避免 heredoc；WSL 内命令用 `bash -lc` 包裹 |
| L-6 | `--output-schema` 在 ChatGPT 账号 provider 组合下上游 400（text.format.schema 序列化 bug，codex-cli 0.149.1） | M1 切片 E：结构化输出 | 提示词约束 + 本地提取校验（`ExtractJSON`）；上游修复后重评估 |
