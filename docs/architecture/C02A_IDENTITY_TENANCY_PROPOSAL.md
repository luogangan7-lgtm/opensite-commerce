# C-02a — 认证数据租户归属提案

**状态：待架构确认，尚未实施。** 基线 `0ac7b7bd4c3e10a7c07f45cee57b0bf0a3564b98`，2026-10-12。本提案不改变 001–010；确认后由 C-02a 新增前向迁移实施，当前下一空号为 014，最终编号由集成者分配。

## 冲突与约束

- [A3](APP_LAYER_DECISIONS.md#a3--staff-authentication-sessions-api-keys-db-pools) 要求全局员工账号登录；认证时尚未解析目标业务租户。不存在的规范化标识符也必须进入持久化账号及账号+IP 计数。
- [A4](APP_LAYER_DECISIONS.md#a4--tenant-resolution-and-authorization) 在认证后从 URL 解析目标租户。身份、会话不绑定某一个业务租户。
- 产品负责人已明确：本卡新表不得采用无租户键例外，必须有 `tenant_id`、复合主键/关联、ENABLE + FORCE RLS；先补设计决策。
- `001_identity.sql` 中 `users` 只有全局 `id`，没有可供设备记录引用的 `(tenant_id,user_id)` 父键。不能把用户的某个业务 membership 偷换成全局身份归属。

## 方案比较

| 方案 | A3/A4 与隔离 | 代价 | 结论 |
|---|---|---|---|
| 固定的真实身份数据托管租户，用户增加身份归属列 | 保持全局登录；业务授权仍取 URL；所有新认证表有真实租户键和复合关联 | 前向补用户列；存量库须明确托管租户与回填；托管租户不能被普通业务流程删除 | 建议采用，待确认 |
| 登录先选择业务租户，分别保存账号计数 | 满足租户键，但轮换租户可重置额度；设备和跨租户员工登录语义改变 | 必须修改 A3/A4、登录契约及防枚举设计；仍需另解全局账号额度 | 不作为等价实现 |
| 无租户键全局认证表 | 贴近原始 A3 | 不满足本卡新增表约束 | 已被负责人拒绝 |

不使用全零 UUID、没有真实租户行的哨兵键，也不自动给全部员工授予某个业务租户的会员权限。

## 建议决策：身份数据托管与业务目标分开

1. 在真实的 `app.tenants` 行上增加 `is_identity_custodian` 标志，部分唯一索引保证至多一行被选中。它表示此安装的身份数据归属，不是请求的授权目标。
2. 空库 bootstrap 在同一事务创建实际业务租户、首个 owner，并把该租户设为身份托管租户。已有 owner 的库必须由受控迁移/运维步骤明确选定一个已有真实租户；不得自动按最小 UUID、最早创建时间或请求输入选择。
3. 为 `app.users` 前向增加 `identity_tenant_id`，外键指向托管租户；增加 `UNIQUE(identity_tenant_id,id)`。保留现有全局 `UNIQUE(email_normalized)`，员工身份和会话仍是全局的。列回填完成前不得开启真实认证写入。
4. 身份归属由数据库校验：用户和新认证表只能引用被选中的托管租户。普通 app 角色不能改变标志、归属键或复制记录到另一个租户；切换托管租户属于独立的数据迁移，不能通过配置热切换。
5. Auth pool 从受限数据库读取确定的托管租户，在自己的事务中 `SET LOCAL app.tenant_id`。缺少托管租户或发现异常时拒绝认证初始化。该值不能取自 URL、Host、请求头或未认证的客户端参数。
6. Runtime pool 仍只在 `RunAuthorized` 中设置 URL 的业务租户。Auth pool 的托管上下文不能传给业务 handler，也不能构造 `AuthorizedActor`。
7. 托管租户的业务状态与身份存储可用性分开：业务租户暂停会阻止对该租户的授权，但不能因此阻止其他业务租户员工认证或成员已撤销的用户退出登录。

部分唯一索引只保证「至多一个」，不是完整归属证明。实施时还需数据库归属校验与锁定：所有用户归属及新认证表写入必须引用已被标记的真实租户；初始化完成后禁止撤销/切换标志，FK 阻止删除；归属校验与并发标志变更不能出现检查后修改的竞态。用户列回填完成后收紧为 NOT NULL，不能把永远可空的列当已完成升级。

### 模块、表与契约

Identity 独占写入这些表；没有新增 HTTP operation，也没有跨业务模块读取私有 store。

| 表 | 键与关联 | 权限/不变量 |
|---|---|---|
| `auth_account_attempts` | PK `(tenant_id,identifier_normalized)`；真实托管租户 FK | 不引用 users，因而不存在账号也被计数；持久账号总量和有上限的退避 |
| `auth_login_attempts` | PK `(tenant_id,identifier_normalized,ip)`；复合 FK 指向账号计数 | 轮换 IP 不影响上表；每次失败单条 UPSERT RETURNING 原子加一 |
| `known_devices` | PK `(tenant_id,id)`；复合 FK `(tenant_id,user_id)` → `users(identity_tenant_id,id)`；cookie 哈希全局唯一 | 仅存哈希；绑定用户、有效期、撤销时间、失败数；不要求仍有业务 membership |

三张新表均 ENABLE + FORCE RLS，使用
`tenant_id = nullif(current_setting('app.tenant_id',true),'')::uuid`
作为 `USING` 和 `WITH CHECK`。只有受信的 `app_auth` 有精确读写权限；`app_runtime` 对这三表无任何表级或列级权限。PUBLIC 无权限。

计数公开契约为身份模块的参数化 SQL/sqlc 查询，不由 HTTP handler 拼 SQL。一次失败的账号总计和账号+IP 更新处于同一事务，按固定顺序锁定账号再更新 IP。认证路径及不存在账号的路径执行同样的持久化操作。

账号失败数及退避计算必须饱和、有最大值，不允许整数溢出或永久硬锁。已有冷却不能被每次拒绝的请求无限向后推。后续 02b 对每次新设备登录记录固定的等待截止时间，不能因为别人的新失败反复重新排队；已知设备只豁免账号总量层，仍受 IP 与账号+IP 限制。具体时间窗口/阈值由后续认证契约给出，本卡测试注入固定时钟验证边界。

### 受限 bootstrap 能力

- `app_credential_writer` 是 NOLOGIN、NOINHERIT、无入/出角色成员关系的函数 owner；无 catalog 读取、DDL、DELETE、TRUNCATE 或 runtime membership。
- 单独的 `app_admin_bootstrap` 是 execute-only NOLOGIN 能力角色。仅离线 CLI 的独立 LOGIN 身份可以持有它；迁移不创建任何 LOGIN 或密码，API 的三类 pool 不持有此能力。
- CLI 通过这个专用 LOGIN 连接，调用固定 search_path、PUBLIC 无 EXECUTE 的 SECURITY DEFINER bootstrap 函数。函数由 writer 持有，以列级权限一次性创建用户、真实租户及 owner membership；必要的托管标志/用户归属初始化也只在该事务内完成。
- 函数以安装级事务锁串行化 bootstrap，在持锁后检查任何已有 owner，发现则拒绝整个事务，不能覆盖密码。直接尝试调用函数的 runtime/auth/domain 角色均应被拒绝。
- 已有 owner 的存在性检查必须跨全部业务租户可见：writer 仅获得 memberships 的 `role` 列 SELECT 与专属只读 RLS 策略，用于固定的 owner 存在性检查；写入策略仍限制到本事务新建的租户。不得用目标租户的 RLS 查询来证明「没有任何 owner」。必须以另一租户已有 owner 的见证测试拒绝 bootstrap。
- CLI 密码只取 stdin 或环境变量，不接受明文密码 flag，不回显密码、PHC 或 DSN。选择真实 Argon2id PHC 编码，使用已钉版本的 [`golang.org/x/crypto/argon2`](https://pkg.go.dev/golang.org/x/crypto/argon2)，成本至少达到 A3 的 m=19 MiB、t=2、p=1。此处未引入依赖，也未实施编码。

这一入口解决「writer 无成员」与「CLI 必须用受限身份」的接线冲突；不让 CLI 以 superuser 连接后 SET ROLE。

## 升级、回滚与开放问题

1. 必须先应用主干中此模块之前的全部迁移，再安装新增结构；测试按逻辑文件后缀定位，不硬编码 014。
2. 建议存量部署由 migrator 在安装本迁移前明确提供 `app.identity_custodian_tenant` 配置；数据库验证 UUID 对应真实租户后，才标记并回填全部全局员工的 `identity_tenant_id`。有存量身份数据而配置缺失、不存在或与既有归属冲突时，整个迁移原子失败。空库不需要预设 UUID，由首 owner CLI 的单事务完成初始化。该配置只属于受控 migrator，不由 runtime/auth 角色或客户端提供；不是认证服务每次可变的租户参数。
3. 必须核对后续 sqlc、角色矩阵及 PR #18 注册表的实际合入状态；新表归 identity。API key 仍绑定业务 `api_keys.tenant_id`，不改为身份托管租户。
4. 回滚业务代码不删除认证数据或修改冻结迁移；结构问题用前向修复。托管租户删除受到 FK 阻止；身份域迁移另立卡，明确备份与影响面。
5. 这是 A3/A4 的补充设计候选：新增的是身份数据归属及初始化前提。HTTP 登录、会话轮换、CSRF、API key 验证、三池装配不在 C-02a 实现范围。

待确认的架构变化包括：把真实租户作为身份数据的长期托管者；为全局 users 增加归属列并规定回填及 NOT NULL 收紧；允许该 migrator 配置成为存量安装的显式前提。users 的原全局 email 唯一性及 auth-only 凭证读取不放宽，新增归属列不能让 runtime 获得用户/密码读取。

## 验收 gate（实施后必须真实执行）

| 不变量 | gate / 注错探针 |
|---|---|
| 未知账号与真实账号均受总量和 IP 计数约束 | `db_identity_auth.sql`；两个租户来源访问同一账号不能获得独立额度 |
| 原子并发两次失败恰好加二 | `check_pg_races.sh` 独立连接、advisory-lock barrier，使用未占用 gate 编号 |
| 冷却到期、退避有上限、拒绝不无限续期 | 固定时钟边界 SQL 测试；删除冷却上限保护 → 红 → 还原 → 绿 |
| 租户键不能伪造、RLS 缺/空 GUC 安全 | `SET ROLE app_auth/app_runtime`，真实 A/B 租户见证；删除 RLS/归属检查 → 红 |
| 托管状态不改变业务授权或隔离 | 托管租户暂停仍可为其他租户认证/退出；跨池上下文泄漏负测，业务事务只取 URL 的已授权目标 |
| 设备正确绑定、失败可撤销、无 membership 仍可管理身份 | 真实 PG user/device 复合 FK 正负测；移除 FK 或撤销保护 → 红 |
| public key id 全局唯一、issuer 与业务 tenant 不变 | 跨租户 key id 重复 INSERT → 唯一约束失败 |
| writer 和 bootstrap 能力隔离、没有运行期授权扩大 | 精确 table/column/function role matrix，真实 SET ROLE 负测；错误 GRANT → 红 |
| bootstrap 幂等且并发只产生一个首 owner | 真 PG + 真专用 LOGIN 的 CLI 验收；不同账号同时初始化，一个提交、一个拒绝 |
| 存量升级不漏前置迁移或错误回填 | 全前置迁移 + 用户/会员见证，再跑新迁移、SQL gate 与完整矩阵 |

最终还须在同一冻结 SHA 上执行 `check_all.sh`、Go vet、默认/pgx race、`check_codegen.sh` 及三个真实 PostgreSQL 18.6 gate，记录至少两次注错的红→还原→绿。此提案只有 **E1 静态设计证据**；实现、所有运行 gate、red→green 和独立复核均 **NOT_RUN**。
