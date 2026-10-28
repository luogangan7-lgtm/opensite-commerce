<!-- source: ChatGPT conversation 6acb3610 (owner 的架构对话)，2026-10-12，由 Claude 编排提问；GPT 已读 feat/pr01a-import 分支的迁移。决议是设计基线，不是实现证据。 -->

我会以 PR #2 的实际代码为依据，不再沿用之前对 001–009 的猜测。先读取 `feat/pr01a-import` 的迁移、查询和 `docs/PR01A_ACCEPTANCE.md`，核对已存在的表、约束与验收证据，再给出第 1–7 项决议及从 PR-01B 到 MVP 首发的依赖顺序；只规定新增 010+ 迁移。

# OpenSite Commerce V1 架构收敛决议

**基准：PR #2 · `feat/pr01a-import` · Commit `1251b95f1cc4`**  
**日期：2026-10-12**  
**约束：001–009 冻结，只允许新增 010+ 前向迁移。**

已读取 [PR #2](https://github.com/luogangan7-lgtm/opensite-commerce/pull/2) 的全部九份迁移、`queries/catalog.sql`、`queries/site.sql`、数据库验收脚本，以及 [PR01A_ACCEPTANCE.md](https://github.com/luogangan7-lgtm/opensite-commerce/blob/feat/pr01a-import/docs/PR01A_ACCEPTANCE.md)。

### 基线核验结果

| 项目 | 当前结论 |
|---|---|
| 001–009 迁移 | 已在 PR #2 分支中核对源码 |
| PostgreSQL 18.6 | 已有真实执行日志，九份迁移运行两遍 |
| SQL 行为测试 | 已有五份测试的执行记录 |
| FORCE RLS 注错 | 已有红→绿记录 |
| sqlc 1.31.1 / pgx 5.10.0 | 已有代码生成和编译验收记录 |
| GitHub Actions | 起草时验收报告写的是 NOT RUN；之后查到有成功运行记录，见下方「GitHub CI 状态补充核验」（按 SHA 分栏） |
| 生产 API / 交易系统 | 尚未完成 |

以上“通过”指 **PR-01A 提交的可追溯本地执行证据**，不是我重新运行了 PostgreSQL，也不代表 GitHub CI 已通过。

源码确认了三个重要事实：

- `001_identity` 已有 `users`、`tenant_memberships`、`auth_sessions`、`api_keys`。
- `005_cms` 已有基础 `inquiries`，后续无需重复创建询盘主表。
- `007_reliability` 已有 `idempotency_records`、`audit_events`、`outbox_events`，**缺少 `webhook_inbox`**。

另有一个必须优先处理的既有问题：`008_rls_grants` 向 `app_runtime` 授予了部分敏感表的 UPDATE 权限，包括成员、域名与媒体状态。需要通过新增迁移收紧，而不能修改 008。

---

## 一、身份三分：员工、消费者与 B2B 买方公司

### 决策

采用三种业务身份、两个认证入口。

| 身份 | 数据所有者 | 核心表 | 用途 |
|---|---|---|---|
| 后台员工 | Identity / Tenant | 现有 `users`、`tenant_memberships` | 登录后台、管理商家 |
| 店铺顾客 | Customer | 新增 `customers`、`customer_addresses`、`customer_credentials` | 注册、访客结账、历史订单 |
| B2B 买方公司成员 | B2B Accounts | 新增 `buyer_companies`、`buyer_company_members`、`buyer_roles` | 企业采购、报价、审批 |

保留 001 的 `users` 作为**后台员工身份**，不把所有前台消费者都加入 `tenant_memberships`。

Customer 的账户以 Tenant 为归属边界；买家可在该租户下的站点购物。后续再配置站点级可见性和市场规则。

`buyer_company_members` 引用 Customer ID。一个 Customer 可以加入多家买方公司，并拥有不同采购角色。

访客结账无需预先创建完整登录凭证，但订单必须保存顾客、收件人与联系方式的成交快照。

尤其需要区分：

- `organizations`：销售方企业。
- `buyer_companies`：向销售方采购的企业。

两者不能复用同一张企业表。

### 被否方案及原因

**否决全员共用 `users/memberships`。** 这容易把消费者与后台管理权限混在一个授权空间。

**否决为 B2B 公司成员创建第三套独立登录系统。** B2B 成员本质仍是 Customer，可额外获得企业采购权限。

### 010+ 迁移影响

新增 `012_customers.sql`、`013_buyer_companies.sql`：

- Customer 唯一性、认证凭证、地址归属。
- 买方公司成员与角色。
- `(tenant_id, customer_id)` 复合外键。
- 独立前台认证权限及 RLS。
- Guest Order 的可空 Customer FK 与订单快照。

001 完全不变。

### 验收 gate

- 访客可下单，不必注册。
- 顾客无法访问后台管理员 API。
- 买方公司 A 的成员不能访问公司 B 的订单。
- 成员被撤权后，原会话不得继续使用该公司权限。
- 同一邮箱在不同租户可独立注册。
- Customer 与后台 User 不发生意外权限继承。

**结论：三分业务身份，后台与前台认证隔离，B2B 公司角色附着于 Customer。**

---

## 二、平台基础层：Idempotency / Inbox / Outbox / Audit

### 决策

单独安排 **PR-F：Platform Reliability**，必须在购物车提交、订单、支付和对外 Webhook 之前完成。

以实际 `007_reliability.sql` 为基线：

| 能力 | 007 当前状态 | 后续处理 |
|---|---|---|
| Idempotency | 已有表 | 补充并发占用与结果重放语义 |
| Audit | 已有表 | 补充审计访问、数据脱敏与不可变约束 |
| Outbox | 已有表 | 补充投递目标、去重、租约和失败恢复 |
| Webhook Inbox | **不存在** | 新建 |
| River 事务任务 | 007 无对应业务协议 | 建立封装与事务测试 |

新增 `webhook_inbox` 的最小字段：

```text
tenant_id
id
provider
provider_account_id
environment
provider_event_id
payload_hash
payload_reference
status
attempts
next_attempt_at
processed_at
created_at
```

唯一约束应至少覆盖：

`(tenant_id, provider, provider_account_id, environment, provider_event_id)`。

Webhook Inbox 保存的是**已经通过签名验证，并完成可信商户账户映射的事件**。不能先相信请求体中的 `tenant_id`。

平台层继续保留 007 已有的 `idempotency_records`、`audit_events` 和 `outbox_events`，通过新增迁移补字段、约束或辅助表。

### River 约定

**内部异步任务：** 同一个 `pgx.Tx` 内写业务数据并执行 River 事务入队。

**外部集成事件：** 业务事务写 Outbox，再由独立 Dispatcher 投递。

两者均按 *at-least-once* 设计，消费者必须幂等。不能承诺天然 exactly-once。

重试、租约恢复、隔离失败任务和人工重放由 OpenSite 自行实现，不依赖 River Pro 才有的扩展能力。

### 被否方案及原因

**否决订单、支付各自实现一套幂等表。** 易出现重复副作用和不同错误语义。

**否决所有任务都先写 Outbox 再投 River。** 会重复建立调度和重试链路。

**否决把 Redis 当成可靠性真源。** 首发 PostgreSQL 足以承担事务基础设施。

### 010+ 迁移影响

建议 `011_platform_reliability.sql`：

- 扩展现有 Outbox。
- 创建 Webhook Inbox。
- 创建必要的投递租约或失败记录。
- Audit：008 已不授予 `audit_events` 的 UPDATE/DELETE 权限（008:43–54），这里不重复修复。改为保持普通 runtime 的审计不可变，并补齐权限矩阵和角色继承的回归负测。
- 为新表增加 RLS、复合 FK 和最小权限 GRANT。

### 验收 gate

必须真实执行：

- 同一 Idempotency Key + 相同请求只产生一次业务副作用。
- request_hash 覆盖规范化后的整个请求：方法、路径模板、解析后的路径参数（如 upload_id）、规范化的 query、请求体（空 body 也参与哈希）。
- 相同 Key + 不同请求（路径、query 或请求体任一不同）返回 **HTTP 422**（错误码 `idempotency_key_reused`，与 REVIEW F-07 一致）；相同 Key + 相同请求体在原请求仍在处理时返回 409，处理完成后重放原响应。
- 重复、乱序 Webhook 不重复扣款或退款。
- 事务回滚后，River Job 不可执行。
- Worker 在提交后宕机，任务仍可恢复。
- Outbox 投递成功但确认失败，重试不会造成重复业务效果。
- 审计事件不能被普通运行角色修改或删除。

**结论：007 保留，F 只补缺口和可靠性协议，不重复造表。**

---

### GitHub CI 状态补充核验

| 时间点 | 绑定 SHA | 证据 |
|---|---|---|
| 起草时（历史证据） | [`1251b95`](https://github.com/luogangan7-lgtm/opensite-commerce/commit/1251b95f1cc4ce98e47ea4923f454efaa5c5ea3c) | [Run 38223245267](https://github.com/luogangan7-lgtm/opensite-commerce/actions/runs/38223245267)：workflow `foundation-contracts`、job `foundation` 结果为 success（覆盖 PG18 迁移与安全、sqlc 生成、pgx 类型检查） |
| PR #2 后续提交 | review 修复提交 | 每个推送都要各自看 `foundation` 的运行结果，以 PR #2 的 checks 页为准；本决议不替后续提交声明结果 |

`docs/PR01A_ACCEPTANCE.md` 早期版本写的 NOT RUN，以及 REVIEW / README 里「未导入、未在真实 PG 跑过」的说法，都是 PR #2 合入前的历史状态，不代表分支现状。PR #2 合入之前，不能视为已合并或生产就绪。

---

## 三、Pricing / Cart / Orders / Checkout 边界

### 决策

采用四个明确模块：

| 模块 | 数据归属 | 核心责任 |
|---|---|---|
| Pricing | `offers`、`price_lists`、`price_entries` | 计算适用售价、阶梯价 |
| Cart | `carts`、`cart_items` | 管理购物意图、数量和选项 |
| Orders | `orders`、`order_items`、`order_events` | 保存成交快照、订单状态 |
| Checkout | `checkout_sessions`、`checkout_attempts` | 编排价格、税费、运费、库存、下单与支付 |

Checkout 放在 `internal/modules/checkout/application`，**作为应用层编排模块，不拥有价格、税务、库存或订单的领域规则**。

核心依赖：

```text
Checkout
  ├── Cart
  ├── Pricing
  ├── Tax
  ├── Shipping
  ├── Inventory
  ├── Orders
  └── Payments
          └── Orders.GetPayable
```

`Payments → Orders.GetPayable` 是单向只读 Port。

Orders 不反向导入 Payments。支付成功后，由 Payments 发出已验证、可幂等处理的支付事件，Orders 更新付款状态。

订单必须先持久化为可付款状态，再调用外部 PSP。不能在数据库事务中等待 Stripe 网络请求。

Checkout 需要持久化报价版本、税费、运费、金额和到期时间，避免重复提交或迟到支付使用过期金额。

### 被否方案及原因

否决将 Checkout 直接放入 Orders：会让订单模块依赖所有支付、价格和运费实现。

否决将订单创建放在 Payments：支付网关不应拥有商品成交规则。

否决 Cart 持有权威最终价格：价格可能因数量、客户身份、税区和配送地址变化。

### 010+ 迁移影响

建议：

- `014_money_pricing.sql`
- `018_cart.sql`
- `019_orders.sql`
- `022_checkout.sql`

Orders 保存不可变商品、价格、税费、运费及折扣快照。

Checkout 的会话与尝试记录采用独立幂等约束。

### 验收 gate

- 同一结账请求重试只生成一笔有效订单。
- 数量、地址、报价过期后必须重新计算。
- 两次并发提交不能超卖。
- 不合法状态的订单不能创建 Payment。
- 支付回调重复或迟到，不重复完成订单。
- 静态依赖检查证明 Orders 不导入 Payments，Payments 不导入 Checkout。

**结论：Checkout 是编排者，Orders 是成交事实，Payments 是资金操作执行者。**

---

## 四、账本：先单币复式记账，再扩展账期和 FX

### 决策

**采纳单币种复式账本优先。**

先实现能可靠处理收款、退款、应收和冲销的账本，再增加 Net 30 等 B2B 账期。

多币种 FX 和 Marketplace 卖家结算推迟到 V1.x，但保留为已确认的产品需求。

### 最小账本表

| 表 | 必需字段 |
|---|---|
| `ledger_accounts` | tenant_id、id、currency、account_type、status |
| `ledger_transactions` | tenant_id、id、currency、source_event_id、status、posted_at |
| `ledger_entries` | tenant_id、transaction_id、line_no、account_id、side、amount_minor |

约束：

- `amount_minor > 0`，使用整数最小货币单位。
- 每笔 Posted Journal 至少两行。
- 同一 Journal 的所有 Entry 币种一致。
- 同一业务事件不得重复记账。
- Posted 后不可修改或删除分录。
- 冲错使用反向 Journal，不修改旧记录。

### 数据库层强制借贷平衡

不能仅靠 Go 服务计算。

使用 PostgreSQL `DEFERRABLE INITIALLY DEFERRED` Constraint Trigger，在事务提交前检查 Posted Journal。

触发器规格：

```sql
CREATE CONSTRAINT TRIGGER ledger_entry_balance
AFTER INSERT OR UPDATE OR DELETE
ON app.ledger_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION app.assert_ledger_balance();
```

另需对 `ledger_transactions` 的 Posting 状态变更建立延迟约束触发器，避免没有 Entry 的空 Journal 被发布。

触发函数必须核对：

```sql
SUM(amount_minor) FILTER (WHERE side = 'debit')
=
SUM(amount_minor) FILTER (WHERE side = 'credit')
```

同时要求 Posted 至少两行、总金额大于零，并验证币种一致。

**锁必须在数据库内强制，而不是依赖 Go 服务约定**：Entry 的 BEFORE INSERT/UPDATE/DELETE 触发器与 Journal 的 Posting 触发器都先 `SELECT … FROM ledger_transactions WHERE id = … FOR UPDATE` 锁定父 Journal，再检查其状态（已 Posted 则拒绝 Entry 变更）。Entry 的父键 `(tenant_id, transaction_id)` 一律**不可变**：BEFORE UPDATE 触发器只要发现父键有变化就拒绝，要换 Journal 只能删掉再插入，删除和插入各自对所属 Journal 加锁、检查状态。这样就不会出现「从已 Posted 的 Journal 移出」或「移入已 Posted 的 Journal」这种单边漏检。**只加锁还不够**：Entry 的每次插入、更新、删除都必须对父 Journal 执行一次真实的 `UPDATE`（例如 `revision = revision + 1`），不能只做 `FOR UPDATE`。如果过账事务运行在 REPEATABLE READ 下，并且在 Entry 事务持锁期间就拿到了快照，那么它等锁释放后写同一行时，PostgreSQL 会报序列化冲突（40001），而不是基于旧快照把不平衡的 Journal 标成 Posted。可选的加强做法：过账命令强制使用 SERIALIZABLE。这样直接 SQL 或未来其他写入方也无法让「过账校验看到旧 Entry、Entry 写入看到 Draft」两个事务同时提交出不平衡的 Posted Journal。验收 gate 增加：两个并发会话分别执行「过账」与「修改 Entry」，在 READ COMMITTED、REPEATABLE READ、SERIALIZABLE 三种隔离级别下各跑一遍，结果必须是其一失败（含 40001）或串行，最终 Posted Journal 始终借贷平衡。另做一次注错：把父行 `UPDATE` 改回只加锁，REPEATABLE READ 那一轮必须变红。

PostgreSQL 支持事务末尾执行的延迟 Constraint Trigger；但上述只是 DDL 契约示例，完整函数仍必须实现及实测（参见 PostgreSQL 18 文档 [CREATE TRIGGER · CONSTRAINT / DEFERRABLE](https://www.postgresql.org/docs/18/sql-createtrigger.html)）。

### 被否方案及原因

否决仅保存 `payment_status` 和 `settlement_amount`：无法可靠处理部分退款、应收与对账。

否决首版建设完整多币种清算账本：汇兑分录、清算账户及跨境结算会显著扩大交易核心复杂度。

否决仅在应用层校验借贷平衡：直接 SQL、并发及异常路径可能破坏账本。

### 010+ 迁移影响

`020_ledger.sql`

必须包含账户、Journal、Entry、币种复合外键、不可变约束和延迟平衡触发器。

V1.x 的 FX 使用独立币种 Journal、清算账户和明确的汇率快照，不允许把两个币种的金额直接相加。

### 验收 gate

- 平衡 Journal 提交成功。
- 不平衡 Journal 在 COMMIT 时失败。
- 无 Entry 的 Posted Journal 被拒绝。
- 并发修改不能绕过平衡约束。
- 重复业务事件只产生一次记账。
- 收款、部分退款、全额退款、应收冲销均可对账。
- Posted Journal 的 UPDATE、DELETE 被拒绝。

**结论：单币账本进入 MVP，账期授信与 FX 在账本稳定后扩展。**

---

## 五、V1 首发 MVP 切线

### 决策

**基本采纳审查方案，附加内置运费、税务、支付和账务一致性要求。**

首发范围：

| 维度 | V1 MVP |
|---|---|
| 部署 | 单实例、单站点 |
| 语言与币种 | 单语言、单交易及记账币种 |
| 仓库 | 默认一个仓库，模型保留 warehouse_id |
| B2C | 访客及注册顾客结账 |
| B2B | 询盘、报价、单层审批、预付 |
| 支付 | 内置支付引擎 + Stripe |
| 税务 | 内置区域税规则引擎 |
| 物流 | 内置运费引擎，运费模板/重量/地区规则 |
| 履约 | 人工确认与部分发货 |
| 售后 | 取消、退款、基础退货 |
| 财务 | 单币复式账本、支付对账 |
| 内容 | 企业官网、商品、博客、模板 |
| 自动化 | API、批量导入、Agent 草稿操作 |
| 运维 | LocalFS、通知、审计、备份恢复、数据导出与删除请求 |

这里的“内置支付”是 OpenSite 自带 Payment Engine、支付状态机和 Stripe 适配器，真实银行卡清算仍由 Stripe 执行。

“内置税务”是区域规则计算，不等于全球税务申报自动合规。

“内置运价”至少应无需承运商 API，就能计算地区、重量、固定费用和免邮规则。

### 被否方案及原因

**否决第一版实现全球税区、全部 PSP、多仓智能分配和 Marketplace。** 它们会显著推迟第一个可靠交易闭环。

**否决只有支付、没有退款和对账就宣布 B2C 可用。** 不具备完整的售后和资金一致性保障。

**否决仅有询盘即宣称 B2B 交易完整。** MVP 至少要求报价确认、单层审批和预付转订单。

### 010+ 迁移影响

由后续模块分别创建 Pricing、Inventory、Tax、Shipping、Cart、Orders、Ledger、Quote、Payments、Fulfillment 等表。

001–009 不改。

数据模型预留币种、仓库、销售渠道和地区，但首版限制启用范围。

**多币种实际结算是已确认的 V1.x 必做能力，不是取消需求。** 正式完成前不能宣传为支持跨币种收款、换汇和结算的完整系统。

### 验收 gate

必须在同一 Commit SHA 下通过：

- B2C 访客及注册顾客完整购买。
- B2B 报价、审批、预付、转订单。
- 税费及运费金额快照。
- Stripe 沙箱收款、取消及退款。
- 并发库存预留不超卖。
- 付款重复通知不重复记账。
- 部分发货、基础退货和退款。
- 数据隔离、通知失败恢复、备份恢复。
- Astro 正式页面与管理后台的核心 E2E、视觉和无障碍检查。

**结论：首发追求完整的小范围交易系统，而不是功能范围很广但交易链不闭合的平台。**

---

## 六、履约、RMA、发票、通知、Webhook 与数据权利归属

### 决策

| 业务能力 | 所属模块 | MVP 范围 |
|---|---|---|
| 履约/发货 | Fulfillment | 手工发货、部分发货、追踪号 |
| 退货 RMA | Returns | 基础退货申请、审核、验收、回库 |
| 退款 | Payments | PSP 退款与幂等处理 |
| 发票/贷项通知单 | Billing | 基础单据与退款冲销 |
| 通知模板 | Notifications | 下单、付款、发货、退款通知 |
| 对外 Webhook | Integrations | MVP 保留可靠 Outbox，订阅投递 API 可延至 V1.x |
| 商品导入导出 | Catalog + DataPortability | CSV 导入、导出、错误回执 |
| 个人数据权利 | Privacy/DataRights | 查阅、导出、删除/匿名化请求 |

Refund 与 RMA 必须分离。

退款成功入账后提供商确认失败/取消的补偿契约见
[C-19d 提案](C19D_REFUND_COMPENSATION.md)（**PROPOSAL，待编排者批准**）；
该提案不修改当前冻结契约，也不代表补偿已实现或允许 live mode。

RMA 判断商品是否退回、能否重新入库；Payments 负责资金退款；Ledger 负责账务记录；Orders 负责订单状态。

发票由 Billing 管理，引用 Orders 的成交快照与 Tax 的计算结果，不重新自行计算税额。

数据删除不能破坏依法必须保留的财务记录，应按适用司法辖区执行匿名化、留存和 Legal Hold。

商家（租户）删除采用[自动删除契约](MERCHANT_DELETION.md)（owner 2026-10-26）：
上线前必须实现 owner-only 自助申请、30 天冷静期与取消、导出副本及 registry 自动擦除审计；
替代支持代办方案。该文档规定实现拆卡和 CI 验收，链接不代表功能已实现。

### 被否方案及原因

否决 Orders 包办所有业务：会形成高度耦合的超级模块。

否决 Returns 直接写库存和支付表：容易重复回库、重复退款。

否决将数据删除理解为数据库级级联 DELETE：可能破坏法定留存和账本完整性。

### 010+ 迁移影响

建议：

- `024_fulfillment_returns.sql`
- `025_billing_notifications.sql`
- `026_data_rights_import_export.sql`

外部 Webhook 订阅和投递端点可在 V1.x 增加；011 的基础 Outbox 必须提前具备。

### 验收 gate

- 部分发货总数量不能超过购买数量。
- 同一 RMA 不得重复入库。
- 累计退款不得超过实际可退金额。
- 退款与贷项通知单可以追溯。
- 通知发送失败可重试，不影响订单提交。
- CSV 导入中断可安全恢复。
- 不同租户的数据不能互相导出。
- 删除请求执行后，普通数据查询不再返回应删除的信息；备份恢复也要重放隐私删除记录。

**结论：履约、基本售后、通知和数据治理进入 MVP；复杂物流、换货、第三方 Webhook 生态与高级票据进入 V1.x。**

---

## 七、两处 PostgreSQL DDL Bug 的正式处理

### 7.1 已核对的 001–009 现状

通过逐文件读取，确认：

**F-03：`UNIQUE NULLS NOT DISTINCT`**

001–009 当前并没有 `app.payments` 表，也没有针对 `provider_payment_id` 的错误唯一约束。

因此 **不需要新增 010 去修改不存在的 Payments 约束**。

未来 Payments 迁移直接使用：

```sql
CREATE UNIQUE INDEX payments_provider_ref_uq
ON app.payments (
  tenant_id,
  provider,
  provider_account_id,
  environment,
  provider_payment_id
)
WHERE provider_payment_id IS NOT NULL;
```

其中 `provider_account_id`、`environment` 在对应在线支付记录中必须非空，避免其他 NULL 键导致唯一性被绕过。

**F-04：RLS 空串**

实际 `008_rls_grants.sql` 已经使用：

```sql
nullif(
  current_setting('app.tenant_id', true),
  ''
)::uuid
```

所以 001–009 不存在需要重新修复的同类直接转换写法。

但应增加真正的连接复用行为测试，当前验收记录也明确列出这项缺口。

### 7.2 被否方案及原因

否决修改 008：会破坏已验收迁移的 SHA-256 和历史一致性。

否决使用 `UNIQUE NULLS NOT DISTINCT` 处理尚未获得 Provider 引用的支付：会错误拒绝合法的新支付。

否决仅靠静态搜索证明 RLS 安全：必须在真实数据库连接上执行事务复用测试。

### 7.3 010+ 迁移影响

**010 应用于真正存在的安全问题，不用于重复修复这两个已排除的问题。**

建议 `010_security_hardening.sql`：

- 收紧 `app_runtime` 对 `tenant_memberships` 的 INSERT/UPDATE。
- 禁止普通运行角色自行验证域名。
- 禁止绕过 Media Service 将素材直接设为 READY/Public。
- 将站点正式发布指针切换收敛到受控发布操作。
- 引入 Release sealed 状态或等效数据库约束，禁止已封存 Release 继续追加子项。
- 审计既有角色的危险权限及 Role Membership。

注意：仅撤销 UPDATE 不够。现有 008 也广泛授予 INSERT，必须同时检查能否通过插入新行伪造“已验证”或“已发布”状态。

后续 Payment 迁移直接采用正确的部分唯一索引，无需修补 001–009。

### 7.4 导入后及后续迁移的 grep 规则

建议在 CI 中执行：

```bash
# 找出需要审查的 NULLS NOT DISTINCT
rg -n -U -i \
  'UNIQUE\s+NULLS\s+NOT\s+DISTINCT' \
  migrations/

# 搜索所有 Provider Payment 引用及约束
rg -n -i \
  'provider_payment_id|payments.*unique' \
  migrations/ queries/

# 精确搜索未经过 nullif 保护的直接 UUID cast
rg -n -U -P \
  "current_setting\s*\(\s*'app\.tenant_id'\s*,\s*true\s*\)\s*::\s*uuid" \
  migrations/ queries/
```

第三条匹配即应阻断。

前两条用于审查，不应简单规定所有 `NULLS NOT DISTINCT` 都不合法。

### 7.5 SQL 断言

RLS 连接复用断言必须有**非空见证数据**，并在**同一数据库连接**上执行（否则空库下即使上下文泄漏也会得到零行，断言恒真）：

```sql
-- 0. 准备（以迁移 owner 身份，单独提交，跨后续事务保留）：
--    按 001–003 的必填列插入测试租户 W 及其下 1 个产品，id 带唯一前缀，测试结束后清理。

-- 1. 租户事务：先证明能看到见证行
BEGIN;
SELECT set_config('app.tenant_id', '<W>', true);   -- true = 事务级（SET LOCAL 语义）
SET LOCAL ROLE app_runtime;
DO $$ BEGIN
  IF (SELECT count(*) FROM app.products) < 1 THEN
    RAISE EXCEPTION 'witness not visible in tenant tx';
  END IF;
END $$;
COMMIT;

-- 2. 同一连接、无租户事务：上下文必须已清空，且看不到见证行
BEGIN;
SET LOCAL ROLE app_runtime;
DO $$ BEGIN
  IF coalesce(current_setting('app.tenant_id', true), '') <> '' THEN
    RAISE EXCEPTION 'tenant setting survived transaction end';
  END IF;
  IF (SELECT count(*) FROM app.products) <> 0 THEN
    RAISE EXCEPTION 'tenant context leaked';
  END IF;
END $$;
ROLLBACK;
```

预期：第 1 步看到 ≥1 行；第 2 步设置为空串/NULL、零行，且不抛 UUID 转换错误。

**回归要求（注错红 → 绿）**：把第 1 步改成 `set_config(..., false)`（会话级、跨事务保留）时，第 2 步必须失败；恢复为 `true` 后通过。红、绿两轮之间必须先执行 `RESET app.tenant_id`，或者换一个新连接，否则红轮留下的会话级设置会让绿轮继续失败。

Payments 表实现后，再执行两类真实数据库断言：

**唯一性断言：**

- 同一租户、Provider、账户和环境下，两笔 `provider_payment_id IS NULL` 的 Created Payment 均能插入。
- 两笔具有相同非空 Provider Payment ID 的记录，第二笔必须失败。

**Catalog 断言：**

- 检查 `pg_index.indisunique = true`。
- 检查 `pg_index.indpred IS NOT NULL`。
- 使用 `pg_get_expr(indpred, indrelid)` 确认谓词包含 `provider_payment_id IS NOT NULL`。
- 不允许针对该 Provider 引用建立导致 Created Payment 冲突的 `indnullsnotdistinct` 唯一约束。

此外，未来每次新增含 `tenant_id` 的表，都要检查 ENABLE/FORCE RLS、`USING`、`WITH CHECK`、复合外键及应用角色权限。

### 验收 gate

这两类 SQL 断言必须纳入 PostgreSQL 18 CI。

至少执行一次故障注入：恢复错误 DDL → 测试红；改为正确约束 → 测试绿。

**结论：现有 008 的空串写法正确；Payments 错误约束尚未进入仓库。010 优先修复实际存在的运行权限和发布不可变性问题。**

---

# 八、修正后的 PR 序列

以下 PR 编号是**计划编号**，不是已经创建的 GitHub PR 编号。PR-01A 已由 PR #2 承载，后续从 PR-01B 开始。

| PR 号 | 模块 / 迁移 | 依赖 | 核心验收 gate |
|---|---|---|---|
| **PR-01B** | 工具链、CI、`010_security_hardening` | PR-01A | 锁定版本；权限升级负测；已发布 Release 不可追加；GitHub 必需门禁 |
| PR-02 | Identity/Tenant 后端认证 API | 01B | 会话轮换、撤权、跨租户拒绝 |
| PR-03 | Module Registry、OpenAPI、模块边界 CI | 01B | 禁止跨模块私有依赖；受影响 CI 不漏测 |
| **PR-F** | Reliability、`011_platform_reliability` | 01B、03 | 幂等、Inbox/Outbox、River 宕机及重放测试 |
| PR-04 | Customer/Guest、`012_customers` | 02、03 | 顾客/员工隔离、访客结账身份 |
| PR-05 | B2B 买方公司、`013_buyer_companies` | 04 | 公司成员及采购权限隔离 |
| PR-06 | Catalog + Media、LocalFS | 02、03 | 商品/SKU、上传哈希、RLS、素材引用 |
| PR-UI-01 | Design System + Visual CI | 03 | React/Astro 组件、视觉/a11y 注错红→绿 |
| PR-07 | CMS、Astro、Builder、Site Release | 06、UI-01 | 企业官网/博客、草稿预览、发布/恢复 |
| PR-08 | Money + Pricing、`014_money_pricing` | 03、06 | 精确金额、阶梯价格、冲突规则 |
| PR-09 | Inventory、`015_inventory` | 06、F | 单仓预留、并发不超卖 |
| PR-10 | Tax Engine、`016_tax` | 04、08 | 税区规则、税额舍入、税快照 |
| PR-11 | Shipping Rate、`017_shipping` | 04、06、09 | 地区/重量运价、不可配送拒绝 |
| PR-12 | Cart、`018_cart` | 04、06、08 | 数量修改、版本和幂等 |
| PR-13 | Orders/GetPayable + **Payment Port 契约 v1**（见第九节）、`019_orders`（逻辑标识） | 09–12、F | 成交快照、不合法状态转换、订单幂等、Port 契约测试 |
| **PR-14** | 单币复式账本、`020_ledger` | 08、F | 延迟借贷平衡、不可变及重复记账拒绝 |
| PR-15 | Inquiry、Quote、单层审批、`021_quotes` | 05、06、08 | 报价到期、审批权限、预付条件 |
| PR-16 | Checkout 编排、逻辑迁移标识 `checkout`（正式编号合入时分配） | PR-F、09–15，其中 PR-13 已提供 Payment Port | B2C/B2B 下单、重报价、失败恢复 |
| PR-17 | Payments/Stripe、`023_payments` | 13、14、16、F | 沙箱收款、Webhook 重放、退款、对账 |
| PR-18 | Fulfillment/Returns、`024_fulfillment_returns` | 09、13、17 | 部分发货、取消、退货、一次性回库 |
| PR-19 | Billing/Notifications、`025_billing_notifications` | 10、13、14、17、18 | 单据冲销、通知重试、无重复副作用 |
| PR-20 | Import/Export/DataRights、`026_data_rights_import_export` | 02、04、06、13 | 批量导入、权限导出、数据删除及留存 |
| PR-OPS-01 | Compose、备份恢复、监控、安全部署 | 01B、F、06 | 异机恢复、DB/Blob 一致性、故障告警 |
| **PR-MVP** | 端到端首发验收 | 全部 MVP PR | 两条交易路径、退款/取消、隔离、恢复、视觉与 CI 同 SHA 通过 |

### 并行关系

PR-01B 是基础阻断项。

在 01B 完成后，PR-02 与 PR-03 可并行；PR-F 在公共契约确认后启动。

Media/Catalog、UI Design System 和 Customer 模块可以分线程推进。

Pricing、Inventory 可并行；Cart（PR-12）依赖 PR-04、06、08，不强制依赖 PR-F；Tax、Shipping 在各自前置契约稳定后推进。

Ledger 可以与 Cart/Orders 的后续工作并行开发，但**必须在 Checkout（PR-16）集成、账期、正式退款对账和交易首发验收前完成**。

Checkout 统一负责交易集成，不允许各模块自行建立第二套结账入口。

> **2026-10-12 补充修订**：第八节 PR 序列表中的固定迁移号（`010_…`–`026_…`）一律改为**逻辑迁移标识**，正式编号在合入时分配；PR-12 / PR-16 / PR-17 的依赖与验收 gate 以第九节为准。

## 最终拍板

架构现在应冻结以下顺序：

**PR-01B 安全与 CI 收敛 → F 可靠性基础 → 三类身份 → 商品/内容 → Money/Pricing/Tax/Shipping/Inventory → Cart/Orders → Ledger → B2B Quote/Approval → Checkout/Payments → Fulfillment/Returns → Billing/DataRights → MVP 验收。**

这份决议保持 001–009 不变，将真实缺口放入 010+，并以每个模块的数据库不变量、API 契约、故障注入及可追溯 CI 结果作为合并依据。

**PR-01A 的本地与 GitHub 基础验收已有成功证据；本决议中 PR-01B 及之后的所有新功能和 Gate 仍是待实施要求，不能预先标记为通过。**

---

## 九、补充修订（2026-10-12，P1-a / P1-b，取代前文冲突条款）

适用文件：`docs/architecture/V1_DECISIONS.md`  
修订范围：迁移编号分配、PR 依赖、Cart/Checkout/Payments 契约。  
约束：已验收的 001–009 永久冻结，所有后续数据库变更仅通过前向迁移实施。

### 一、P1-a：迁移编号与并行 PR 合入规则

### 决策：选择方案 A——合入前由集成者分配正式迁移号

**迁移编号表示进入受保护主干的数据库演进顺序，不表示模块开发顺序，也不与 PR 编号绑定。**

自本决议起，取消 `010–026` 与具体功能模块之间的永久编号预留关系。原文中的 `012_customers.sql`、`020_ledger.sql` 等名称改为逻辑迁移标识，实际数字以合入时分配的版本为准。

执行规则：

1. **并行开发阶段**：数据库变更以 `db/migration-proposals/<module>_<description>.sql` 保存，不得在功能 PR 中预占未来编号。每个迁移提案注明所属模块、前置迁移、数据兼容性与回滚预案。
2. **PR 验证阶段**：CI 基于受保护主干的完整迁移历史，在临时工作树中将本 PR 的迁移提案物化为下一个连续编号；先执行全部现有迁移，再执行本 PR 的迁移及 PostgreSQL 18 测试。临时编号不视为正式分配。
3. **合入准备阶段**：集成者更新 PR 到最新主干，检查业务依赖，将迁移提案移动到 `migrations/` 并分配当前下一个连续编号。若有多个提案，同一 PR 内按显式依赖拓扑排序。
4. **正式验收阶段**：对最终编号、最终文件内容和最终 Commit SHA 重新执行真实 PostgreSQL 18 迁移、空库测试、升级测试、`sqlc generate`、Go 编译、RLS/约束检查。更新后的 CI 与独立审核必须通过才能合入。
5. **合入后**：迁移文件名及内容永久冻结，禁止改号、改名、改内容或复用已合入编号。修复只能新增下一份迁移。

**实现（INF-MIG，2026-10-13）**：提案目录实际为 `migrations/pending/<stable-id>_<name>.sql`（stable-id 为短 slug，如 `c15b-serving`；`registry.yaml`/`sqlc.yaml` 直接引用该路径）。顺序 = stable-id 字典序，可用文件头 `-- depends-on: <stable-id>` 覆盖；未知依赖、环、重名一律失败。`scripts/migrate.sh` 在 `OPENSITE_ALLOW_PENDING_MIGRATIONS=1`（仅一次性开发/CI 库）时于临时目录接续编号执行，否则只要 `pending/` 非空即拒绝（生产/演练 fail closed）；CI 的 foundation 与 mvp-e2e 先在临时 checkout 中执行 `scripts/pending_migrations.py ci-normalize`；frontend 与基线生产 job 不改 checkout，由一次性库夹具 `start_storefront_api_e2e.sh` 以该标志经临时目录应用。合入时集成者执行一次 `scripts/land_migration.sh <stable-id>`（字节不变地移动到下一个连续编号并改写引用；pending 文件首行必须是 `-- stable-id: <id>`，落号后其他提案的 depends-on 仍可解析，开发库校验和不变；`ops_deploy` 遇到 pending 拒绝时报 `MIGRATION_REFUSED_PENDING`）。已合入的编号文件（含 001–014）仍永久冻结、只前进。开发库上 pending 迁移的版本名在落号后会变，重建开发库即可。

PR-01B 仍是首个需要合入的基础变更，因此正常情况下其安全加固迁移将获得 `010`；但其他 PR 不预留 `011+`。

### 被否方案：B——所有迁移 PR 严格串行合入

该方案虽然简单，但会人为制造无业务必要的依赖。例如 Customer 与 Reliability、Pricing 与 B2B Buyer Company 即使没有直接数据依赖，也必须等待编号前序 PR，降低多 Agent 并行效率。

采用 A 不意味着允许忽略真实业务依赖。涉及外键、接口、数据迁移或领域行为的依赖必须保持；只取消由迁移数字造成的虚假依赖。

### 必须新增的 CI 检查

| Gate | 检查要求 |
|---|---|
| Migration History | 001 起连续、无重复、无缺号、无历史 SHA 漂移 |
| Migration Ownership | 新迁移必须归属一个模块和 PR |
| Pending Validation | 提案必须在临时连续编号下通过真实 PG18 验证 |
| Final Number Validation | 合入前正式编号与最新主干连续 |
| Forward Compatibility | 空库安装、既有数据库升级均通过 |
| Codegen | `sqlc.yaml` 覆盖实际新 DDL，生成代码无未提交差异 |
| Merge Gate | 最终 PR SHA 对应的数据库、代码生成和独立审核全部通过 |

**以下情况必须阻断合并：** 迁移缺号、旧迁移被修改、数据库已应用版本在源码中消失、提案仍未转成正式迁移、编号分配后未重新验收，或测试使用的 SHA 与最终提交不一致。

影响分析和必需检查集合由受保护主干的 CI 策略确定，不能通过 PR 自行修改模块清单来跳过测试。必需测试出现 `skipped`、`cancelled`、`failure` 或未产生执行记录，均不得视为通过。

---

### 二、P1-b：Cart / Checkout / Payments 的 Port 与 Gate 边界

### 决策一：PR-12 只管理购物意图；Checkout 提交统一由 PR-16 承担

PR-12 不新增对 PR-F 的强制依赖，但必须严格限制职责：

**Cart 负责：**

- 创建和读取购物车。
- SKU、数量、选项的修改与移除。
- 购物车版本控制。
- 顾客与访客购物车关联。
- 购物车数据持久化。

**Cart 不负责：**

- 创建订单。
- 最终成交价格及税运快照。
- 库存预留。
- 发起支付。
- 正式提交 Checkout。

Cart 修改优先使用设置绝对数量的幂等 `PUT/PATCH` 操作，配合 ETag / `If-Match`、数据库唯一约束及乐观并发控制。不得提供未经统一幂等保护的“重复请求即继续加一件”操作。

原决议中的“F 必须先于购物车提交”修订为：

**PR-F 必须先于 Checkout 确认提交、Orders 业务命令及 Payments 外部副作用；普通 Cart 意图编辑不必等待 PR-F。**

如果 PR-12 将来扩展为非幂等批量命令，则该扩展必须显式依赖 F。

### 决策二：支付 Port 在 PR-13 定义，PR-16 使用 Fake，PR-17 实现真实支付

为消除 PR-16 与 PR-17 的循环依赖，采用 Contract First。

**PR-13：冻结最小 Payment Port 契约。**

建议位置：

- `internal/contracts/payment/v1/`：与 Provider 无关的支付请求、结果、状态及接口。
- `internal/modules/orders/port/payable.go`：Orders 提供只读 `GetPayable` 契约。

支付 Port 至少定义：

- `CreatePaymentIntent`：订单、金额、币种、幂等键及订单版本。
- `PaymentIntentResult`：Intent ID、状态、下一步支付动作。
- `PaymentStatus`：Pending、RequiresAction、Authorized、Captured、Failed 等语义。
- `PaymentError`：可重试、不可重试、金额不符及状态冲突。
- 经验证支付结果交付 Orders 的事件契约。

不得在 Port 中暴露 Stripe 专有对象作为核心业务类型。

**PR-16：实现 Checkout 编排。**

Checkout 依赖抽象 Payment Port，不导入具体 Stripe Provider。

使用测试专用 Fake Payment 实现验证：

- 订单创建与支付意图请求顺序。
- 金额、币种及订单版本一致性。
- 重复提交。
- 支付处理中断。
- Fake Provider 失败。
- 超时和状态恢复。
- B2B 预付与 B2C 购买路径。

Fake 不得在生产配置中充当真实收款渠道，也不得凭客户端返回值直接将订单标记为已付款。

**PR-17：实现 Payments 领域服务与 Stripe Adapter。**

真实 Payment Service 实现前述 Port，并通过 Orders 的 `GetPayable` 获取权威应付金额。

模块依赖方向：

```text
Checkout → Payment Port
Checkout → Orders
Payments Service → Orders.GetPayable
Stripe Adapter → Payment Provider Interface

Orders 不依赖 Payments 实现
Payments 不依赖 Checkout 实现
```

真实支付结果由经过签名验证、可信账户映射和 Inbox 去重的事件处理流程提交，Orders 通过受控命令更新付款状态。

### C-19b consent contract

Before a real-provider `Port.Capture`, Checkout must durably record buyer acceptance
of `(tenant, buyer, order, payable_version, amount_minor, currency)` and compare it
with the locked immutable payable before acquiring the `ready -> confirming`
claim. A changed payable needs explicit acceptance and a fresh request key; Start
fingerprints its accepted total, and its immutable snapshot binds the order/version.
Amounts/currencies match exactly; missing/malformed input is 422
`validation_failed`, while a valid stale total is 409 `checkout_total_changed`.

`AdoptOrder` currently has no acceptance parameter or receipt. Its only composed
provider is the nonproduction Fake (`composeFakeCheckout`, `wireStorefrontCheckout`);
`quoteCheckout.Prepay` adopts the sealed quote-conversion order. There is no real
provider Capture path today. Adoption/quote authorization or a sealed Orders
snapshot alone **does not establish payment consent**. C-19b must extend admission
and persist the above acceptance before enabling a real provider for this source,
or keep real-provider adoption disabled. Its CI gate must reject missing/stale,
wrong-buyer/order/version/currency acceptance without a claim or provider effect,
and prove exact matching consent and stable-key recovery. PSP-PREREQ deliberately
adds no speculative adoption API or Stripe code.

The existing cart guard is required only when acquiring a new capture claim.
Receipt-free pre-C-09e `confirming` claims recover their original event/provider
key; completed attempts and paid sessions replay the persisted result. Neither
path creates new payment consent or a new business attempt. Event identity,
payable version, amount, currency, provider reference and claim key checks remain
mandatory. `ApplyPaymentEvent` records already completed trusted provider facts
without calling Capture; it must retain actual money even for old sessions and
must never be treated as permission to initiate capture. No async/batch writer
may bypass this admission contract when C-19b introduces real-provider IO.

### C-19b sandbox implementation boundary

`OPENSITE_PAYMENT_PROVIDER` defaults to `fake`; the only additional value is
`stripe-sandbox`, accepted only with `OPENSITE_ENVIRONMENT=test|development` and
an env-only `STRIPE_TEST_SECRET_KEY` beginning `sk_test_`. Live/restricted keys
have no admission path. No publishable key is needed. The sandbox also requires
`STRIPE_TEST_WEBHOOK_SECRET` (from the pinned CLI listener) and
`STRIPE_TEST_ACCOUNT_ID`, with an enabled `stripe/test` provider-account binding.
The existing storefront tenant/origin/key/rules and runtime DB settings compose
Checkout. Missing cart composition refuses provider IO. Only `pm_card_visa` and
`pm_card_chargeDeclined` are admitted by `STRIPE_TEST_PAYMENT_METHOD` (Visa default).
No customer/email/address fields are sent to Stripe.

Checkout's real-payment admission rereads the persisted cart accepted total,
locked payable and settlement currency, with the original create/capture key.
`AdoptOrder`/quote and receipt-free recovery remain disabled for this provider;
Fake recovery behavior is preserved. DB time bounds create retries to 23 hours
because Stripe may prune idempotency keys after 24 hours. Confirmation/cancel
recover the referenced PaymentIntent's terminal state. Refund retries first
recover the operation's durable metadata; histories beyond 100 refunds fail
closed. The sandbox supports synchronous test-card refunds; pending refunds
remain unavailable for reconciliation rather than acquiring no-side-effect proof.

The adapter uses stdlib HTTPS and equivalent constant-time HMAC-SHA256 verification
rather than a large SDK dependency: fixed Stripe URL, bounded responses/errors,
API version `2026-08-26.dahlia`, no redirects, exact raw bytes, rotating v1 signatures,
and five-minute DB-clock tolerance before and after the existing stream lock.
The mapped account, stream, inbox, Checkout and immutable Orders remain the data
and lock authorities. The original fake inbox namespace stays isolated.
`POST /api/v1/payments/stripe/webhook` returns 503 when unconfigured.

Live mode requires a separate design/card, consent contract for any new source,
security review, production gate and explicit owner launch authorization. It
cannot be enabled through a sandbox flag or secret replacement. Real sandbox
lifecycle CI is a dependent delivery unit under C-19b's line-budget split; until
its final-SHA receipt exists the MVP Stripe acceptance row remains NOT_RUN.

### Stripe live smoke (CI-only, owner-gated)

Owner decision 2026-10-27: “同意替代方案，只保留A段” (LIVE-A-ONLY,
#414). This revision replaces the previous live payment/recovery design. The
preceding C-19b production/sandbox boundary stays in force.

1. **Phase A only.** This is a restricted live key permission probe. Create a
   **50-minor-unit USD PaymentIntent with no payment method**, then cancel it.
   Never supply card/payment-method details or confirm a payment. Assert the
   same intent's live account/run/phase, amount/currency, canceled status,
   `amount_received=0`, `payment_method=null` and **zero journals**. There is no
   payment, refund, reconciliation or recovery acceptance in live mode.
   Those journeys remain in **Stripe test mode + offline fixtures**.

2. **Policy authority.** Stripe's official [Testing documentation](https://docs.stripe.com/testing),
   read **2026-10-28**, states: “The Stripe Services Agreement prohibits testing
   in live mode using real payment method details.” Live real-card testing is
   prohibited; owner consent does not override the SSA. The probe supplies no
   payment method and moves no money.

3. **Owner gate and credentials.** Only `workflow_dispatch` on `main`, Environment
   `stripe-live`, with owner approval for every run. Agents never dispatch/rerun
   it or call Stripe live. The required key permission is **PaymentIntents write
   only** (including its implied read capability). The key lives exclusively in
   `STRIPE_API`; accept only `rk_live_`, refuse `sk_` and test keys. The expected
   account remains the private `STRIPE_SMOKE_ACCOUNT` Environment secret and
   scopes every request through `Stripe-Account`. Actual Dashboard permissions
   remain UNKNOWN until owner verification; never broaden them automatically.
   No additional customer/email credential is required.

4. **Data authority and write entrances.** Stripe responses own provider state;
   Ledger owns journals. The probe has no financial Port, Payments ingestion,
   Checkout/Orders writes, Inbox or live webhook. Only create-without-method and
   cancel are new provider writes. Production, old, bulk, asynchronous and
   sandbox paths gain no live admission. Read the mapped disposable DB clock
   before IO; no provider request holds a DB lock. There is no wait-state
   deadline or financial lock sequence. CI process timeouts bound execution,
   never imply provider cancellation or success.

5. **Identity and uncertainty.** Refuse Actions attempts other than 1 in both
   wrapper and adapter. Before create, completely paginate PaymentIntents using
   durable run metadata, with bounded pages and duplicate-cursor rejection.
   An existing or uncertain identity exits non-zero without a new create.
   Metadata is a locator, never financial evidence; idempotency expiry does not
   authorize another create. Repeated Execute on one adapter is refused.
   Unknown create/cancel outcomes remain `PENDING`, with owner inspection of the
   original run; there is no automated repair or reimbursement path.

6. **Security gates.** Normal CI enforces dispatch/main/Environment admission,
   one secret consumer, both `secrets.X` and `secrets['X']` ownership forms,
   fail-closed computed contexts, and no production constructor reference in any
   Go path including `cmd/`. A static negative fixture rejects money-moving
   endpoints (confirm/capture/invoice/refund/payment_method); the transport
   independently permits only list/create/cancel PaymentIntents and an exact
   create form without a payment method. Keep allowlisted public-output canaries
   for success, failure, timeout and panic. Discard raw child responses/errors;
   no secret, full provider ID, URL, email or raw artifact is public.

7. **Acceptance.** Tests first RED, repaired GREEN and at least two CI mutation
   probes bind to exact source SHAs and run IDs. Normal Foundation covers fake
   HTTP, real PG zero-journal witnesses and the actual Go-process output wrapper;
   normal CI compiles the owner-only entry without executing live transport.
   Required checks must pass at current head before ready for review. Independent
   security review remains distinct from CI; no local tests/builds/containers or
   live dispatch. Offline green proves only its tested environment. Live probe
   and production acceptance remain **NOT_RUN** without separate owner receipts.

### 修订后的三行 PR 序列

| PR 号 | 模块与依赖 | 验收 Gate |
|---|---|---|
| **PR-12** | Cart；依赖 PR-04、06、08；不强制依赖 F | 购物车 CRUD、绝对数量更新幂等、ETag 冲突、访客购物车隔离；禁止订单创建、库存预留及支付副作用 |
| **PR-16** | Checkout 编排；依赖 PR-F、09–15，其中 PR-13 已提供 Payment Port | Fake Provider 完成 B2B/B2C Checkout 编排、重复提交、重报价、支付失败及恢复测试；只验收支付编排，不宣称真实收款可用 |
| **PR-17** | Payments Core + Stripe；依赖 PR-13 Payment Port、14 Ledger、16 Checkout、PR-F | Stripe 沙箱支付、原始 Webhook 验签、账户映射、Inbox 去重、授权/扣款、取消、部分及全额退款、账本对账与故障恢复 |

### 合并与首发限制

PR-16 可以在没有真实 Stripe Adapter 时合并，但生产环境的在线支付与 Checkout 完成能力必须保持未启用状态。

PR-17 必须通过真实 Stripe 沙箱流程，不能以 Fake Provider 的结果替代。

**MVP 首发 Gate** 必须在同一最终 SHA 上验证完整 B2C/B2B 购买、支付、退款、账务、订单状态以及重复 Webhook 和宕机恢复。

---

### 三、修订生效规则

本次只修正迁移编号分配和交易 Port 依赖关系，不改变已经确定的业务模块所有权、MVP 范围及 001–009 冻结规则。

所有原 PR 序列表中的固定迁移数字统一改为**逻辑迁移标识**；后续由集成者在合入前分配正式连续编号。

**架构决议不等于测试结果。以上新增 Gate 均为待实施要求，必须获得真实 CI 证据后才能标记 PASS。**
