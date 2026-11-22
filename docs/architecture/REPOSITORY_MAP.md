# 按业务查找 OpenSite Commerce 文件

从要改的业务操作出发。在 [OpenAPI](../../api/openapi.yaml) 找到 `operationId`，用它到[模块注册表](../../modules/registry.yaml)查所属模块，再查该模块的公开契约、实现路径、表和测试入口。注册表里的模块可以拥有表而尚无目标 Go 包，也可以通过平台适配器提供 HTTP 操作。[应用层决议 A1](APP_LAYER_DECISIONS.md) 规定一个 Go module、静态装配，以及模块之间允许使用的端口。

## 文件该放在哪里

| 要查或修改的内容 | 先看哪里 | 归属怎样确定 |
|---|---|---|
| 业务规则、用例和消费方端口 | [业务模块](../../internal/modules)、[模块说明](../modules) | 按注册表的 `go_path`、`legacy_go_paths`、`approved_public_ports` 定位。模块内的 `contract` 是跨域接口，私有实现留在所属模块。Money 等已有公开包按登记路径使用。 |
| HTTP 入口与后台任务 | [HTTP 平台](../../internal/platform/httpapi)、[命令入口](../../cmd)、[组合根](../../internal/bootstrap) | 路由将请求交给所属域。购物车 HTTP 适配器在平台目录，购物车操作归 Cart。API 与 Worker 是进程，各自可装配多个模块。 |
| 查询、事务与数据变更 | [查询源](../../queries)、[迁移](../../migrations)、[运行角色](../../deploy/runtime-roles.md)、[租户事务适配器](../../internal/tenanttx/pgxadapter) | `owns_tables` 定义 app 表的逻辑归属。迁移另有 owner 和 `touches_tables`。修改 SQL 从查询源和迁移入手，[sqlc 配置](../../sqlc.yaml)决定生成到哪里。调用数据库函数还需核对执行角色、函数写入和事务边界。 |
| 隐私、删除与保留 | [字段清单](../../modules/privacy)、[商家删除契约](MERCHANT_DELETION.md) | Tenant 管生命周期和写入隔离，DataRights 管工作清单与回执，各业务域处理自己持有的数据。 |
| 顾客商城 | [Storefront](../../web/storefront)、[商城支付 BFF 契约](STOREFRONT_BFF_CONTRACT.md)、[类型化客户端](../../packages/api-client) | Astro SSR 读已发布视图，`getPublishedSite` 归 Publish。购物、账户、订单分别经过对应的服务器适配器进入 Cart、Checkout、Customer、Orders。 |
| 员工后台 | [React 入口](../../web/admin/src/main.tsx)、[后台 API 适配器](../../web/admin/src/api.ts) | 员工会话归 Identity，商品内容归 Catalog，价表归 Pricing；绑定商品媒体的操作归 Media。 |
| Sourcyo 批发站 | [独立站点](../../web/apparel)、[询盘处理器](../../web/apparel/functions/api/rfq.ts)、[Supabase 建表](../../deploy/supabase/apparel_rfq.sql) | 询盘落在独立的 `public.apparel_rfq`；Commerce 的 B2B 报价另走 Quotes。服装站有[专属 CI](../../.github/workflows/apparel.yml)。 |
| 共享前端、运行环境与测试 | [共享包](../../packages)、[本地部署](../../deploy)、[脚本](../../scripts)、[测试](../../tests)、[工作流](../../.github/workflows) | UI、文案契约、设计令牌和 API 客户端由站点消费。模块登记的测试是定位线索，实际执行集合以对应 runner 为准。 |

## 顺着一次退款读代码

1. [HTTP 适配器](../../internal/modules/returns/http.go)接收 `refundReturn`，处理订单版本、作用域和幂等回执，进入 Returns 用例。
2. [退款服务](../../internal/modules/returns/internal/service/service.go)通过[消费方端口](../../internal/modules/returns/contract/returns.go)读取已收款事实，在租户事务内冻结退款意图。[存储适配器](../../internal/modules/returns/internal/store/store.go)接入[查询源](../../queries/returns.sql)。
3. 准备事务提交后，服务用已经冻结的身份调用支付提供方。成功后在另一笔事务中经 [Payments 记账入口](../../internal/modules/payments/refund.go)完成入账、状态更新和 HTTP 回执。改动这一段需要保留失败、结果不确定及重放的处理。
4. [组合根](../../internal/bootstrap/returns.go)把专用事务执行器、Payments、账户读取和账本接在一起。[真实 PG 认证退款测试](../../internal/bootstrap/returns_authority_pgx_test.go)覆盖该接线。当前端口签名仍暴露 `pgx.Tx`，而 `RefundCompleter` 和 `RefundPoster` 的部分要求在使用时才检查。若收紧接口，应以现有装配和退款回归验证其兼容性。

## 历史决议与验收怎么读

- 从[历史设计与原始工件](../history/README.md)追溯需求。从 [V1 决议](V1_DECISIONS.md)第八节查交付卡，再读第九节对迁移落号、Cart、Checkout 和 Payments 的修订。匿名页面使用[发布读模型](ADR_STOREFRONT_SERVING.md)，商家删除另受后续专门契约约束。
- [模块门禁](../../modules/README.md)核对操作、依赖和查询直接写入目标。[离线入口](../../scripts/check_all.sh)与[真实 PostgreSQL 入口](../../scripts/check_pg.sh)分工；后者按实际目录及数据库对象核验。[MVP 验收矩阵](../acceptance/MVP_MATRIX.md)按行记录验收提交与回执。
- [历史服装站证据](../../wave7/POLISH/DETAIL-QA.md)随当时的源码与素材版本解读。`work` 中已跟踪的 sender 测试仍由[工作流](../../.github/workflows/sender-source-guard.yml)调用。`wave6` 的目录锚点仍由[打包测试](../../web/apparel/tests/packaging-gate.test.ts)使用。归档前应先查活跃引用。
