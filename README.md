# OpenSite Commerce

可自部署的 B2C 与 B2B 电商系统。Commerce API 和后台任务采用 Go 模块化单体，交易数据保存在 PostgreSQL。仓库还包含消费者商城、员工后台和 Sourcyo 批发站。

## 项目入口

- [按业务查找文件](docs/architecture/REPOSITORY_MAP.md) · 从页面、操作、端口或表定位代码和测试，含退款链路示例。
- [模块注册表](modules/registry.yaml) 与[治理契约](modules/README.md) · 查询表、API 操作、依赖和测试的归属。
- [V1 架构决议](docs/architecture/V1_DECISIONS.md) 与[应用层决议](docs/architecture/APP_LAYER_DECISIONS.md) · 查业务边界、修订和交付卡。
- [MVP 验收矩阵](docs/acceptance/MVP_MATRIX.md) · 按提交和旅程查证据；[PR-01A 验收](docs/PR01A_ACCEPTANCE.md)记录早期数据库阶段。
- [设计演进与原始工件](docs/history/README.md)、[研究报告](docs/research/architecture-v1-report.md)、[2026-10-12 架构审查](docs/review/REVIEW.md)。

## CMS 运行接线

见[本地运行说明](deploy/README.md#cms-运行接线)。退款协调器的配置和授权见[运行时角色契约](deploy/runtime-roles.md#fulfillment-and-refund-coordinator)。

## 许可证

[Apache-2.0](LICENSE)
