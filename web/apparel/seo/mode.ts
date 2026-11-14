/** Build policy shared by Astro, SEO inventory and dist gates; defaults to protected preview.
 * Production requires an explicit owner-supplied HTTPS origin; no inferred domain.
 */
export const seoBuildMode = process.env.SEO_BUILD_MODE || 'preview';
if (!['preview', 'production'].includes(seoBuildMode)) throw Error('Invalid SEO_BUILD_MODE');
export const isProduction = seoBuildMode === 'production';
export const placeholderOrigin = 'https://sourcyowear.example.invalid';
export function productionOrigin(value: string | undefined): string {
  if (!value) throw Error('PRODUCTION_ORIGIN required');
  const url = new URL(value);
  if (url.protocol !== 'https:' || url.username || url.password || url.pathname !== '/' || url.search || url.hash || /invalid|localhost|pages\.dev|preview/i.test(url.hostname)) throw Error('Unsafe PRODUCTION_ORIGIN');
  return url.origin;
}
export const buildOrigin = isProduction ? productionOrigin(process.env.PRODUCTION_ORIGIN) : placeholderOrigin;
