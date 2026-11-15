# Sourcyo Apparel 基础设施事实

## Supabase 项目区域

- 查询日期：2026-11-10（UTC）。
- 来源接口：Supabase 管理 API `GET https://api.supabase.com/v1/projects/{ref}`。
- 核查方式：生产只读核查，接口返回 HTTP 200；响应字段 `region` 的值为 `us-east-1`（美国区域）。
- 用途：支持八语隐私页中“询盘记录存储于 Supabase 美国区”的区域表述；页面正文仅写国家，不展示区域代号。

仅保留与本事实相关的脱敏响应字段：

```json
{"region":"us-east-1"}
```

证据定位：`work/apparel-b2b/fleet-20261104/launch-20261110/l4-REPORT.md` 的“Supabase：数据库检查阻塞”节；该核查记录的 SHA-256 为 `b4e251a174fa93acce879a390bafa5fb815af2699ec62fced4a155bc30676e4f`。本条区域事实来自管理 API 回执，而非隐私文案或环境变量名称的推断。

范围限制：同次核查记录的项目状态为 `INACTIVE`，数据库检查受阻；区域字段不证明项目已恢复、询盘写入可用、数据表存在或 RLS 验收通过。这是上述查询日期的区域记录，不是持续健康保证。

本文件不记录实际项目 ref、项目 URL、凭据或任何密钥。本次只补充事实依据，不修改隐私页措辞或生产配置。

## 法务参数的批准出处

八语 privacy / terms 的六项参数以 `docs/apparel-b2b/owner-input.md` 末尾“法务参数（2026-11-10 业主批准）”一节为权威来源，业主原话为“6 项法务参数，按建议”。该来源快照 SHA-256：`93160e212b28b2e359f3aaa92edb03975e990d448756a8ad461ed09c396ba40a`。本节仅定位出处，批准值在 owner-input 中维护，不在此复制为第二份权威。页面文字更新不证明 GA4 后台保存设置、Cookie 配置或数据库清理机制已经部署或生效。
