/** Published SEO inventory. Depends on catalog and pinned identities; consumed by head, endpoints and dist gate.
 * Invariant: only published, corroborated products enter the sitemap. Domain remains a non-production placeholder.
 */
import { products } from '../src/data/catalog';
import { factPaths } from './contract';
export const seoOrigin = 'https://sourcyowear.example.invalid';
export const publishedPaths = ['/', '/collections/', '/contact/', ...products.map(p => `/products/${p.slug}/`).filter(p => factPaths.includes(p))];
