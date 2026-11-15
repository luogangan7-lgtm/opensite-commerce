/** Public previews allow retrieval of the noindex HTML and HTTP policy. */
import { seoOrigin } from '../../seo/published';
export function GET() { return new Response(`User-agent: *\nAllow: /\nSitemap: ${seoOrigin}/sitemap.xml\n`, { headers: { 'Content-Type': 'text/plain' } }); }
