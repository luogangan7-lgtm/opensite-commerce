# Frontend foundation (C-08a)

The admin skeleton uses Vite, React, React Router and TanStack Query. Bun 1.3.12 owns one workspace lockfile. Storefront and API-client directories reserve the A5 boundaries; they contain no Astro pages or generated client.

The dependency direction is `design-tokens → ui → composition → page templates → pages`. `component-contracts` owns typed English copy. Components do not own network requests or business state. The catalog composes the shared components at `/_gallery`, accessible in development and the explicit `gallery` test build. The ordinary production build removes the gallery import and route.

`packages/design-tokens/tokens.json` is the sole token source. Its generator produces CSS custom properties and TypeScript constants. `tokens:check` rejects drift, the contrast gate validates registered foreground/background pairs, and the source style gate rejects literal colors, lengths and physical spacing in UI/admin. Generated build outputs and dependencies are excluded. CSS uses logical properties. All UI text uses typed dictionary keys; an unknown key is a TypeScript error.

The catalog contains 70 real controlled state cases, including the native modal dialog. Its table rows describe the controls shown in the catalog. No merchant, customer or transaction fixtures are invented. Browser gates assert state coverage, actual input state, keyboard focus, modal semantics, focus wrapping/restoration, production exclusion and axe serious/critical violations. Visual snapshots include each case plus hover/focus states; focused cases include the outline outside the control border box.

## CI gates

All tests, checks, builds and container execution run only on GitHub CI (owner rule, 2026-10-13). These commands describe the CI gate, not local instructions:

```sh
bun install --frozen-lockfile
bash scripts/check_frontend.sh
bun scripts/frontend/check-baselines.ts
bun run visual:test
```

Use the pinned official Playwright image from `playwright.config.ts` and `checks.yml` for visual comparisons. The test package and browser image both use Playwright 1.64.0. The image fixes Linux amd64 and browser dependencies; bundled Inter 5.3.0 fixes font files. Tests require loaded Inter 400/500/600 faces, en-US, UTC, light colors, reduced motion, scale 1, desktop 1280×800 and mobile 390×844. Native controls inherit this browser environment.

## CI alone produces golden images

1. Change the source, commit with `[visual-baseline-update]` in the commit message, and push the PR branch. This triggers the explicit `frontend-baselines` producer job in the same pinned container as comparisons. The ordinary `frontend` job keeps comparing the currently committed golds, so a source change remains red until the CI output is committed.
2. Download `frontend-baselines-<run-id>` from the successful producer job. Verify its run/head SHA and artifact ZIP digest, then copy its `desktop/` and `mobile/` into `tests/frontend/baselines`, preserving the CI-produced `*.png.provenance.json` siblings. The initial schema-2 migration removes the legacy root `provenance.json`. Inspect actual PNGs before committing them.
3. Commit the CI artifact, push, and require the ordinary `frontend` and existing `foundation` jobs to pass. The update script rejects local invocation. Local failure screenshots are diagnostic evidence only and must never be copied into golden paths.

Every PNG has a separate schema-2 provenance record: snapshot identity, renderer scope, producing run/head SHA, pinned rendering inputs, dependency fingerprint and PNG hash. There is no mutable global index. `check-baselines` requires exactly one record per PNG and recomputes the scope's current inputs. `verify-baseline-artifact` groups records by their original run, verifies run/head SHA/repository/workflow/successful producer, checks GitHub's ZIP digest, and compares each committed PNG **and raw record bytes** to that run's artifact. Different runs may legitimately coexist. Duplicate/unsafe ZIP paths, forged runs, missing/expired artifacts and stale inputs fail closed.

The writer automatically regenerates only affected scopes (`gallery`, `admin`, `storefront-published`, `storefront-commerce`) and preserves every other PNG/record byte. Scope roots are renderer/spec entries; TypeScript imports/reexports/literal dynamic imports, Astro scripts/frontmatter, CSS imports/assets and workspace exports are traced transitively. Unknown dynamic/local dependencies fail closed. Frozen dependencies, tokens and rendering configuration remain shared inputs. Non-importable published-read fixtures/backend inputs are conservatively included only for storefront scopes.

The UI library and aggregate gallery currently share physical source/style files. Those dependencies really overlap, so they invalidate all consumers; this change does not claim independent golds for edits to that shared file. Independent admin versus storefront leaf inputs can merge without rewriting one another's records. A new renderer/snapshot family must extend the root/identity registry and its tests.

Artifacts retain 90 days. Renew individual scopes through the explicit CI producer with `OPENSITE_BASELINE_SCOPES` (comma-separated scope names); automatically stale scopes cannot be excluded. A post-main bot PR is an optional future renewal mechanism, not a default acceptance path: silently replacing visuals on main could conceal a real shared-input change. Such a bot would still need pinned production, artifact verification, the ordinary visual gate and independent PR review; it must never auto-merge changed golds.

This explicit push marker works while the new workflow is still in its first PR; `workflow_dispatch` would require the workflow to exist on the default branch. It gives no repository write or approval permission to the producer. Independent PR review remains a separate task.

## Boundaries and receipts

No migrations, Go code, HTTP client generation, business pages, login, Astro, tax, shipping or promotions are included. Existing foundation CI steps are preserved; the frontend jobs are appended. Required-context configuration belongs to the integrator.

The PR body records the final SHA gates and hardcoded-color, label-association and token-difference red → green probes. CI artifacts are the golden image evidence; local captures are not accepted as baselines. Official environment documentation: [Playwright Docker](https://playwright.dev/docs/docker), [Bun installation](https://bun.sh/docs/installation).
