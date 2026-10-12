## 全部发现

A01 基线E0；A02 附件E0；A03 发布残留；A04 版本下限缺失；A05 Pro边界未列；A06 许可范围不足；A07 工具能力未形成验收gate。

## 分析

**A01 · P1 · L5–30、82、88、136、927、992、2299、2511**  
问题：以下均为E0：V0.7/PR-01可审计、001–009存在、ADR-070已接受、pgx/sqlc现状、RLS policy/harness、Identity/Catalog/CMS表、LocalFS/media ID方向、约12个API、增强CI、已有PG+River、既有OpenAPI方向。“Commerce未实现”、Compose现状、历史BLOCKED及“从未通过”也未核实。  
根因：附件叙述当成事实。建议修复：统一标“待核实”。验收gate：补工件、SHA、ADR记录及绑定SHA的测试日志。

**A02 · P1 · L152–156、278、2557–2559**  
问题：“工程包已整理/生成”为E0。失效附件清单：L154/2559的 `opensite-commerce-v1-architecture-pack.zip`；L278的 `opensite-commerce-v1-target-architecture.png`，均使用 `sandbox:/mnt/data/`。  
根因：沙箱产物未交付。建议修复：补交或删除主张。验收gate：解压、清单/哈希核对、PNG渲染通过。

**A03 · P1 · 下列42行**  
问题：42块引用控制标记、66次ID不能独立追源；并非字面“citeturn”。完整行号：18,32,122,179,390,392,411,413,414,415,416,417,418,463,468,469,521,586,614,737,826,887,953,967,992,1276,1586,1620,1622,1624,1763,1813,1858,2185,2187,2211,2286–2291。  
根因：引用导出失效。建议修复：换官方URL及核验日期。验收gate：控制标记、sandbox链接为零；来源支持对应主张。

**A04 · P2 · L18、1017–1157**  
问题：事实成立，兼容下限未注明：[PG18受支持](https://www.postgresql.org/support/versioning/)；官方文档仍列19为开发线；[uuidv7()](https://www.postgresql.org/docs/18/functions-uuid.html)为PG18原生；[UNIQUE NULLS NOT DISTINCT](https://www.postgresql.org/docs/15/release-15.html)自PG15引入。  
根因：选型缺版本契约。建议修复：注明这些默认DDL要求PG18。验收gate：干净PG18运行DDL及约束测试。

**A05 · P1 · L179、411、927–935**  
问题：[River OSS为MPL-2.0](https://github.com/riverqueue/river/blob/master/LICENSE)，基础定时任务可用；[Pro-only](https://riverqueue.com/docs/pro)包括工作流、序列、持久周期任务、并发限制、批处理、加密、死信队列、完成即删、每队列保留期。报告未列功能映射。  
根因：能力未分版。建议修复：补Community/Pro矩阵。验收gate：无Pro依赖运行，事务回滚、重启、重复执行测试通过。

**A06 · P1 · L413–416、468–469、887、2286–2291**  
问题：许可主张多数正确，但范围未落实：

- [RustFS](https://github.com/rustfs/rustfs/blob/main/LICENSE)、[SeaweedFS](https://github.com/seaweedfs/seaweedfs/blob/master/LICENSE) OSS均Apache-2.0。
- [Valkey](https://github.com/valkey-io/valkey/blob/unstable/COPYING)为BSD-3-Clause；[Redis 8](https://redis.io/legal/licenses/)为RSALv2/SSPLv1/AGPLv3三选一。
- [Yjs](https://github.com/yjs/yjs/blob/main/LICENSE)为MIT。
- [Meilisearch](https://github.com/meilisearch/meilisearch/blob/main/LICENSE)是MIT与BUSL-1.1分区，不是整仓任选许可。
- [Elastic](https://www.elastic.co/pricing/faq/licensing)部分源码可选AGPLv3/SSPL/ELv2，官方发行版仍为ELv2。

根因：源码许可代替发行许可。建议修复：锁版本、CE/EE、文件范围及发行物。验收gate：实际SBOM、功能与许可清单一致。

**A07 · P1 · L386–403、1620–1693**  
问题：[React Aria](https://react-aria.adobe.com/getting-started)定位正确，提供无默认样式的可访问组件及hooks；[toHaveScreenshot](https://playwright.dev/docs/test-snapshots)支持截图回归，需一致环境；[Storybook a11y](https://storybook.js.org/docs/writing-tests/accessibility-testing)基于axe，当前集成需runner及 `a11y.test='error'` 才阻断CI。报告未交付配置。  
根因：能力代替证据。建议修复：补可执行配置、固定环境及人工检查边界。验收gate：注入视觉差异/a11y违规→红→修复→绿，绑定SHA。

## 系统性问题

缺少“主张→工件/版本→证据→gate”追溯链。外部产品能力不能证明OpenSite已实现；当前工程基线主张为E0，运行验收为NOT_RUN。

扫描文件SHA256：`41e9af3142947aa9600ac9e7b2c69c728bfdc1ca6b7c18464c5c51ff88b922a5`。