/** Responsibility: pin the disposable CI environment and audit real rendered pages.
 * Dependencies: Playwright, axe and main published-read DTO; callers: E2E config/specs.
 * Invariants: CI opt-in, explicit loopback origins, seeded host identity, no route interception.
 */
import AxeBuilder from '@axe-core/playwright';
import { expect, type APIRequestContext, type Page, type TestInfo } from '@playwright/test';
import type { PublishedSite } from '../../packages/api-client/src';

if (process.env.GITHUB_ACTIONS !== 'true' || process.env.OPENSITE_MVP_E2E !== '1') {
  throw new Error('MVP E2E runs only in the explicit GitHub Actions disposable-stack job');
}

function origin(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Required CI origin is missing: ${name}`);
  const url = new URL(value);
  if (url.protocol !== 'http:' || !['127.0.0.1', 'localhost'].includes(url.hostname) ||
      !url.port || url.username || url.password || url.pathname !== '/' || url.search || url.hash) {
    throw new Error(`${name} must be a loopback HTTP origin with an explicit port`);
  }
  return url.origin;
}
function seeded(name: string, value: string): string {
  if (process.env[name] !== value) throw new Error(`${name} must identify the owned disposable CI seed`);
  return value;
}

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Required CI value is missing: ${name}`);
  return value;
}

export const fixture = {
  // First owner created by the real bootstrap CLI in run.py; a separate tenant from the storefront seed.
  ownerEmail: required('E2E_OWNER_EMAIL'),
  ownerPassword: required('E2E_OWNER_PASSWORD'),
  ownerTenantId: required('E2E_OWNER_TENANT_ID'),
  api: origin('E2E_API_URL'),
  storefront: origin('E2E_STOREFRONT_URL'),
  admin: origin('E2E_ADMIN_URL'),
  tenantId: seeded('E2E_TENANT_ID', 'c0900001-0000-4000-8000-000000000001'),
  productSlug: seeded('E2E_PRODUCT_SLUG', 'canvas-tote'),
  cmsSlug: seeded('E2E_CMS_SLUG', 'about'),
  siteId: 'c0900003-0000-4000-8000-000000000001',
  releaseId: 'c0900005-0000-4000-8000-000000000001',
  productId: 'c0900007-0000-4000-8000-000000000001',
  title: 'OpenSite sample shop',
  productName: 'Canvas tote',
  secrets: ['OTHER_TENANT_SECRET', 'DRAFT_REVISION_SECRET', 'DRAFT_CATALOG_SECRET',
    'CANDIDATE_RELEASE_SECRET', 'OPEN_DRAFT_SECRET', 'UNPUBLISHED_CATALOG_SECRET'],
} as const;

export function assertPublicOnly(text: string): void {
  for (const secret of fixture.secrets) expect(text).not.toContain(secret);
}

export async function publishedSite(request: APIRequestContext): Promise<PublishedSite> {
  const host = new URL(fixture.storefront).hostname;
  const response = await request.get(`${fixture.api}/api/v1/public/sites/current`, {
    params: { host }, maxRedirects: 0,
  });
  expect(response.status()).toBe(200);
  expect(response.headers()['content-type']).toContain('application/json');
  expect(response.headers()['cache-control']).toContain('no-store');
  const text = await response.text();
  assertPublicOnly(text);
  const site = JSON.parse(text) as PublishedSite;
  // The serving body is host-independent (ADR_STOREFRONT_SERVING §7.3) and carries the host's tenant.
  expect(site).toMatchObject({ tenant_id: fixture.tenantId, site_id: fixture.siteId, release_id: fixture.releaseId,
    title: fixture.title, locale: 'en' });
  expect(site).not.toHaveProperty('hostname');
  expect(site.products.find(product => product.slug === fixture.productSlug)).toMatchObject({
    id: fixture.productId, name: fixture.productName, skus: [],
  });
  return site;
}

export async function audit(page: Page, info: TestInfo): Promise<void> {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
    'The real page must fit the selected viewport').toBe(true);
  const report = await new AxeBuilder({ page }).analyze();
  await info.attach(`axe-${info.attachments.length}`, {
    body: JSON.stringify({ url: page.url(), violations: report.violations }, null, 2),
    contentType: 'application/json',
  });
  expect(report.violations.filter(violation => ['serious', 'critical'].includes(violation.impact ?? '')),
    'Real page has serious/critical accessibility violations; fix the UI rather than excluding rules').toEqual([]);
}
