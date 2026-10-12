# C-02a：全局员工认证与离线初始化

2026-10-12 编排者最终决策（Humaux：`[decision] OpenSite C-02a：员工认证数据为全局表（沿用 001 users/auth_sessions 先例），不引入「身份托管租户」`）取代原托管租户提案。本说明补充 [A3/A4](APP_LAYER_DECISIONS.md#a3--staff-authentication-sessions-api-keys-db-pools)。

`auth_account_attempts`、`auth_login_attempts`、`known_devices` 是全局身份数据，不带 `tenant_id` 或租户 RLS；已知设备直接引用 `users(id)`。只有受信任 `app_auth` 获得所需列权限，其余应用角色与 PUBLIC 的表级和列级权限为零；精确矩阵与真实 SET ROLE 负测共同验证。`api_keys` 保留业务租户绑定，公开 id 另有全局唯一索引。

失败记账先取得账号事务锁，在锁后使用数据库事务时钟，再按账号→账号×IP 顺序各执行一次 UPSERT RETURNING。15 分钟窗口，计数饱和到 1,000,000；账号退避为 1、2、4、8、16、32、60 秒，账号×IP 第 5 次失败进入 60 秒冷却。生效中的冷却保留原到期时间；拒绝请求不得重新记为一次密码失败。计数与设备失败函数不接受调用方时间，锁后使用数据库 now()；测试通过管理员回拨 fixture 行验证窗口和冷却到期。设备 5 次失败自动撤销，撤销不可逆。

`app_credential_writer` 是无登录、无成员的函数 owner；离线专用 LOGIN 仅持有 execute-only `app_admin_bootstrap`，不能兼任 migrator 或其它应用角色。bootstrap 函数固定 search_path、撤销 PUBLIC EXECUTE，安装级事务锁之后跨租户检查任何 owner；只接受 READ COMMITTED，以避免等待锁后读取旧快照。一次事务创建用户、真实业务租户与 owner 关系；提前生成租户 UUID 并设置函数作用域的租户上下文，让 customers 的 buyer_roles 触发器在精确列授权下初始化默认角色；返回前恢复调用方租户上下文。已有 owner 时拒绝，不覆盖密码。

CLI 密码只取重定向 stdin 或 `OSC_BOOTSTRAP_PASSWORD`；DSN 取 `OSC_BOOTSTRAP_DATABASE_URL`（或 `DATABASE_URL`），不回显密码/PHC/DSN/驱动错误。使用钉版本 x/crypto 的 Argon2id PHC（m=19456 KiB、t=2、p=1；16 字节随机 salt、32 字节结果）。

本卡提供持久化原语和离线 CLI，尚未提供登录 HTTP、dummy-hash 等时路径、cookie 签名/轮换、全局 IP 层、已知设备豁免、三池或 API key 验证。02b 必须用固定 arrival+cap 截止时间证明持续攻击下新设备不会反复排队；60 秒数据库退避上限本身不等于该验收完成。受限 app.auth_prune_attempts(limit) 清理 24 小时未失败、15 分钟窗口与冷却均已到期的计数行；每表单次至多 1000 行，锁繁忙账号跳过并在锁后复核，app_auth 无直接 DELETE 权限。部署定时任务应每小时调用并按返回数量继续小批清理；本卡提供能力与并发 gate，调度器接线留给运维卡。PR #28 已实现每 IP 限流、IPv6 /64 聚合、已知设备冷却豁免与独立保留哈希槽。

验收：`db_identity_auth.sql`、精确 `db_role_matrix.sql`、advisory-lock 两连接 +2 竞态、应用全部前置迁移后的存量升级、真实专用 LOGIN 的 CLI 成功/已有 owner 拒绝/并发唯一、至少两项注错红→恢复绿，以及卡片要求的全量离线/Go/PG18 gate。测试按迁移逻辑后缀发现文件，集成可重编号。
