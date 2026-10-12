# OpenSite Commerce V1 架构报告审查（2026-10-12）

- 审查对象：`docs/research/architecture-v1-report.md`（由原始 `deep-research-report.md` 清洗得到：去掉 42 处失效引用标记、3 个 ChatGPT 沙箱链接。原始文件 SHA256 `41e9af31…b922a5`）
- 审查方式：Claude 统筹，3 路 Codex 只读子审查（A 事实/基线 · C 需求/模块/路线 · D 安全/运维/治理），原始输出在 [`raw/`](raw/)。B 路（SQL/事务）因 Codex 模型容量报错未跑成，改由 Claude 完成。
- 证据级别：**E1**。只审了文档，没有可运行的代码。下面所有「验收 gate」都还**没有执行（NOT_RUN）**。例外：F-03 和 F-04 已在 PostgreSQL 18.6 一次性容器里复现了 bug 并验证了修复（**E2**，[`repro/p0-ddl.sql`](repro/p0-ddl.sql) → [`repro/p0-ddl.pg18.log`](repro/p0-ddl.pg18.log)）。

## 状态更新 2026-10-12（找到了完整来源）

owner 提供了原始 ChatGPT 对话（`6acb3610`，51 轮）。从中取回了以下内容：

- **V0.1 → V1.0 前置审查全文**：12 篇，约 28 万字，已放进 [`docs/history/`](../history/README.md)。
- **3 个代码包和 4 份报告**：已取回，报告和校验和在 [`docs/history/artifacts/`](../history/artifacts/)。

**F-01 改判：基线确实存在，但从未在真实数据库上验收。**
- 最新一版是 `opensite-commerce-pr01-acceptance-gated.zip`，sha256 `c0be392b…`，共 56 个文件。内容包括 Go 1.23、迁移 001–009、5 个 SQL 行为测试、sqlc 配置、pgx 适配器、OpenAPI（12 个 operation），以及 ADR-070/071。
- 它自带的验收报告写明：PG18 迁移、RLS/角色测试、`sqlc generate`、pgx 编译这几项全是 **BLOCKED / NOT RUN**（ChatGPT 沙箱里没有 PG、Docker、sqlc，也没有网络）。只有离线检查跑过，包括 `go test -race`、`go vet`、静态契约检查和假 psql 编排测试。
- **2026-10-12 已导入（PR #2）**：Codex S1 做了安全审查，结论 IMPORT_WITH_FIXES；在真实 PG18.6 上 gate 1–3 全部通过，GitHub CI 也是绿的，Codex R2 独立复跑同样通过。证据见 [`docs/PR01A_ACCEPTANCE.md`](../PR01A_ACCEPTANCE.md)。S1 列出的 SQL P1 问题交给 010_security_hardening（PR-01B）处理。

**F-02 已关闭**：附件包已全部找回。

**F-22 先取默认值**：代码包里自带 Apache-2.0 LICENSE。代码随 PR-01A 导入时就沿用这个许可证，owner 想换可以另提。

## 结论

大方向成立，可以作为 V1 的设计输入：模块化单体、PostgreSQL 作唯一事实源、pgx+sqlc 不用 ORM、River、默认 LocalFS、外部能力走 adapter、可选基础设施不作默认依赖。
**但它还不能直接当实施基线用**，现行 P0 有 3 个：PR-01 基线代码从未在真实 PG18 上验收（F-01，已改判）；示例 DDL 有两处会在真实 PG18 上出错（F-03 / F-04）。F-02 已关闭。下表保留原始审查结论，被状态更新取代的行已标注。

## 全部发现

| ID | 级 | 报告行 | 问题 | 来源 |
|---|---|---|---|---|
| F-01 | **P0** | 5–30, 2299, 2511 | ~~找不到任何对应工件~~（已被状态更新取代）。**现状**：V0.7 / PR-01 代码包已从 ChatGPT 对话取回（迁移 001–009、5 个 SQL 行为测试、sqlc、pgx 适配器、ADR-070/071），但真实 PG18 迁移、RLS/角色测试、`sqlc generate`、pgx 编译**一项都没跑过**，代码也还没进仓库 | Claude + A01 |
| F-02 | ~~P0~~ 已关闭 | 152–156, 278, 2557–2559 | ~~附件从未交付~~。3 个代码包和 4 份报告已从原对话取回：报告原文和代码包 sha256 见 `docs/history/artifacts/`；代码包本身未入仓，走 PR-01A 导入。架构 PNG 没有找回，以 mermaid 为准 | A02 |
| F-03 | **P0** | 1063–1067 | `payments` 表的 `UNIQUE NULLS NOT DISTINCT (tenant_id, provider, provider_payment_id)` 有问题。payment 在 `created` 状态时还没有 provider 引用（`provider_payment_id` 为 NULL），同一租户、同一 provider 下第二笔待创建的支付就会撞唯一约束，并发结账直接失败 | Claude |
| F-04 | **P0** | 1235–1248 | RLS 模板里的 `current_setting('app.tenant_id', true)::uuid`：连接池复用的连接上，`SET LOCAL` 在事务结束后会把值复位为空串 `''`，`''::uuid` 直接报错。Humaux 项目 2026-08-25 在 PG18.6 上实测过这个行为 | Claude |
| F-05 | P1 | 1030–1212 | DDL 缺约束：`captured<=amount`、`refunded<=captured`、`on_hand>=reserved`（available 不变量）、`net=gross-fee+adjustment`、`period_end>period_start`；`payments.order_id`、`shipments.order_id`、`approval_requests.policy_id` 缺 `(tenant_id, …)` 复合外键；`tax_rules` 缺版本唯一键；RLS 只示范了一张表 | Claude |
| F-06 | P1 | 1225–1231 | 「借贷平衡」只写成了一句 `SUM(debit)=SUM(credit)`，没有给出 DB 层的强制手段 | Claude |
| F-07 | P1 | 1057, 1291–1319 | 幂等键只在 `payments` 表里按租户唯一。应改成统一的 `(tenant, operation, key)` 加请求哈希：同一个 key 换了请求体要返回 422。OpenAPI 片段也缺 401/403/422 和 `components` 定义 | Claude |
| F-08 | P1 | 124–134, 2328–2348 | idempotency、inbox、outbox、audit 被定为共享平台能力，但 20 个 PR 里没有一个负责建它们 | C03 |
| F-09 | P1 | 22, 1898–1928 | 没区分店铺顾客、后台员工、B2B 公司成员三类身份。customers/addresses 模块缺失 | C01 |
| F-10 | P1 | 202–212, 294–321, 491–508 | mermaid 图、目录结构、16 项能力表三处的模块归属对不上；checkout 编排没有落点 | C02 |
| F-11 | P1 | 670–698, 2344 | 账期/应收（PR-10）排在账本（PR-16）之前，但应收本身就需要记账 | C04 |
| F-12 | P1 | 493–497 | 缺履约（拣货、部分发货、取消）、退货退款 RMA、发票冲销、通知模板、对外 webhook、导入导出、GDPR 数据权利 | C07, C08 |
| F-13 | P1 | 138–146 | V1 没有 MVP 切线，16 项能力全进 V1，规模过大 | C10 |
| F-14 | P1 | 1233–1274 | RLS 只讲了 owner 和 BYPASSRLS。还缺 SUPERUSER、TRUNCATE、SECURITY DEFINER 的 search_path 审计，也缺「同一个 pgx.Tx 不能回落到连接池」的契约 | D01, D02 |
| F-15 | P1 | 1875–1896 | API Key 没给哈希规格（256 位随机 + SHA-256/HMAC + 恒时比较）；示例写成 `osk_live`，环境前缀本身不构成隔离 | D03 |
| F-16 | P1 | 1862–1871 | 缺会话轮换、cookie 属性、CSRF、CORS 契约；撤权后需要立即失效 | D04 |
| F-17 | P1 | 1827–1858 | SSRF：做完 IP 校验后，连接必须固定到这个已验证的 IP；还没覆盖 IPv6 ULA 和 IPv4-mapped 地址 | D05 |
| F-18 | P1 | 560–575 | webhook 要对原始 body 验签，并设时间容差；`provider account → tenant` 的映射必须可信 | D06 |
| F-19 | P1 | 2197–2243 | 备份缺 WAL 归档新鲜度监控和 DB/blob 一致切点；restore drill 没有成为 gate | D08 |
| F-20 | P1 | 2011–2067 | Agent 的 A0–A4 分级没有绑定到执行：审批应绑定命令哈希、资源版本和期限，单次消费 | D10 |
| F-21 | P1 | 411, 927–935 | 没区分 River Community 和 Pro 的能力（工作流、并发限制、死信队列等是 Pro-only） | A05 |
| F-22 | P1 → 默认值 | 2265–2293 | 项目自身许可证：代码包自带 Apache-2.0，随 PR-01A 导入时启用；owner 可以另行更换 | D11 |
| F-23 | P1 | 1, 403 | 命名冲突：GitHub 上已有 `opensite-ai` 组织，npm 上已有 `@opensite/ui` 包 | D13 |
| F-24 | P2 | 2075–2101 | 内存预算上端约 8.25GB，超过 8GB 机器。Admin/Builder 应该静态构建、由 Caddy 托管，不需要常驻 Node | D07 |
| F-25 | P2 | 1620–1693 | 截图基线需要锁定镜像、浏览器、字体哈希、locale 和时区 | D09, A07 |
| F-26 | P2 | 413–416, 468–469 | 许可证核对停在源码层面，没细化到发行物和版本（Meilisearch 的 MIT/BUSL 分区、Elastic 发行版用的是 ELv2） | A06 |

## 根因 → 修复 → 验证（P0）

- **F-01**（已改判）
  - 根因：ChatGPT 沙箱里没有 PostgreSQL、Docker、sqlc，也没有网络，所以只跑了离线检查。
  - 修复：先对代码包做安全审查，再导入仓库（PR-01A）；然后在真实 PG18 上把 9 份迁移跑两遍，跑 5 个 SQL 行为测试、`sqlc generate` 和 pgx 编译。
  - 验证：干净 checkout 后，上述全部通过，日志绑定 SHA。
- **F-02**（已关闭）：附件已从原对话取回，见 `docs/history/artifacts/`。
- **F-03**
  - 修复：改为 `CREATE UNIQUE INDEX … ON payments(tenant_id, provider, provider_payment_id) WHERE provider_payment_id IS NOT NULL`。
  - 验证：同一租户、同一 provider 下并发插入两笔 `created` 支付都能成功；重复的 provider 引用插入要失败。
- **F-04**
  - 修复：policy 改成 `tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid`。
  - 验证：同一连接上先跑租户 A 的事务，再跑一个不设租户的事务，结果应为 0 行且不报错。

## 修正后的 PR 序列（采纳 C 路，Claude 调整）

| 阶段 | 内容 |
|---|---|
| 0 | 代码包安全审查 → 导入 → 01B 锁工具链 → 01A PG18 真实验收 → 02 身份/租户 → 03 模块边界 lint |
| 1 | **F 平台**（idempotency / inbox / outbox / audit + River 同事务入队）· customers/addresses · B2B 公司 · Catalog/CMS 契约 |
| 2 | PR-04 拆成 Money → Pricing → Cart/Orders · 05 BlobStore · 07 设计系统 · Astro 骨架 |
| 3 | 06 发布 · 08 询盘 · 09 库存 · 12 税引擎 |
| 4 | 16a 单币账本 → 10b 账期 → 11 审批 · 13 物流 · checkout 编排 → 14 Stripe |
| 5 | 履约 / RMA / 发票 / 通知 / 数据权利 → **首发验收** |
| V1.x | 15 Adyen · 16b FX · 17 Marketplace · 18 插件 · 19 Yjs · 20 可选基础设施（拆成子 PR） |

**MVP 切线**：单站、单语言、单币、单仓（数据模型保留 `warehouse_id`）。B2C 访客和顾客购买；B2B 支持询价、报价、单层审批、预付。支付用 Stripe，税用区域规则，手工履约，存储用 LocalFS。
**首发 gate**：两条购买路径 + 退款/取消 + 宕机恢复 + 租户隔离 + 备份恢复，在同一个 SHA 上全绿。

## 系统性问题

1. **主张和证据混在一起写。** 报告把「已有」「应有」「建议」写在同一张表里。从本仓库起，每条现状描述都要带证据级别（E0–E4）和工件指针。
2. **gate 只停留在清单上。** 报告列了很多 gate，但没有一条是可执行的命令和失败条件。新 gate 要留下一次「注错 → 红 → 修 → 绿」的记录，才算存在。
3. **按能力排路线，没有按发布闭环排。** 发布单位应该是「购买 → 履约 → 售后」这样的闭环，而不是 16 项能力。

## 待 owner 决断

- 开源许可证（F-22）：默认采用代码包自带的 Apache-2.0，随 PR-01A 导入。owner 如果想要 AGPL-3.0（网络服务修改必须回馈），在导入前提出。
- 命名（F-23）：是否沿用 OpenSite 这个名字，npm scope 怎么定。
