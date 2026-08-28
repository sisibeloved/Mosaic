# ADR-0002：Web 前端采用 Vite 静态 SPA（React + TypeScript）

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | 架构 v0.6 §8.2.1、§8.6（DE-01） |

## 背景

Room 实时交互在客户端完成，SSR 仅服务壳与登录；认证为 OIDC Authorization Code + PKCE（SPA 友好）；API、实时流与文件均由 Go 服务提供。

## 决策

React + TypeScript + Vite 构建静态 SPA，产物作为静态资源由 Go 二进制内嵌或任一静态服务承载；`packages/protocol-ts` 为第一方 SDK（从 Room Protocol Schema 生成边界类型）。

## 后果与放弃方案

- 交付物从 Node 运行时镜像简化为静态 bundle（DE-01 相应简化）；
- 放弃 Next.js：无 SSR/SEO/BFF 需求，多一个 Node 服务与更快的依赖漂移是纯成本；出现公开分享页/SEO/独立 BFF 需求时再引入，属可逆决策；
- 登录回调用 PKCE + 页面内处理，无需服务端会话渲染。
