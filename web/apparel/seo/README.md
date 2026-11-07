# R09 八语 SEO/GEO 离线契约

此模块只提供契约与 fixture 验证，不接入页面，不表示生产可索引。
EN 无前缀；zh 路径对应 zh-Hans；八语每页 canonical 自指，互链完整且 x-default 指向 EN 同一路径。query/hash 不属于 canonical 或 sitemap 输入。生成 sitemap 为 URL 清单，发布阶段另渲染 XML 并对实际 HTML、HTTP 和 XML 复核。

来源：项目路线图 R09、R07 路由盘点及 R04 sku-source-delta.json（快照同目录）。仅以其中 slug 作为目录身份，不从 slug、fabric_id、生成图片、尺码表映射推断材质/克重/质量/销售包装。结构化数据仅有 Product 身份和 URL；不满足 rich-result 全字段条件，不声称具备富结果资格。所有新增事实字段须经来源核验并修改契约，UNKNOWN 不输出。现阶段不输出名称、描述、价格、库存、评分、图片或质量断言。

GEO 验收限事实一致性和可审计来源，不承诺 AI 引用、排名或收录。当前 fixture 含 R04 全115身份，实际发布目录仅68款；集成前必须按发布白名单求交集，不得直接将115身份上线。只生成首页、集合入口、Contact 和产品 fixture；About、未定类目及待定编辑页均不纳入本卡。生成示意可以用于展示，但不能充当事实证据。

运行 `bun test web/apparel/seo`。集成门禁：当前发布白名单、真实八语正文、HTML canonical/alternate、真实站点图互相对应；预览继续 noindex 且不进入正式站点图；正式域名与 index 开关另卡授权；独立复核后才能晋升。

构建默认 SEO_BUILD_MODE=preview：占位域、HTML noindex、全局 X-Robots-Tag noindex、robots Disallow 必须同时存在。
正式构建使用 `SEO_BUILD_MODE=production PRODUCTION_ORIGIN=<owner提供的https源> bun run --cwd web/apparel build`，随后同环境运行 dist gates；不提供域或含 invalid/localhost/pages.dev 即失败。生产产物不得含占位域、noindex 或 Disallow。此入口不构成上线授权。
CI 默认预览，seo-probes 在构建后证明占位域 production 探针及各保护移除会红；生产绿灯只使用临时合成 dist 与测试域，不作为正式域事实。
