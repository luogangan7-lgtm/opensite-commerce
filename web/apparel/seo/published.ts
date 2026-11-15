/** Published SEO inventory. Depends on catalog and pinned identities; consumed by head, endpoints and dist gate.
 * Invariant: only published, corroborated products enter the sitemap. Domain remains a non-production placeholder.
 */
import { products, categories } from '../src/data/catalog';
import { factPaths } from './contract';
export { buildOrigin as seoOrigin } from './mode';
export const publishedPaths = ['/', '/about-us/', '/collections/', '/contact/', '/oem-odm/', '/sampling/', '/shipping/', '/privacy/', '/terms/', ...Object.keys(categories).map(c => `/collections/${c}/`), ...products.map(p => `/products/${p.slug}/`).filter(p => factPaths.includes(p))];
