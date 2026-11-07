/** Static crawler policy: pre-release remains blocked until production authorization. */
import { seoOrigin } from '../../seo/published';
import { isProduction } from '../../seo/mode';
export function GET() { return new Response(`User-agent: *\n${isProduction ? 'Allow: /' : 'Disallow: /'}\nSitemap: ${seoOrigin}/sitemap.xml\n`, { headers: { 'Content-Type': 'text/plain' } }); }
