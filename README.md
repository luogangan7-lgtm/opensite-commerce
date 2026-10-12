# OpenSite Commerce

可自部署的 B2C + B2B 电商系统（规划中）。技术方向：Go 模块化单体、PostgreSQL 18 唯一事实源（pgx + sqlc，启用 RLS）、River 任务队列、Astro 前台、React 后台；默认用 LocalFS 存储，S3 兼容存储可选。

> **状态：架构阶段。** PR-01 数据库基础代码（ChatGPT 沙箱产出，从未在真实 PostgreSQL 上跑过）正在走导入 + PG18 真实验收（PR-01A），通过前不进 main。

## 文档

- [V1 架构研究报告](docs/research/architecture-v1-report.md)：设计输入
- [架构演进史 V0.1 → V1.0](docs/history/README.md)：原始设计对话全文 + ChatGPT 沙箱工件的报告与校验和
- [V1 架构收敛决议](docs/architecture/V1_DECISIONS.md)：身份三分、平台基础层、交易边界、账本、MVP 切线、修正后的 PR 序列（PR-01B → MVP）
- [架构审查 2026-10-12](docs/review/REVIEW.md)：3 个现行 P0（F-01 已改判，F-02 已关闭）· 19 个 P1 · 3 个 P2、修正后的 PR 序列、MVP 切线

## 许可证

拟采用 Apache-2.0（PR-01 代码包自带），LICENSE 文件随 PR-01A 代码导入时加入。在此之前，本仓库公开可读，但不授予复用、修改或再分发的权利。
