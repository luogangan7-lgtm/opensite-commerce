/** CP-SHOP correctness against catalog, explicit unknowns and existing back provenance. */
import {test,expect} from 'bun:test';
import {products} from '../src/data/catalog';
import {shopFacts,selectStyles,setShopQuery,cardViews,pageSize} from '../src/data/shop';
test('facets intersect explicit product data and preserve Featured order',()=>{
 for(const category of new Set(products.map(p=>p.category))) {
  expect(selectStyles(products,new URLSearchParams({category}))).toEqual(products.filter(p=>p.category===category));
 }
 expect(selectStyles(products,new URLSearchParams('colour=black'))).toEqual(products.filter(p=>p.images.some(i=>i.role==='front'&&i.colour==='black')));
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
 q=setShopQuery(q,'colour','stone/greige');q=setShopQuery(q,'sort','code');
 const round=new URL('https://example.org/collections/?'+q).searchParams;
 expect(round.get('colour')).toBe('stone/greige');expect(round.get('sort')).toBe('code');expect(round.get('utm_source')).toBe('test');
 expect(setShopQuery(round,'sort','featured').has('sort')).toBe(false);
 expect(selectStyles(products,round)).toEqual(selectStyles(products,q));
});
test('each swap comes from a SHA-bound back role of the same colour',()=>{
 expect(pageSize).toBe(24);
 for(const p of products) {const {front,back}=cardViews(p);expect(front.role).toBe('front');expect(back).toBeDefined();expect(back!.role).toBe('back');expect(back!.colour).toBe(front.colour);expect(back!.sha256_16||back!.web_sha256).toBeTruthy();expect(p.images).toContain(back!);}
});
