# POLISH detail QA — 2026-11-15

Author verification; no deployment. Base `ad036298e5363348033e031ead48e27b6aa4fd1b`.

| Detail | Live before | Local production build after | Evidence |
|---|---|---|---|
| Wholesale portrait | centred crop cuts head | top crop retains full head plus headroom | home-D/M.png → home-D/M-after.png |
| CTA arrows | shared flex gap `normal` (0px) | shared token `--sy-space-xs` = 8px; existing hero/fabric gaps 16/12px | measurements.json; polish.spec.ts |
| Dark wholesale block | content begins at top padding | `justify-content:center`; content balanced in 400px card | home-D-after.png |
| Home secondary grid | heading x96, secondary x48 | both x96 desktop; x32 tablet; x24 mobile | live-before.json; measurements.json |
| Section spacing | desktop sections 96px; fabric split 0px outer | retained 96px desktop, 64px tablet/mobile; fabric uses inner copy padding | measurements.json |
| PDP main frame | 4:3; letterboxed portrait | 4:5 (0.8 ratio), natural portrait fills matching frame; contain retained for square details | pdp-D/M-after.png; polish.spec.ts |
| Product views | button sits against picture edge | separate flow item with 16px token gap | pdp-D-after.png; polish.spec.ts |
| Thumbnail rail | variable height thumbnails | one horizontal row, 64×80px thumbnails, horizontal overflow available; vertical content fully visible | polish.spec.ts; pdp-D-after.png |
| Left column | short 4:3 gallery leaves empty column | portrait now 4:5; desktop gallery sticky at 24px | measurements.json |
| MOQ | Confirmed in quote | MOQ 100 pcs · mix colours & sizes, 8 locales, summary + quick/full specs | copy.json; owner about-copy.md:22,58 |
| Collections | hero copy x48; centred image crop | hero copy x96 desktop; portrait anchored at top; common CTA gap applies | coll-D/M.png → coll-D/M-after.png |
| Header | full language name | globe + uppercase locale code, desktop and mobile menu | Shell.astro; after screenshots |

Figma FIRST: formal file `iV07xrwn5UfjWpIQ3LWPoE`, session `6e8d1ccb-05be-4f75-9288-76e4ea2bf8a2`. Backup page `1226:38511` preserves all 21 affected roots. Home EN/AR D/T/M share Locale bindings for all eight languages. Nine formal CP-PDP frames (EN/ZH/AR × 1440/768/390) have 4:5 images, single-row controls and MOQ in strip plus quick/full specs. `figma-readback.json` contains native dimensions and localized readback; other five locales are variable modes. MOQ strip variable `1163:3117`, spec variable `1226:45195`. Collection desktop Hero panel padding updated to96. Existing Figma CTA spacing8 matches shared CSS token. Figma geometry is author readback, not independent acceptance.

Evidence boundaries: required six names are LIVE before screenshots; `*-after.png` are local build viewport screenshots at1440/390, height1300. Home is scrolled to Ways to buy; PDP and Collections show opening. Orca full-page stitching repeated the viewport, so those captures were replaced by native viewport screenshots. Existing cookie UI remains visible. Local Home/PDP imagery was reconstructed through existing SHA-validating `packageDigital`/`packageDetails`; no assets changed. Direct HTTP image fetch returned403 and was not treated as image evidence. Local package sources and visual checks do not certify deployed bytes. Root translation cross-check is pending; the eight-locale authored copy is in `copy.json`.

Validation: regression RED captured original 4:3 mismatch and zero CTA gap; initial CTA test accidentally selected a label span, corrected to the explicit aria-hidden arrow. CSS zero position serializes as0px, so the assertion accepts both0px and0%. Existing gallery/structure/media expectations updated from4:3 to4:5. Targeted unit suite18PASS/1SKIP; Astro check0errors/0warnings. Final Playwright and official CI pointers are reported with the PR head (no production claim).
