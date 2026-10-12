## 全部发现

均为设计缺口，未确认 P0；实现 gate 全部 **NOT_RUN**。

|ID|级别|报告行号|问题|
|---|---|---|---|
|D01|P1|1233–1274|RLS 绕过能力审计不足|
|D02|P1|1251–1261|租户与事务连接契约不完整|
|D03|P1|1875–1896|API Key 安全规格缺失|
|D04|P1|1862–1871、2330|浏览器鉴权及撤权缺合同|
|D05|P1|1827–1858|SSRF 网络合同不全|
|D06|P1|560–575、1767–1798|支付安全缺完整 gate|
|D07|P2|2075–2101|内存及进程预算不闭合|
|D08|P1|2197–2243|恢复合同不完整|
|D09|P2|1620、1655–1673|截图环境未锁定|
|D10|P1|2011–2067|Agent 执行合同缺失|
|D11|P1|2265–2293|项目自身许可证未选|
|D12|P2|2440–2505|披露政策与贡献流程缺失|
|D13|P2|1、403|公共命名冲突风险|

## 分析

- **D01** 根因：ACL 漏审绕过能力。修复：禁超级用户、高权成员、TRUNCATE；SECURITY DEFINER 限定 owner、search_path、EXECUTE。gate：真实运行角色审计及跨租户绕过负测。[PG](https://www.postgresql.org/docs/18/ddl-rowsecurity.html)
- **D02** 根因：未保证可信 tenant 与同事务连接。修复：认证 membership→tenant→同一 pgx.Tx，禁止回落 pool。gate：单连接 A→B、回滚/取消/缺 tenant、篡改租户负测；HTTP 与 worker 均覆盖。
- **D03** 根因：缺密码学规格。修复：256位随机 secret、SHA-256/HMAC-SHA-256、恒时比较；prefix 仅索引，绑定环境，示例用 `osk_test`。gate：错 secret/环境拒绝，未知 prefix 同类响应，数据库/日志无完整 key。`osk_live` 字样本身不构成环境隔离。
- **D04** 根因：身份与浏览器契约空白。修复：权限交集取当前 membership，撤权即失效；会话轮换/注销，cookie 明确 HttpOnly/Secure/SameSite/host-only，补 CSRF 与精确 CORS。gate：撤权、过期、跨站及恶意兄弟域写操作负测。
- **D05** 根因：最终 IP 校验未绑定拨号机制。修复：连接固定到已验证 IP，补 IPv6 ULA/映射地址、代理及重定向凭证规则。gate：rebinding、IPv6、代理、重定向攻击均不能连接禁区；须证明没有发出连接。
- **D06** 根因：未覆盖完整支付数据路径。修复：原始 body 验签、provider 时间容差、account/mode→tenant 可信映射；PAN/CVV 禁入服务与日志。gate：篡改/陈旧/错账户拒绝，合法重试仅一次效果，伪卡数据泄漏测试。[Stripe](https://docs.stripe.com/webhooks)
- **D07** 根因：预算上端约8.25GB，未留同时运行峰值。修复：留构建/备份余量；Admin/Builder 优先静态构建由 Caddy 托管，Astro 动态路由保留对应 SSR 运行时。React Admin 不天然需要常驻 Node。gate：4核8GB约定负载叠加备份/发布无 OOM；停 Admin Node 后深链、登录及权限拒绝通过。
- **D08** 根因：缺归档新鲜度与恢复集合同。修复：限制低流量 WAL 切段+异机落盘延迟；定义 DB/blob 切点、配置凭证和 RTO 起止。gate：仅异机副本恢复新环境，RPO≤5分、业务恢复≤60分，必要媒体引用完整；归档失败告警。[PG](https://www.postgresql.org/docs/18/continuous-archiving.html)
- **D09** 根因：“一致环境”未版本化。修复：锁镜像/browser/字体哈希、locale、时区、viewport；验证字体加载，多平台分基线。gate：同 SHA 干净重跑稳定，缺字体注错必红，键盘焦点另测。React Aria 不能替代整页交互验收。
- **D10** 根因：分级/批准未绑定执行。修复：服务端注册 command→等级；批准绑定主体、租户、命令哈希、资源版本、期限，单次消费并执行前重验；明确 A3 必须人工批准的动作。gate：伪报 A2、绕过入口、批准后换参数、撤权、并发重放均拒绝且无副作用。无数据库写权限仍可能通过 Domain API 破坏业务。
- **D11** 根因：只审依赖，未决定商业授权。修复：Apache/MIT 可闭源商业分发；Apache 有显式专利授权，MIT 无该明确条款；AGPL 修改版网络服务须供对应源码，仍允许商用。按商业集成或网络修改回馈目标选型。gate：LICENSE/SPDX、兼容性与分发义务检查。[AGPL](https://opensource.org/license/agpl-3.0)、[Apache](https://www.apache.org/licenses/LICENSE-2.0)、[MIT](https://opensource.org/license/mit)
- **D12** 根因：架构 SECURITY 未定义社区政策。修复：根 SECURITY.md 写支持版、私报渠道、响应窗口；CONTRIBUTING 写 gate、审查与版权授权。gate：GitHub 可发现披露政策，外部 clean checkout 贡献流程跑通；双授权方案另核贡献版权。
- **D13** 根因：命名空间归属未核验。[GitHub 已有 OpenSite AI](https://github.com/opensite-ai)，[npm 已有 @opensite/ui](https://www.npmjs.com/package/@opensite/ui)。修复：核实归属或选独立 scope，另做目标市场商标检索。gate：包名与命名审查凭证。裸名可用性、商标注册未确认；占用不能直接判为侵权。

## 系统性问题

原则、拟议 gate 与工程事实混写。L11–30、154、2557 的代码、ADR、迁移及附件均未验证，不能作为验收前提。gate 必须落成命令、失败条件及当前 SHA/哈希证据，并记录注错红→修复绿；清单或流程图不能证明门禁存在。