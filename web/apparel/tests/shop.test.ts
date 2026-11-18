/** CP-SHOP correctness against catalog, explicit unknowns and existing back provenance. */
import {test,expect} from 'bun:test';
import {readFileSync} from 'node:fs';
import {ownerMoq,pdpMoq,shopCustomMoq,shopCardMoq} from '../src/data/owner-facts';
import {categoryCountForms,locales,dictionaries,type Key} from '../src/i18n';
import {products} from '../src/data/catalog';
import {shopFacts,selectStyles,setShopQuery,cardViews,pageSize,featuredStyles,formatCategoryCount} from '../src/data/shop';
const colourFamilies=['black','white','cream','grey','charcoal','navy','blue','green','brown','beige','red','pink','purple'] as const;

test('every raw front colour has an approved family',async()=>{
 const shop:any=await import('../src/data/shop');
 const raw=new Set(products.flatMap(p=>p.images.filter(i=>i.role==='front'&&i.colour).map(i=>i.colour!)));
 expect(raw.size).toBeGreaterThan(50);
 for(const value of raw) expect(colourFamilies, `unmapped raw colour: ${value}`).toContain(shop.colourFamily?.[value]);
});

test('colour families are ordered by catalog product count descending',async()=>{
 const shop:any=await import('../src/data/shop');
 const count=(f:string)=>products.filter(p=>shop.shopFacts(p).colour.includes(f)).length;
 const expected=[...new Set(Object.values(shop.colourFamily)) as string[]].sort((a,b)=>count(b)-count(a)||a.localeCompare(b));
 expect(shop.colourFamilies).toEqual(expected);
});
test('colour facet labels have no raw annotations or CJK outside zh',()=>{
 for(const locale of locales) for(const family of colourFamilies) {
  const key=`cp-shop/colour-${family}` as Key, label=dictionaries[locale][key];
  expect(label, `${locale}:${key}`).toBeTruthy();
  expect(label, `${locale}:${key}`).not.toContain('(');
  if(locale!=='zh') expect(label, `${locale}:${key}`).not.toMatch(/[\u3400-\u9fff]/);
 }
});
test('facets intersect explicit product data and preserve Featured order',()=>{
 for(const category of new Set(products.map(p=>p.category))) {
  expect(selectStyles(products,new URLSearchParams({category}))).toEqual(products.filter(p=>p.category===category));
 }
 expect(selectStyles(products,new URLSearchParams('colour=black'))).toEqual(products.filter(p=>shopFacts(p).colour.includes('black')));
 for(const key of ['fabric','fit','fleece'] as const) for(const value of new Set(products.flatMap(p=>shopFacts(p)[key]))) {
  expect(selectStyles(products,new URLSearchParams({[key]:value,colour:'black'}))).toEqual(products.filter(p=>shopFacts(p)[key].includes(value)&&shopFacts(p).colour.includes('black')));
 }
 expect(selectStyles(products,new URLSearchParams('fit=unknown'))).toEqual([]);
 expect(selectStyles(products,new URLSearchParams())).toEqual(products);
});
test('sort is numeric code order and does not mutate catalog',()=>{
 const before=products.map(p=>p.slug), sorted=selectStyles(products,new URLSearchParams('sort=code'));
 for(let i=1;i<sorted.length;i++) expect(sorted[i-1].style_codes.join('/').localeCompare(sorted[i].style_codes.join('/'),'en',{numeric:true})).toBeLessThanOrEqual(0);
 expect(products.map(p=>p.slug)).toEqual(before);
});
test('URL roundtrip preserves unrelated state, encoded colour and default removal',()=>{
 let q=new URLSearchParams('utm_source=test');
 q=setShopQuery(q,'colour','beige');q=setShopQuery(q,'sort','code');
 const round=new URL('https://example.org/collections/?'+q).searchParams;
 expect(round.get('colour')).toBe('beige');expect(round.get('sort')).toBe('code');expect(round.get('utm_source')).toBe('test');
 expect(setShopQuery(round,'sort','featured').has('sort')).toBe(false);
 expect(selectStyles(products,round)).toEqual(selectStyles(products,q));
});
test('each swap comes from a SHA-bound back role of the same colour',()=>{
 expect(pageSize).toBe(8);
 for(const p of products) {const {front,back}=cardViews(p);expect(front.role).toBe('front');expect(back).toBeDefined();expect(back!.role).toBe('back');expect(back!.colour).toBe(front.colour);expect(back!.sha256_16||back!.web_sha256).toBeTruthy();expect(p.images).toContain(back!);}
});

test('Arabic category pages render the plural count before JavaScript runs', () => {
 const html = readFileSync('web/apparel/dist/ar/collections/zip-ups-jackets/index.html', 'utf8');
 expect(html.match(/<p data-shop-count[^>]*>([^<]*)<\/p>/)?.[1]).toBe('10 موديلات');
});

test('Arabic category counts follow the approved noun forms', () => {
 const forms = categoryCountForms('ar');
 for (const [total, expected] of [
  [0, 'لا موديلات'], [1, 'موديل واحد'], [2, 'موديلان'], [10, '10 موديلات'],
  [16, '16 موديلاً'], [115, '115 موديل'],
 ] as const) expect(formatCategoryCount('ar', total, forms)).toBe(expected);
});

test('every rendered listing MOQ line uses the owner 50-piece fact in all eight locales',()=>{ // owner 2026-11-18: "最低50件起订"
 expect(ownerMoq.ready).toBe(50);
 for(const locale of locales) {
  const html=readFileSync(`web/apparel/dist/${locale==='en'?'':locale+'/'}collections/index.html`,'utf8');
  const lines=[...html.matchAll(/<p\b[^>]*class="shop-moq"[^>]*>(.*?)<\/p>/g)].map(m=>m[1]);
  expect(lines).toHaveLength(products.length);
  for(const line of lines)expect(line).toBe(shopCardMoq(locale).replaceAll('&','&amp;'));
 }
});

test('custom minimum uses deterministic owner grouping across Node and Bun locale runtimes',()=>{
 expect(ownerMoq.custom).toBe(1000);
 expect(shopCustomMoq('ar')).toContain('1,000');
 expect(shopCustomMoq('fr')).toContain('1 000');
 expect(shopCustomMoq('de')).toContain('1.000');
});

test('Featured follows the eight owner-approved D-SHOP B identities without dropping catalog items',()=>{
 const ordered=featuredStyles(products);
 expect(ordered.slice(0,8).map(p=>p.style_codes[0])).toEqual(['59043','58065','8900-31','69018','TZ2804-7','0253','59011','8900-32']);
 expect(new Set(ordered.map(p=>p.slug))).toEqual(new Set(products.map(p=>p.slug)));
 expect(shopFacts(products.find(p=>p.slug==='apparel-59043')!).fit).toEqual(['oversized']);
 expect(selectStyles(products,new URLSearchParams('fabric=cotton&fit=oversized')).some(p=>p.slug==='apparel-59043')).toBe(true);
});
