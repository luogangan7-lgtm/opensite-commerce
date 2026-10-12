# Strict OpenAPI transport POC

This package is generated from `api/openapi.yaml` with the Go tool pinned in
`go.mod`: oapi-codegen **v2.8.0**, runtime **v1.6.0**, nullable **v1.2.0**.
`api/oapi-codegen.yaml` selects models, strict-server, std-http-server, embedded
spec and three-state nullable fields. Do not hand-edit `api.gen.go`.

```sh
go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
python3 scripts/check_oapi_codegen.py
go test -race ./internal/platform/httpapi/generated ./internal/bootstrap
```

The POC proves compilation and HTTP response mapping for stdlib ServeMux,
OAS 3.1 `oneOf` discriminators/const schema validation, optional nullable
absence/null/value preservation, and typed `application/problem+json` responses.
`compatibility_test.go` also proves the existing platform `DecodeJSON` rejects
unknown fields and second JSON values for generated DTOs, and `Schema.VisitJSON`
rejects invalid union branches and undeclared nested fields.

Strict interfaces constrain request/response envelopes; the generator's ordinary
JSON decoder does **not** enforce full schema constraints or authentication.
Future transports must keep RouteSpec auth, transport limits and strict JSON
admission before dispatch, plus schema validation for unions/custom unmarshallers.
Schema constraint failures map to 422; transport/parser failures use the platform
problem mapper. Default generator error callbacks return text errors and must be
replaced with `httpapi.WriteProblem` when real transports are wired. Never mount
`generated.Handler` directly as a production bypass of the RouteSpec router.

Only health is implemented today. Generated planned identity/business methods
are POC scaffolding, never registered by bootstrap. PR-02 implements sessions;
this card neither implements credentials nor authenticates requests. Existing
legacy dev-api remains diagnostic and is excluded from the production route set.

`Router.RouteSpecs()` reads actual successful registrations. Bootstrap contract
tests reconcile their operation IDs, methods, paths, access classes and scopes
with implemented operations in the source OpenAPI in both directions. PR #18's
module registry reconciliation is complementary and was still open at this base.

`createOrganization` creates a seller company **within an existing tenant**
(`002_site.sql`), so it also uses the tenant path and `organization.create`.
Private business operations use cookie **or** tenant-bound bearer; self operations
need only the caller's active session/user. Cookie writes require exact Origin
plus signed CSRF; bearer calls bypass CSRF only after selecting and validating
bearer authentication. No anonymous owner or tenant bootstrap is introduced.
