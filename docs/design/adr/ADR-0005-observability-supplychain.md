# ADR-0005：可观测与供应链——OTel Trace/Metric + slog；syft + osv-scanner

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | 架构 v0.6 §8.2.3、§8.5（BE-06）、§8.2.7 |

## 决策

- Trace 与 Metric 使用 OTel Go SDK（已稳定）；日志权威为标准库 `slog`（JSON、结构化、默认脱敏），OTel Log 仍为 Beta，不作依赖；
- 日志与 trace 的关联通过 trace_id 注入 slog 字段实现；
- 供应链（对应 BE-06）：go-licenses 生成依赖许可清单，syft 生成 SPDX/CycloneDX SBOM，osv-scanner 做漏洞扫描，全部进 CI；
- 审计事件独立于日志管道（架构 §8.2.3 Audit 通道），不随日志保留策略漂移。

## 后果与放弃方案

- 放弃引入日志聚合专用 SDK：exporter 保持 OTLP，后端由部署方选择；
- OTel Log 转 Stable 后可平滑接管 slog 输出，届时修订本 ADR。
