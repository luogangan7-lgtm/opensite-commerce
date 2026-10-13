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

1. Only when the PR's visual comparison is red for an intended change: commit with `[visual-baseline-update]` in the commit message and push the PR branch. This triggers the explicit `frontend-baselines` producer job in the same pinned container as comparisons; it re-renders only scopes whose comparison fails. The ordinary `frontend` job keeps comparing the currently committed golds, so the change remains red until the CI output is committed.
2. Download `frontend-baselines-<run-id>` from the successful producer job. Verify its run/head SHA and artifact ZIP digest, then copy its `desktop/` and `mobile/` into `tests/frontend/baselines`, preserving the CI-produced `*.png.provenance.json` siblings. The initial schema-2 migration removes the legacy root `provenance.json`. Inspect actual PNGs before committing them.
3. Commit the CI artifact, push, and require the ordinary `frontend` and existing `foundation` jobs to pass. The update script rejects local invocation. Local failure screenshots are diagnostic evidence only and must never be copied into golden paths.

Two questions are kept apart (C-08d):

- **Is the golden trustworthy? (provenance, committed)** Every PNG has a separate schema-2 record: snapshot identity, renderer scope, producing run/head SHA, pinned rendering inputs, the producer-time input fingerprint (evidence only), PNG hash and the CI-observed same-scope snapshot inventory. `check-baselines` requires one record per PNG, pinned renderer values, matching PNG hashes and the complete producer-observed inventory in both viewports; deleting a PNG and its record together cannot shrink it. It does **not** compare the recorded fingerprint with current inputs. `verify-baseline-artifact` checks every golden whose PNG or record bytes differ from `HEAD^1` (the PR's base in the `pull_request` merge commit, the previous main on push) against its producer: run/head SHA/repository/workflow/successful job, GitHub's ZIP digest, committed PNG **and raw record bytes** versus the artifact, the producer workflow allowlist (`workflow-provenance.ts`: no repository code, env, inputs or services before the writer, nothing after it except the pinned upload) and the writer inputs at that SHA (`update-baselines.ts` and its local imports, `package.json`, `bun.lock`, `tsconfig.json`, `bunfig.toml`) byte-equal to the candidate tree. Forged/edited PNGs or records, forged runs, a modified pre-writer workflow and expired artifacts of *changed* goldens fail closed.
- **Does the candidate still match? (comparison, not committed)** Every PR runs Playwright against the committed goldens with zero tolerance. The comparison receipt (candidate/head SHA, input fingerprints, golden hashes, result) is the `frontend-comparison-<run-id>` artifact and job summary; it never enters the repository. A change with identical pixels therefore needs no new PNG; a visual change goes red until the producer flow supplies new goldens.

Artifact expiry: GitHub keeps producer artifacts 90 days. Unchanged goldens are not re-downloaded: their committed hashes were verified against the live artifact when they entered main (every main commit lands through that check), so later PRs inherit that verification and an expired artifact cannot block unrelated PRs. This assumes main only changes through PRs with this check, as the repository process requires. Only goldens a PR changes need a live artifact; an expired one is regenerated by the producer. No renew job or attestation is required.

The producer re-renders a scope (`gallery`, `admin`, `storefront-published`, `storefront-commerce`) only when its comparison fails, its PNGs/records are missing or invalid, or `OPENSITE_BASELINE_SCOPES` requests it; every other PNG/record byte is preserved. Because nothing runs before the writer, the writer builds the storefront itself and `bun install` runs with `--ignore-scripts`. Renderer code executed by the writer could only forge pixels the candidate comparison would then reject. Scope fingerprints (reported in records and receipts, not gated) trace TypeScript imports/reexports/literal dynamic imports, Astro scripts/frontmatter, CSS imports/assets, workspace exports and files imported by Vite/Astro configs; storefront scopes include the Go read API, `cmd/river-migrate` and SQL migrations their fixture runs.

Known nondeterminism: `storefront-commerce-cart.png` has differed between producer runs by ±1 in single colour channels at a few anti-aliased rounded-corner pixels of the cart line and summary cards (21–31 pixels, no layout change). The pattern (corner anti-aliasing only, no glyph, layout or text change) points to Chromium rasterisation rather than fonts, animation or time; fixing it needs browser raster flags that would re-render every golden, so it is left as is. With comparison-based selection the producer no longer rewrites it unless the cart comparison actually fails.

The graph uses the legacy compiler API from the exact `typescript-parser` alias (`npm:typescript@6.0.3`, the same version/integrity already used by storefront). Native TypeScript 7.0.2 is still the root typechecker, invoked by its explicit package binary so parser aliases cannot change CLI selection. The frozen lock binds both tools.

The `gallery` and `admin` scopes share the same admin app entry point (`web/admin/index.html`), not merely shared UI files, so a change reachable from that entry invalidates both; this change does not claim independent golds between them. Independent admin versus storefront leaf inputs can merge without rewriting one another's records. A new renderer/snapshot family must extend the root/identity registry and its tests.

Force individual scopes through the explicit CI producer with a job-level `OPENSITE_BASELINE_SCOPES` (comma-separated scope names, the only optional producer env key); scopes whose comparison fails cannot be excluded. A post-main bot PR is an optional future renewal mechanism, not a default acceptance path: silently replacing visuals on main could conceal a real shared-input change. Such a bot would still need pinned production, artifact verification, the ordinary visual gate and independent PR review; it must never auto-merge changed golds.

This explicit push marker works while the new workflow is still in its first PR; `workflow_dispatch` would require the workflow to exist on the default branch. It gives no repository write or approval permission to the producer. Independent PR review remains a separate task.

## Boundaries and receipts

No migrations, Go code, HTTP client generation, business pages, login, Astro, tax, shipping or promotions are included. Existing foundation CI steps are preserved; the frontend jobs are appended. Required-context configuration belongs to the integrator.

The PR body records the final SHA gates and hardcoded-color, label-association and token-difference red → green probes. CI artifacts are the golden image evidence; local captures are not accepted as baselines. Official environment documentation: [Playwright Docker](https://playwright.dev/docs/docker), [Bun installation](https://bun.sh/docs/installation).
