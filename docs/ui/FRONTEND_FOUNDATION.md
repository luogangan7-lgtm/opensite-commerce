# Frontend foundation (C-08a)

The admin skeleton uses Vite, React, React Router and TanStack Query. Bun 1.3.12 owns one workspace lockfile. Storefront and API-client directories reserve the A5 boundaries; they contain no Astro pages or generated client.

The dependency direction is `design-tokens → ui → composition → page templates → pages`. `component-contracts` owns typed English copy. Components do not own network requests or business state. The catalog composes the shared components at `/_gallery`, accessible in development and the explicit `gallery` test build. The ordinary production build removes the gallery import and route.

`packages/design-tokens/tokens.json` is the sole token source. Its generator produces CSS custom properties and TypeScript constants. `tokens:check` rejects drift, the contrast gate validates registered foreground/background pairs, and the source style gate rejects literal colors, lengths and physical spacing in UI/admin. Generated build outputs and dependencies are excluded. CSS uses logical properties. All UI text uses typed dictionary keys; an unknown key is a TypeScript error.

The catalog contains 70 real controlled state cases, including the native modal dialog. Its table rows describe the controls shown in the catalog. No merchant, customer or transaction fixtures are invented. Browser gates assert state coverage, actual input state, keyboard focus, modal semantics, focus wrapping/restoration, production exclusion and axe serious/critical violations. Visual snapshots include each case plus hover/focus states; focused cases include the outline outside the control border box.

## Run the gates

```sh
bun install --frozen-lockfile
bash scripts/check_frontend.sh
bun scripts/frontend/check-baselines.ts
bun run visual:test
```

Use the pinned official Playwright image from `playwright.config.ts` and `checks.yml` for visual comparisons. The test package and browser image both use Playwright 1.64.0. The image fixes Linux amd64 and browser dependencies; bundled Inter 5.3.0 fixes font files. Tests require loaded Inter 400/500/600 faces, en-US, UTC, light colors, reduced motion, scale 1, desktop 1280×800 and mobile 390×844. Native controls inherit this browser environment.

## CI alone produces golden images

1. Change the source, commit with `[visual-baseline-update]` in the commit message, and push the PR branch. This triggers the explicit `frontend-baselines` producer job in the same pinned container as comparisons. The ordinary `frontend` job keeps comparing the currently committed golds, so a source change remains red until the CI output is committed.
2. Download `frontend-baselines-<run-id>` from the successful producer job. Verify its run/head SHA and image, then copy its `desktop/`, `mobile/` and `provenance.json` into `tests/frontend/baselines`. Inspect actual PNGs before committing them.
3. Commit the CI artifact, push, and require the ordinary `frontend` and existing `foundation` jobs to pass. The update script rejects local invocation. Local failure screenshots are diagnostic evidence only and must never be copied into golden paths.

The provenance file records the producing run, source SHA, rendering inputs, source fingerprint and PNG hashes. The local consistency check does not establish CI authority. CI additionally downloads the actual producer artifact using a read-only Actions token, verifies GitHub's ZIP digest, and compares the committed metadata and every PNG byte against that artifact. Baseline artifacts retain 90 days; renew golds through the explicit CI producer before their artifact expires. Missing/expired artifacts fail closed.

This explicit push marker works while the new workflow is still in its first PR; `workflow_dispatch` would require the workflow to exist on the default branch. It gives no repository write or approval permission to the producer. Independent PR review remains a separate task.

## Boundaries and receipts

No migrations, Go code, HTTP client generation, business pages, login, Astro, tax, shipping or promotions are included. Existing foundation CI steps are preserved; the frontend jobs are appended. Required-context configuration belongs to the integrator.

The PR body records the final SHA gates and hardcoded-color, label-association and token-difference red → green probes. CI artifacts are the golden image evidence; local captures are not accepted as baselines. Official environment documentation: [Playwright Docker](https://playwright.dev/docs/docker), [Bun installation](https://bun.sh/docs/installation).
