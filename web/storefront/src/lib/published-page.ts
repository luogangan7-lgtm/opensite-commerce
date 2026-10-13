/** Responsibility: load a published view and prepare the top-level SSR response before rendering.
 * Dependencies: public API client, presentation validation, UI copy; callers: published Astro routes.
 * Invariants: one request-scoped read, URL hostname/canonical only, no cross-host cache; children only render.
 */
import type { AstroGlobal } from 'astro';
import { readPublishedSite, PublicSiteNotFoundError, type PublishedSite } from '@opensite/api-client';
import { copy } from './copy';
import { formatAmount, normalizeHostname, readAPIOrigin } from './presentation';
import { requestCanonical } from './host-policy';

export async function preparePublishedPage(
  context: Pick<AstroGlobal, 'url' | 'request' | 'response'>,
  path: string,
  apiOrigin = process.env.OSC_READ_API_ORIGIN ?? 'http://127.0.0.1:8080',
) {
  let site: PublishedSite | undefined;
  let status: 404 | 502 | undefined;
  const canonical = requestCanonical(context);
  try {
    if (!canonical) throw new PublicSiteNotFoundError();
    const read = await readPublishedSite(readAPIOrigin(apiOrigin), normalizeHostname(context.url.hostname));
    // Intl rejects some well-formed-looking locales; fail as 502 here, not mid-stream in render.
    Intl.getCanonicalLocales(read.locale);
    for (const item of read.products) formatAmount(item.unit_amount, item.currency, read.locale);
    site = read;
  } catch (error) {
    status = error instanceof PublicSiteNotFoundError ? 404 : 502;
  }
  const ui = copy(site?.locale);
  const isList = path === '/products';
  const productSlug = path.startsWith('/products/') ? path.slice('/products/'.length) : undefined;
  const product = productSlug ? site?.products.find(item => item.slug === productSlug) : undefined;
  const page = site?.pages.find(item => item.path === path);
  if (!status && !isList && !product && !page) status = 404;
  const title = product?.name ?? (isList ? ui.products : page?.title) ?? site?.title ?? ui.storefront;
  const description = product?.description ?? (isList ? site?.description : page?.description) ?? '';
  context.response.status = status ?? 200;
  context.response.headers.set('Cache-Control', 'no-store');
  return { site, status, ui, isList, product, page, title, description, canonical };
}

export type PublishedPageView = Awaited<ReturnType<typeof preparePublishedPage>>;
