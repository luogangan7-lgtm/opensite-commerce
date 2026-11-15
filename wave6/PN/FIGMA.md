# PN Figma evidence — 2026-11-15

Authority: owner-approved PN.card.md, five EN replacements; file `iV07xrwn5UfjWpIQ3LWPoE`.
Official session `1adaabe7-df9f-4ac7-953a-08ded5f4ff2a` confirmed file identity, pages and 532 Locale variables. Local bridge connection `9e459dc8-fcc2-4f87-bb13-9cf9982438e5` confirmed formal document name; used for complete read-only scanning, existing gates and exports. No design mutation was needed or performed.

## Before / after

Exact-match inventory: five canonical EN values plus corresponding product dictionary translations = 33 distinct old strings. Warm scans until node counts stabilize across all 20 pages (including backups), 95002 text nodes, and every variable collection found **zero matching text nodes and zero matching variable values**. Before: zero; after: zero; mutated node IDs: `[]`. There is therefore no before/after replacement row to report. K02's design does not contain these product-specific sourcing fields. No new design text or variable was invented. Existing styles, variable bindings and layout remain untouched.

Raw evidence: `figma-scan.args.json` (exact match set and scan), `figma-scan.json` (per-page coverage). Official inspection also found no legacy EN values among Locale variables.

## Existing gates

- `structure.json`: GREEN, 253 roots / 41,084 nodes, a/b/c/d=0, unexplained overrides=0, touch=0, completeness=true. Uses the existing FD3 async-main-component adapter of v12 structure-check without predicate/threshold changes; exact source retained in `structure.args.json`.
- `zh-locale.json`: all GREEN, 532/532 nonempty Locale values, 1,599 CJK nodes, 635/635 font-family bindings; exact source in `zh-locale.args.json` from current `figma-evidence/v12/zh-locale-check.js`.

## Screenshot node IDs

| Locale | Desktop | Tablet | Mobile |
|---|---|---|---|
| EN | 69:1806 | 493:10239 | 69:1889 |
| AR | 79:2167 | 153:5409 | 79:2247 |
| ZH | 198:6516 | 198:6599 | 198:6684 |

Nine actual PNG exports at scale 0.5 are in `screenshots/`; text receipts map node IDs to paths. EN desktop, AR mobile and ZH mobile were visually inspected. Other locale values were included in exact-match scanning; no extra locale frames or temporary modes were created for this copy-only task.

## Code evidence

Baseline: `d191de31e536c8b7b3a868e99de10180e3c42ad0` (latest remote feat/apparel-b2b-v1 at fetch).
`translations.tsv` is the replacement source for all nine JSON data files. Counts per file: overview 106, fleece 21, combed cotton 17, supplier-attributed 430 gsm 16, indicative colours 9 (169 values; 155 products). Only `colourway_note` and `fabric_weight_fit` changed; structural comparison verified unchanged keys, array lengths and other values.
`unit-red.txt`: new unit gate run against base data, 2 fail / 7 pass (canonical + EN expose legacy copy).
`unit-green.txt`: same gate plus i18n contract after replacement, 17 pass / 0 fail.
Official apparel workflow already discovers `web/apparel/tests/*.test.ts`; no workflow changes.
Root DeepSeek + Hy4 translation cross-check and independent review remain required before merge. No deploy.
