/** Listing adapter: catalog facts only; consumed by static cards, URL controls and tests.
 * Unknown composition/fit never matches a facet. Featured uses the approved prefix and preserves the remaining catalog order.
 */
import type { Product } from './catalog';
export const shopFacets = ['category', 'colour', 'fit', 'fabric', 'fleece'] as const;
export type ShopFacet = typeof shopFacets[number];
export const pageSize = 24;
export type Family = 'black'|'white'|'cream'|'grey'|'charcoal'|'navy'|'blue'|'green'|'brown'|'beige'|'red'|'pink'|'purple';
// Every raw front-colour annotation in the catalog collapses to one customer-facing family.
export const colourFamily: Record<string,Family> = {
 'black':'black','黑':'black','black (overview only)':'black',
 'white':'white','white/cream':'white','white/cream (overview only, no model photo)':'white','off-white/light grey (unclear vs 18)':'white','off-white/very light grey':'white',
 'cream':'cream','cream (overview only)':'cream',
 'grey':'grey','灰':'grey','light grey':'grey','dark grey':'grey','slate grey':'grey','heather grey':'grey','mid grey':'grey','pale heather grey (img 7)':'grey','pale heather grey/off-white (img 9-10)':'grey',
 'charcoal':'charcoal',
 'navy':'navy','藏青':'navy','navy (overview only)':'navy',
 'blue':'blue','blue (denim)':'blue','blue (dusty/denim)':'blue',
 'green':'green','dark green':'green','dark green (overview only)':'green','sage/olive grey':'green','sage green':'green','khaki/olive':'green','light grey-green/sage':'green',
 'brown':'brown','rust brown':'brown','camel brown':'brown','camel brown (img 5)':'brown',
 'beige':'beige','beige/sand':'beige','stone/light greige':'beige','stone/greige':'beige','khaki/sand':'beige','tan/beige':'beige','camel/beige':'beige','sand/beige':'beige','tan/beige (img 6; may equal img 4 - unclear)':'beige','tan/beige (img 6)':'beige','taupe/greige':'beige','taupe/greige (img 4)':'beige',
 'red':'red','burgundy':'red',
 'pink':'pink','pink (overview only)':'pink',
 'purple':'purple','purple/plum':'purple','slate purple-grey':'purple',
};
// Inferred flat swatch hexes; one common shade per family, no invented shade names.
export const colourHex: Record<Family,string> = {
 black:'#1a1a1a',white:'#ffffff',cream:'#e6dfcc',grey:'#9a9a94',charcoal:'#444444',navy:'#26374a',blue:'#476b8a',green:'#315840',brown:'#6b4026',beige:'#c8b18c',red:'#a52a2a',pink:'#d9a5ad',purple:'#756477',
};
// Approved family order by catalog product count, descending (ties alphabetical).
export const colourFamilies:Family[]=['black','white','brown','green','grey','beige','navy','cream','charcoal','red','purple','pink','blue'];
// Owner-approved D-SHOP B display order; remaining catalog entries retain their order.
export const shopNameKeys: Record<string,string> = { '59043':'cp-shop/figma-0001', '58065':'cp-shop/figma-0008', '8900-31':'cp-shop/figma-0010', '69018':'cp-shop/figma-0013', 'TZ2804-7':'cp-shop/figma-0015', '0253':'cp-shop/figma-0017', '59011':'cp-shop/figma-0019', '8900-32':'cp-shop/figma-0010' };
const featuredCodes = ['59043','58065','8900-31','69018','TZ2804-7','0253','59011','8900-32'];
export function featuredStyles(items: Product[]): Product[] {
  const rank = (p: Product) => { const i=featuredCodes.indexOf(p.style_codes[0]); return i < 0 ? featuredCodes.length : i; };
  return [...items].sort((a,b) => rank(a)-rank(b));
}
export function shopFacts(p: Product): Record<ShopFacet, string[]> {
  const text = p.fabric_weight_fit.toLowerCase();
  return {
    category: [p.category],
    colour: [...new Set(p.images.filter(i => i.role === 'front' && i.colour).map(i => colourFamily[i.colour!]))],
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
export function setShopQuery(query: URLSearchParams, key: ShopFacet | 'sort', value: string | string[]): URLSearchParams {
  const next = new URLSearchParams(query); next.delete(key);
  for (const item of Array.isArray(value) ? value : [value]) if (item && !(key === 'sort' && item === 'featured')) next.append(key, item);
  return next;
}
export function cardViews(p: Product) {
  const front = p.images.find(i => i.role === 'front') || p.images[0];
  const back = p.images.find(i => i.role === 'back' && i.colour === front.colour);
  return { front, back };
}
