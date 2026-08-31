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
| W-6 | Windows 无 POSIX 权限语义：Go `os.Chmod` 只拨只读位，`0700/0600` 收敛在 NTFS 上是 no-op | 三轮复审 #17：数据目录/DB 文件 owner-only 收紧 | Unix 侧 chmod 生效（DB 文件 0600 + 目录 0700 补收紧，IT 钉在 `TestOpenTightensExistingDirectoryPerms`）；Windows 侧依赖数据目录落在用户 profile（默认 user-only ACL），显式 ACL 收敛（icacls/x_sys）登记 M2 |
| W-7 | Codex/ChatGPT 桌面应用（MSIX 商店包）把 codex.exe bundled 在 `WindowsApps\OpenAI.Codex_*\app\resources` 下；与独立 CLI 安装是**配置、登录态、会话各自独立的实例**，不是同一安装的两个入口 | M2 C 轨多实例发现设计（2026-08-31）；路径形状据上游 issue（openai/codex #40700/#41059）归档，本机未装该应用、未真机实证 | 发现挂 AppGlobs、仅 native 面展开（WSL 发行版内无 Windows 应用概念）；命中标 `app:codex-desktop` 渠道、家族优先级先于 CLI（负责人裁定）；`WindowsApps` 目录 ACL 限制枚举属预期，glob 失败静默跳过；路径形状待真机实证复核，不符则改 glob 不动机制 |
| W-8 | kimi-desktop（Electron）安装目录只有 `kimi-webbridge.exe` / `kimiim-cli.exe`，**无可驱动 CLI 面** | M2 C 轨本机 Windows 侧 `%LOCALAPPDATA%\Programs\kimi-desktop` 实查（2026-08-31） | kimi spec 不挂 AppGlobs；Kimi Work 实例经手动登记（`AddManual` 带 `channel=app:kimi-work`）进入优先级体系，Kimi Code 优先（两者独立计费，负责人裁定） |

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
| L-7 | `codex exec resume` 子命令不接受 `-C`（exit 2 "unexpected argument"）——工作目录参数只能在会话首轮 exec 传入 | 二轮审校 #18：WorkDir 隔离引入后由生产路径 ST 抓到（生成全失败、错误信息只落在非 JSON 行） | `-C` 仅首轮；回归钉在 `TestWorkDirResumeArgvShape`；子进程错误行建议适配器日志化（M2 backlog） |
| L-8 | `wsl.exe -d <distro> -- env K=V ... cmd` 会携带发行版默认环境（shell profile 注入 + WSLENV 透传），白名单外变量泄漏给发行版内进程 | 三轮复审 #7：环境白名单边界 | 前缀改 `env -i` 清空后仅显式注入（PATH/HOME/CODEX_HOME/网络配置）；argv 形状钉在 `TestWSLArgsConstruction` |
| L-9 | `kimi -p -` **不**把 `-` 当 stdin 标记——headless 提示词只能走 argv，无 stdin 通道 | M2 C 轨 kimiadapter 实测（kimi 0.39.1，2026-08-31） | prompt 经 argv 传入 + `MaxPromptRunes` 护栏（默认 6000 runes，超限 fail-closed 报错由 Mosaic 侧截断/分轮），全程不拼 shell 字符串 |
| L-10 | kimi `--output-format stream-json` 行流只三类：`meta system.version` / `assistant` content / `meta session.resume_hint`（含 `session_id` 与 `kimi -r` 恢复命令）；**无 usage 面**；`-p` 与 `-S <session_id>` 可组合恢复；`-p` 模式默认 auto 权限且不可与 `--auto`/`--yolo`/`--plan` 组合 | 同上；fixtures 钉 0.39.1 真实捕获（intent/generate 两场景 + manifest sha256 门禁） | 适配器 `UsageReporting=false` 诚实上报；会话句柄取 resume_hint 的 `session_id`；权限形态由适配器固定默认、不暴露组合标志 |
