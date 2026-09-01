// Package wslenv：WSL 发行版侧网络配置读取（Windows 宿主 → 发行版执行面）。
//
// 实证（2026-09-01 真机复现）：Windows 宿主环境没有 https_proxy（WSL 里才有
// 127.0.0.1:7890 出网配置）；适配器 env -i 白名单从宿主透传 → 发行版内 codex
// 直连被墙 → 每次意图评估干等 180s 超时。宿主的代理值对发行版同样无意义
// （loopback 指向不同网络栈）——网络配置必须取自发行版自身登录环境，
// 经白名单过滤后注入（白名单与适配器 codexEnv/kimiEnv 同口径；OQ-20：网络
// 配置不是凭据）。非 Windows 平台全部 no-op。
package wslenv
