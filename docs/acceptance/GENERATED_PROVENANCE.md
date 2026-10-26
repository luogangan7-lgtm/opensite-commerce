# Generated artifact provenance — PR #44 / C-22a

Source history is not rewritten. This note records the exact mapping that the
PR #44 body did not name at import time; earlier receipts stay historical.

## Imported output

- Path: `internal/platform/httpapi/generated/api.gen.go`, commit
  `65c35362d9964d16bc7f6fca8e3cad1d22df7499` (ancestor of main `cab64f2e`).
- Import shape: `git show --stat 65c35362` reports 1,505 changed lines in
  `api.gen.go` plus 32 lines in `tests/test_privacy_registry.py`; no other path.
- Input source: the commit's parent `c9a050a9` ("Merge origin/main into C-22a
  data rights (CI generator refresh follows)"). `65c35362` changes no OpenAPI
  input, so the generator input is the tree at `65c35362` itself.
- Official generator: `github.com/oapi-codegen/oapi-codegen/v2 v2.8.0`
  (`go.mod:26`; tool directive `go.mod:48`), invoked with
  `api/oapi-codegen.yaml`, whose output is
  `internal/platform/httpapi/generated/api.gen.go` (`api/oapi-codegen.yaml:8`).

## CI conformance receipt

- `foundation-contracts` run
  [39763798043](https://github.com/luogangan7-lgtm/opensite-commerce/actions/runs/39763798043)
  (`pull_request`, head `65c35362`, 2026-10-22T10:31:56Z).
- Job `generated-patch` completed **success**: the pinned generator re-rendered
  the imported bytes and found no diff over the generated paths. No
  `generated-patch` artifact exists for this run, which is the success condition
  for that job. The run's only failure was `foundation`, on the privacy tests
  that this commit was introducing
  (`PRIVACY_CLASSIFICATION_MISSING: orders.provider_ref`;
  personal != financial_obligation), not on generated drift.

## Final integrated mapping

- PR #44 head `7779e8b7bb6406deb337f3a7e8bff48ed01d3ed3` (merge parents
  `0ce8d6b4` and `93006152`).
- [foundation 39806280270](https://github.com/luogangan7-lgtm/opensite-commerce/actions/runs/39806280270)
  and [MVP 39806280195](https://github.com/luogangan7-lgtm/opensite-commerce/actions/runs/39806280195)
  both succeeded on that exact head.
- Squash-merged as `be0a6413af77b6a1c6369a74c601b7f6b9ae78ed` (single parent
  `93006152`). Both commits have tree
  `8f56e89a98aa6a30ee3f8f555ef31706cdc2eef3`, so the merged tree is identical to
  the tested head.
- The earlier `267ff2d7` receipts (`39752079248`, `39752079257`) remain
  historical evidence for that earlier source; they are not final-head
  acceptance and are preserved, not rewritten.

## PR #44 body

The PR #44 body was refreshed on 2026-10-26 to state this final mapping and to
keep the `267ff2d7` receipts explicitly historical.
