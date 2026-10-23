# OpenSite Commerce

可自部署的 B2C + B2B 电商系统（规划中）。技术方向：Go 模块化单体、PostgreSQL 18 唯一事实源（pgx + sqlc，启用 RLS）、River 任务队列、Astro 前台、React 后台；默认用 LocalFS 存储，S3 兼容存储可选。

> **状态：PR-01 数据库基础已导入并完成验收。** 9 份迁移、RLS、角色、sqlc、pgx 已在真实 PostgreSQL 18.6 上通过，GitHub CI 也是绿的，并经 Codex 独立复跑（见 [`docs/PR01A_ACCEPTANCE.md`](docs/PR01A_ACCEPTANCE.md)）。下一步是 PR-01B：工具链、CI 重写和 `010_security_hardening`。还没有任何业务功能，不可用于生产。

## 文档

- [V1 架构研究报告](docs/research/architecture-v1-report.md)：设计输入
- [架构演进史 V0.1 → V1.0](docs/history/README.md)：原始设计对话全文 + ChatGPT 沙箱工件的报告与校验和
- [V1 架构收敛决议](docs/architecture/V1_DECISIONS.md)：身份三分、平台基础层、交易边界、账本、MVP 切线、修正后的 PR 序列（PR-01B → MVP）
- [架构审查 2026-10-12](docs/review/REVIEW.md)：现行 P0 已全部处理（F-01 已验收，F-02 已关闭，F-03 / F-04 在 001–009 中不成立）· 19 个 P1 · 3 个 P2、修正后的 PR 序列、MVP 切线

## CMS 运行接线

带 `pgx` 构建标签的 API 使用 `OPENSITE_DATABASE_URL` 连接已迁移的 PostgreSQL，并在组合根接入 CMS 写事务、只读快照和 C04 幂等回执；回执完成与业务写入使用同一事务。`OPENSITE_IDEMPOTENCY_ROOT` 指定幂等响应文件目录，需持久保存并仅由 API 服务访问。`OPENSITE_CMS_PREVIEW_KEY` 必须是无填充 URL-safe Base64 编码的随机密钥，解码后至少 32 字节，密钥通过环境变量传入；缺少密钥时不构造 CMS writer。生产组合根未提供 staff authenticator 时，管理接口返回 401；认证器还必须提供可信 tenant 和稳定 actor，不能由请求字段代替。真实 PG 测试使用独立的 `DATABASE_URL` 指向已迁移的测试数据库。

预览令牌最长有效 5 分钟，通过 `Authorization: Bearer` 传递。预览响应禁用缓存及搜索索引。令牌固定 CMS 草稿 revision，草稿改变后旧令牌不可继续读取；预览商品来自读取时的目录快照，可能随商品或价格变化。发布后商品、页面与媒体引用随 sealed release 固定，恢复通过 publication revision 防止旧状态覆盖新发布。

## 许可证

Apache-2.0（见 [LICENSE](LICENSE)，随 PR-01 代码包导入）。owner 可以另行更换。

本地部署的第八个独立 LOGIN 为 `opensite_returns_login`，只授予
`app_returns_service`。API 使用 `OPENSITE_RETURNS_DATABASE_URL` 或独立的
`OPENSITE_RETURNS_DB_PASSWORD` 数据库组件配置；迁移与恢复在写入前校验凭据。
启动仅提供 `Config.ReturnsCoordinator.Runner` 可信组合入口，尚未接入退款行为。
缺失配置会记录设置名并保留当前不可用行为。详见
[运行时角色契约](deploy/runtime-roles.md#fulfillment-and-refund-coordinator)。
