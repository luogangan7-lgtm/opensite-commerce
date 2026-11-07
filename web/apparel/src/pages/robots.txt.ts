/** Static crawler policy: pre-release remains blocked until production authorization. */
import { seoOrigin } from '../../seo/published';
export function GET() { return new Response(`User-agent: *\nDisallow: /\nSitemap: ${seoOrigin}/sitemap.xml\n`, { headers: { 'Content-Type': 'text/plain' } }); }
