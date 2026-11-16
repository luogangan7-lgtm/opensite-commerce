/** Listing adapter: catalog facts only; consumed by static cards, URL controls and tests.
 * Unknown composition/fit never matches a facet. Featured preserves catalog order.
 */
import type { Product } from './catalog';
export const shopFacets = ['category', 'colour', 'fit', 'fabric', 'fleece'] as const;
export type ShopFacet = typeof shopFacets[number];
export const pageSize = 24;
export function shopFacts(p: Product): Record<ShopFacet, string[]> {
  const text = p.fabric_weight_fit.toLowerCase();
  return {
    category: [p.category],
    colour: [...new Set(p.images.filter(i => i.role === 'front' && i.colour).map(i => i.colour!))],
    fit: ['oversized', 'relaxed', 'regular', 'slim'].filter(v => new RegExp(`\\b${v}\\b`).test(text)),
    fabric: ['cotton', 'polyester', 'terry'].filter(v => new RegExp(`\\b${v}\\b`).test(text)),
    fleece: /fleece[- ]lined|silver-fox fleece/.test(text) ? ['yes'] : /\bunlined\b/.test(text) ? ['no'] : [],
  };
}
export function selectStyles(items: Product[], query: URLSearchParams): Product[] {
  const result = items.filter(p => {
    const facts = shopFacts(p);
    return shopFacets.every(key => !query.getAll(key).length || query.getAll(key).some(v => facts[key].includes(v)));
  });
  return query.get('sort') === 'code' ? result.sort((a,b) => a.style_codes.join('/').localeCompare(b.style_codes.join('/'), 'en', { numeric: true })) : result;
}
export function setShopQuery(query: URLSearchParams, key: ShopFacet | 'sort', value: string): URLSearchParams {
  const next = new URLSearchParams(query); next.delete(key);
  if (value && !(key === 'sort' && value === 'featured')) next.set(key, value);
  return next;
}
export function cardViews(p: Product) {
  const front = p.images.find(i => i.role === 'front') || p.images[0];
  const back = p.images.find(i => i.role === 'back' && i.colour === front.colour);
  return { front, back };
}
