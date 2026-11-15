/** Static XML endpoint; uses the published SEO allow-list, never preview or editorial routes. */
import { sitemap } from '../../seo/contract';
import { seoOrigin, publishedPaths } from '../../seo/published';
export function GET() {
  const urls = sitemap(seoOrigin, publishedPaths);
  return new Response(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${urls.map(url => `<url><loc>${url}</loc></url>`).join('')}</urlset>`, { headers: { 'Content-Type': 'application/xml' } });
}
