# PR-01A：PostgreSQL 18 真实验收记录

- 日期：2026-10-12
- 代码来源：`opensite-commerce-pr01-acceptance-gated.zip`（sha256 `c0be392b…`，见 `docs/history/artifacts/SHA256SUMS.zips`）
- 导入前安全审查：Codex S1，结论 IMPORT_WITH_FIXES，未发现新的 P0
- 运行环境：
  - 数据库：Docker `postgres:18` → PostgreSQL 18.6（aarch64），一次性 compose 项目，跑完已用 `down -v` 销毁
  - Go 1.26.2
  - sqlc v1.31.1（源码 `go install …@v1.31.1` 编译）
  - pgx v5.10.0

| Gate | 命令 | 结果 | 日志 |
|---|---|---|---|
| 1 PG18 schema + RLS/FK/角色 | `scripts/check_pg.sh`（9 份迁移跑两遍 + 5 个 SQL 行为测试） | **PASS**（exit 0，4 个 PASS 断言，0 个 ERROR） | [gate1.log](evidence/pr01a/gate1.log) |
| 1 注错 → 红 → 绿 | 对 `app.tenant_memberships` 执行 `NO FORCE ROW LEVEL SECURITY` 后跑 `db_security_audit.sql`，再恢复重跑 | 注错后 **红**（exit 3，`Tenant table missing ENABLE/FORCE RLS`）→ 恢复后 **绿** | [inject.log](evidence/pr01a/inject.log) |
| 2 sqlc generate + pgx 编译 | `scripts/check_codegen.sh` | **PASS**（生成 `internal/db/`，带 `-tags pgx` 的 test 和 vet 都通过） | [gate2.log](evidence/pr01a/gate2.log) |
| 3 离线检查 | `scripts/check_all.sh` | **PASS** | [gate3.log](evidence/pr01a/gate3.log) |
| race | `go test -race -tags pgx -count=1 ./...` | **PASS** | 本地运行 |

## 本 PR 相对代码包的改动

- `go.mod`：`go` 版本提到 1.25.0，钉住 pgx v5.10.0；新增 `go.sum`。
- 新增 `internal/db/`：由 sqlc v1.31.1 生成。
- `scripts/check_all.sh`：`python` 改为 `python3`。macOS 默认没有 `python`，原写法会让 gate 3 以 exit 127 失败。

## NOT_RUN 与已知缺口（交给后续卡）

- **CI 还没在 GitHub 上跑过**：`.github/workflows/checks.yml` 用的是浮动版本，还在 CI 里执行 `go get`，要按 C1 方案重写（PR-01B）。
- **模块路径**仍是 `example.com/opensite-commerce`，PR-01B 改。
- **镜像没钉 digest**：`deploy/compose.yaml` 写的是 `postgres:18`，没有钉 `18.6@sha256`。
- **S1 列出的 P1 还没修**，对应模块接入生产前必须修：
  - runtime 角色能写成员角色、域名验证状态、媒体 ready/public（008:43–50）；
  - 已发布 release 的子项仍然可以追加（006:18–51）；
  - 既有角色不收敛，审计漏掉高权限属性和权限矩阵（009:6–11）。
- **还缺的测试**：
  - 同一连接上 SET LOCAL 后空串复用的测试（F-04：静态写法已经是 `nullif`）；
  - 迁移失败时的回滚测试；
  - 仓库路径含空格时的测试（这次在含空格的路径下跑通了，但没有专门的用例）。
