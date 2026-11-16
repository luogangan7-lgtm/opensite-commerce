/** Owner-approved store listing: static fallback, URL facets, ordering, pagination, RTL and CLS. */
import {test,expect} from '@playwright/test';
if(process.env.CP6_BROWSER_EXECUTABLE) test.use({launchOptions:{executablePath:process.env.CP6_BROWSER_EXECUTABLE}});
import {readFileSync} from 'node:fs';
import type {Product} from '../src/data/catalog';
const products:Product[]=JSON.parse(readFileSync(new URL('../src/data/products.json',import.meta.url),'utf8'));
import {selectStyles,cardViews,featuredStyles} from '../src/data/shop';
for(const locale of ['en','ar','zh']) for(const width of [390,768,1280,1440]) {
 test(`${locale} store listing ${width}`,async({page})=>{
  await page.setViewportSize({width,height:900});
  await page.addInitScript(()=>{
   (window as any).shopCLS=0;
   new PerformanceObserver(list=>{for(const entry of list.getEntries()) if(!(entry as any).hadRecentInput)(window as any).shopCLS+=(entry as any).value;}).observe({type:'layout-shift',buffered:true});
  });
  const prefix=locale==='en'?'':`/${locale}`;
  await page.goto(`${prefix}/collections/`);
  await expect(page.locator('[data-shop-card]:visible')).toHaveCount(24);
  await page.evaluate(()=>document.fonts.ready);
  const rowGeometry=await page.locator('[data-shop-card]:visible').evaluateAll(cards=>{
   const rows=new Map<number,{height:number;bottom:number}[]>();
   for(const card of cards){const r=card.getBoundingClientRect(),a=card.querySelector('.shop-quote')!.getBoundingClientRect(),key=Math.round(r.y);rows.set(key,[...(rows.get(key)||[]),{height:r.height,bottom:a.bottom}]);}
   return [...rows.values()].map(row=>({heights:Math.max(...row.map(c=>c.height))-Math.min(...row.map(c=>c.height)),actions:Math.max(...row.map(c=>c.bottom))-Math.min(...row.map(c=>c.bottom))}));
  });
  for(const row of rowGeometry){expect(row.heights).toBeLessThanOrEqual(1);expect(row.actions).toBeLessThanOrEqual(1);}

  await expect(page.locator('html')).toHaveAttribute('dir',locale==='ar'?'rtl':'ltr');
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
  const rects=await page.locator('[data-shop-card]:visible').evaluateAll(els=>els.slice(0,2).map(e=>e.getBoundingClientRect().x));
  expect(locale==='ar'?rects[0]>rects[1]:rects[0]<rects[1]).toBe(true);
  if(width===1440) {
   const box=await page.locator('.shop-grid').boundingBox();
   expect(locale==='ar'?box!.x:box!.x+box!.width).toBeCloseTo(locale==='ar'?48:1392,0);
  }
  const photo=page.locator('.shop-photo').first(), box=await photo.boundingBox();
  expect(box!.width/box!.height).toBeCloseTo(.8,2);
  const first=featuredStyles(products)[0],back=cardViews(first).back!;
  await expect(photo.locator('[data-role="back"]')).toHaveAttribute('src',back.src);
  await expect(photo.locator('[data-shop-view-toggle-slot]')).toHaveCount(1);
  await expect(page.locator('.shop-card').first()).not.toContainText(/front\s*\/\s*back/i);
  // The flip button comes from the shared ViewToggle (PR657); until it is mounted the slot stays empty.
  const toggle=photo.locator('[data-shop-view-toggle-slot] button');
  if(await toggle.count()){
   await expect(toggle).toHaveAccessibleName(/\S/);
   await expect(toggle).not.toHaveAccessibleName(/^↻$/);
   if(width===390){const target=(await toggle.boundingBox())!;expect(target.width).toBeGreaterThanOrEqual(44);expect(target.height).toBeGreaterThanOrEqual(44);}
   await toggle.click();
   await expect(photo).toHaveAttribute('data-swapped','true');
  }
  await page.locator('[data-shop-more]').click();
  await expect(page.locator('[data-shop-card]:visible')).toHaveCount(48);
  if(width<=1024) await page.locator('[data-shop-filter-open]').click();
  await page.locator('[data-shop-facet="colour"]').selectOption('black');
  if(width<=1024) await page.locator('[data-shop-filter-dialog] form button').click();
  await page.locator('[data-shop-sort]').selectOption('code');
  await expect(page).toHaveURL(/colour=black.*sort=code/);
  await page.reload();
  const expected=selectStyles(featuredStyles(products),new URLSearchParams('colour=black&sort=code')).slice(0,24).map(p=>p.slug);
  await expect(page.locator('[data-shop-card]:visible')).toHaveCount(expected.length);
  expect(await page.locator('[data-shop-card]:visible').evaluateAll(els=>els.map(e=>(e as HTMLElement).dataset.shopCard))).toEqual(expected);
  if(width<=1024) await page.locator('[data-shop-filter-open]').click();
  await page.locator('[data-shop-clear]').click();
  if(width<=1024) await page.locator('[data-shop-filter-dialog] form button').click();
  await expect(page.locator('[data-shop-card]:visible')).toHaveCount(24);
  await page.goBack();
  await expect(page.locator('[data-shop-sort]')).toHaveValue('code');
  if(locale==='en'&&width===1440) {
   await page.goto('/collections/');
   await page.evaluate(async()=>{await document.fonts.ready;await Promise.all([...document.images].filter(i=>{const r=i.getBoundingClientRect();return r.width>0&&r.height>0&&r.top<innerHeight&&r.bottom>0}).map(i=>i.decode().catch(()=>{})));});
   expect(await page.evaluate(()=>(window as any).shopCLS)).toBeLessThanOrEqual(.05);
  }
  await page.locator(`.shop-tabs a[href="${prefix}/collections/zip-ups-jackets/"]`).click();
  for(const target of await page.locator('.shop-tabs a, .shop-quote').evaluateAll(els=>els.slice(0,12).map(e=>{const r=e.getBoundingClientRect();return {h:r.height,w:r.width};})))expect(target.h).toBeGreaterThanOrEqual(43.5);
  const tab=await page.locator('.shop-tabs [aria-current]').boundingBox(),rail=await page.locator('.shop-tabs').boundingBox();
  expect(tab!.x).toBeGreaterThanOrEqual(rail!.x);expect(tab!.x+tab!.width).toBeLessThanOrEqual(rail!.x+rail!.width+1);
  await expect(page.locator('[data-shop-card]:visible')).toHaveCount(products.filter(p=>p.category==='zip-ups-jackets').length);
 });
}
test('without JS the complete catalog is visible',async({browser,baseURL})=>{
 const context=await browser.newContext({javaScriptEnabled:false,baseURL});const page=await context.newPage();
 await page.goto('/collections/');await expect(page.locator('[data-shop-card]:visible')).toHaveCount(products.length);await context.close();
});
