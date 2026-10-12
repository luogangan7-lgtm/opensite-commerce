# OpenSite Commerce

可自部署的 B2C + B2B 电商系统（规划中）。技术方向：Go 模块化单体、PostgreSQL 18 唯一事实源（pgx + sqlc，启用 RLS）、River 任务队列、Astro 前台、React 后台；默认用 LocalFS 存储，S3 兼容存储可选。

> **状态：PR-01 数据库基础已导入并完成验收。** 9 份迁移、RLS、角色、sqlc、pgx 已在真实 PostgreSQL 18.6 上通过，GitHub CI 也是绿的，并经 Codex 独立复跑（见 [`docs/PR01A_ACCEPTANCE.md`](docs/PR01A_ACCEPTANCE.md)）。下一步是 PR-01B：工具链、CI 重写和 `010_security_hardening`。还没有任何业务功能，不可用于生产。

## 文档

- [V1 架构研究报告](docs/research/architecture-v1-report.md)：设计输入
- [架构演进史 V0.1 → V1.0](docs/history/README.md)：原始设计对话全文 + ChatGPT 沙箱工件的报告与校验和
- [架构审查 2026-10-12](docs/review/REVIEW.md)：现行 P0 已全部处理（F-01 已验收，F-02 已关闭，F-03 / F-04 在 001–009 中不成立）· 19 个 P1 · 3 个 P2、修正后的 PR 序列、MVP 切线

## 许可证

Apache-2.0（见 [LICENSE](LICENSE)，随 PR-01 代码包导入）。owner 可以另行更换。
