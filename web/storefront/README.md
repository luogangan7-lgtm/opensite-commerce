# Published storefront

Astro 7.3.8 runs SSR on Node using the official standalone adapter. Bun 1.3.12 builds and checks it; React islands reuse `@opensite/ui`, the shared tokens and the typed storefront dictionary. No admin code or PostgreSQL credentials belong in this process. `bun run start:storefront` strips database identity variables before Node starts.

The renderer asks the internal Go `getPublishedSite` API at `OSC_READ_API_ORIGIN` for the request hostname. The API uses a dedicated login inheriting only `app_domain_resolver`, a read-only repeatable-read transaction, and host-scoped FORCE RLS policies. It returns only the current sealed release and page revisions pinned by its routes. Unknown/unverified/unpublished hosts return 404; upstream failure renders 502 with a retry action. Responses are request scoped and `no-store`.

Routes: `/`, nested CMS paths, `/products`, `/products/[slug]`, and accessible 404/502 pages. CMS paths under `/products`, `/404` and `/502` are reserved. Titles, descriptions and canonical URLs are server rendered; the currency amount remains an integer string through the transport and uses BigInt for formatting.

The version-1 release manifest contains public `locale`, `title`, `description`, `navigation` and `products`. Page documents contain only `title`, `description`, and heading/paragraph text blocks; HTML is escaped. Product models are immutable release snapshots while catalog PR #32 awaits integration. There is no live catalog, Builder, write-publish/restore or checkout implementation here.

For a disposable PostgreSQL 18 database, apply migrations and run `OSC_STOREFRONT_SAMPLE_SEED=1 bash scripts/seed_storefront.sh` with `DATABASE_URL` provided through the environment. The atomic, repeatable sample contains published pages/products, two tenants and unpublished/draft witnesses. `scripts/start_storefront_api_e2e.sh` provisions a restricted ephemeral login and owns the test API process; Playwright starts it before SSR.

Run `bash scripts/check_frontend.sh` and, with the disposable database configured, `bun run visual:test`. Local screenshots are diagnostic only. Push `[visual-baseline-update]`; download the successful `frontend-baselines` CI artifact, verify its producing SHA/container and digest, then commit its PNGs and provenance unchanged. The normal frontend gate verifies those bytes against GitHub before comparing both viewports and running axe.
