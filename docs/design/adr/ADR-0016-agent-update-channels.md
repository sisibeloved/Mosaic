# ADR-0016：Agent 更新通道三分类——registry / self-update / mosaic

- 状态：Accepted（2026-10-27，负责人裁定："现在是三种更新通道：1. 第三方分发仓库，包括但不限于 npm；2. 自更新，包括 claude、kimi、mcode 等，本质是一样的，只是各家提供的参数和输出各有差异；3. Mosaic 自带更新通道，主要是 zcode，依赖于我们的打包脚本。这三种得梳理清楚并落到文档中，包括代码实现也需要按这三种抽象，便于后续拓展到其它的 agent"）
- 关联：交付计划 v1.92–v1.97（0.4.1 版本检测与更新）、ADR-0013（稳定 Bot 身份）、harness 注册表（`internal/harness/registry.go`）

## 背景与问题

0.4.1 的版本检测与更新按家族逐个实证落地后，通道词汇随迭代膨胀出四个杂项（npm / self-update / releases / manual）——按**实现细节**而非**机制类型**分类：releases 实为自建分发的信号面、manual 实为"更新面暂缺"。新增 agent 时无从按类型对号入座，zcode（自建打包）的更新面也没有归位。负责人裁定按更新机制归为三类，代码与文档同构。

## 决策

### 三种通道类型

| 通道 | 机制 | 检测面（最新版） | 更新面（执行） | 现役家族 |
|---|---|---|---|---|
| **registry** | 第三方分发仓库 | 仓库客户端查询（npm view） | 仓库客户端安装（npm install -g pkg@latest） | codex |
| **self-update** | CLI 官方自更新子命令 | 可借其仓库分发查最新（claude/mcode 有 npm 面）；无仓库面如实呈现"无独立查询面"（kimi） | 执行家族子命令（`claude update` / `kimi upgrade --yes` / `mcode update`——参数/输出各有差异，机制同一） | claude、kimi、minimax |
| **mosaic** | Mosaic 自带更新通道（自建分发 CLI） | 自建 releases 信号：产品更新源（桌面 app-update.yml → feed latest.yml）优先，不可达回退源码仓 tag（git ls-remote） | 驱动用户提供的更新脚本 `$HOME/.mosaic/update/<adapter>.sh`（拉源码 + 自有打包管线 + 安装）；脚本缺位 = 无更新面（指引说明约定路径） | zcode |

要点：

- **检测面与更新面正交**：self-update 家族可借 registry 面查最新版（claude/mcode），mosaic 的检测面（releases 信号）独立于更新脚本在位与否。
- **通道 = 更新机制的类型**，不再按包管理器/信号源命名。原 npm → registry（现役实现），原 releases/manual → mosaic（信号面归检测、脚本归更新）。
- **mcode 归 self-update**（负责人裁定口径 + `mcode update` 子命令实证 "Check for and install a Minimax Code update"，2026-10-27）——npm 面仅作检测。

### 代码抽象（`internal/harness/update.go`）

```go
// registry 通道执行面：仓库客户端三方法（新仓库 = 新实现，家族配置换绑）
type packageRepo interface {
    latestCmd(pkg string) string   // 登录 shell 内的查询命令
    installCmd(pkg string) string  // 登录 shell 内的安装/更新命令
    parseLatest(out string) string
}
type npmRepo struct{} // 现役唯一实现；brew/apt/pip 等为后续兄弟实现

// 家族配置：三选一 channel + 各面参数
type familySpec struct {
    channel  string      // registry | self-update | mosaic
    repo     packageRepo // registry：检测+安装；self-update：可兼作检测
    pkg      string      // 仓库包名
    selfArgs []string    // self-update：升级子命令
    gitRepo  string      // mosaic：自建 releases 信号仓
    note     string      // 无检测面/无自动面时的指引
}
var updateFamilies = map[string]familySpec{ … }
```

- 检测（CheckUpdate）/ 更新命令构造（buildUpdateCmd）按 `channel` 三分派；过程可视化任务（StartUpdate/SSE 逐行转发）通道无关——四种执行形态（仓库安装/自更新/mosaic 脚本/版本回探）同一管线。
- 全部命令经登录 shell（`bash -lc`；Windows native `cmd /c`）——npm 需 nvm PATH、镜像源随 npmrc（实证见 update.go 包注释）。
- API 通道枚举收拢为三值：`registry | self-update | mosaic`。

### 扩展指引（新 agent 接入）

1. 按分发形态选通道类型：有官方仓库包 → registry（或 self-update + 仓库检测）；有升级子命令 → self-update；自建打包 → mosaic。
2. 在 `updateFamilies` 加一行配置（参数实证后落注释）——检测/更新/可视化管线零新代码。
3. 新仓库生态（brew/apt/pip）→ 实现 `packageRepo` 三方法；新自建分发的信号源 → 在 mosaic 检测面内按优先级扩展（feed → tag → …）。
4. mosaic 家族的更新脚本约定：`$HOME/.mosaic/update/<adapter>.sh`，须可执行；脚本内容 = 该家族的自有打包管线（拉源码、构建、安装到 PATH 可见位），Mosaic 以登录 shell 驱动并逐行转发输出。

## 后果与放弃方案

- 放弃按实现细节命名的四杂项枚举（npm/releases/manual）——0.4.1 未发布，无兼容包袱；API 枚举一次收拢。
- mosaic 脚本约定是**用户侧契约**（放置即用），不做设置界面/注册表配置面——自建管线的参数（源码仓、打包选项）属脚本内部事务，Mosaic 不感知；若未来多脚本/多参数需求出现再评估配置面。
- zcode 检测的"产品更新源优先、源码 tag 回退"语义不变（v1.95 裁定）：feed 是自建 infra（离线常态），note 始终标注信号来源与"产品通道可能领先"。
- 版本方案不可比不判更新（comparable 语义，v1.94）通道无关，保持。
