# OpenSite Commerce

可自部署的 B2C + B2B 电商系统（规划中）。技术方向：Go 模块化单体、PostgreSQL 18 唯一事实源（pgx + sqlc，启用 RLS）、River 任务队列、Astro 前台、React 后台；默认用 LocalFS 存储，S3 兼容存储可选。

> **状态：架构阶段，仓库里还没有代码。** 文档中所有「已实现」的描述都还没有经过验证，详见审查报告。

## 文档

- [V1 架构研究报告](docs/research/architecture-v1-report.md)：设计输入
- [V1 架构收敛决议](docs/architecture/V1_DECISIONS.md)：身份三分、平台基础层、交易边界、账本、MVP 切线、修正后的 PR 序列（PR-01B → MVP）
- [架构审查 2026-10-12](docs/review/REVIEW.md)：4 个 P0 · 19 个 P1 · 3 个 P2、修正后的 PR 序列、MVP 切线

## 许可证

还没有选定。在添加 LICENSE 文件之前，本仓库公开可读，但不授予复用、修改或再分发的权利。
