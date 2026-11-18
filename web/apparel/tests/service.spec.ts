/** CP-SVC actual output: all locales preserve sources, capability groups and RFQ entry. */
import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
const media: {id:string;kind:string;src:string;altKey:string}[] = JSON.parse(readFileSync(new URL('../service-media.json',import.meta.url),'utf8'));
const locales=['en','zh','es','fr','de','pt','ru','ar'];
const dictionaries: Record<string,Record<string,string>>=Object.fromEntries(locales.map(locale=>[locale,JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`,import.meta.url),'utf8'))]));
for(const locale of locales) for(const kind of ['oem-odm','sampling','shipping']) test(`${locale} ${kind} approved service contract`,async({page})=>{
 const prefix=locale==='en'?'':`/${locale}`;
 await page.goto(`${prefix}/${kind}/`);
 const view=page.locator(`[data-service-page="${kind}"]`);
 // The owner rejected centered cover crops that cut faces at the nose.
 // Check every catalog slot at each delivery width, including non-hero portraits.
 for(const width of [1440,900,768,390]) {
  await page.setViewportSize({width,height:844});
  for(const portrait of await view.locator('[data-service-media^="catalog-"] img').all()) {
   await expect(portrait).toHaveCSS('object-position','50% 0%');
   await expect(portrait).toHaveCSS('object-fit','cover');
   if(await portrait.isVisible()) {
    const sourceHeight=await portrait.evaluate(n=>{
     const r=n.getBoundingClientRect();return r.height/Math.max(r.width/1122,r.height/1402);
    });
    expect(sourceHeight).toBeGreaterThanOrEqual(480);
   }
  }
  if(kind==='oem-odm') await expect(view.locator('.opening')).toHaveCSS('padding-top',width>900?'48px':'24px');
  if(kind==='oem-odm') {
   const tiles=view.locator('[data-techniques] figure');
   await expect(tiles).toHaveCount(16);
   for(const tile of await tiles.all()) {
    await expect(tile.locator('[data-technique-label]')).toBeVisible();
    await expect(tile.locator('[data-technique-label]')).not.toHaveText('');
    await expect(tile.locator('[data-technique-description]')).toBeVisible();
    await expect(tile.locator('[data-technique-description]')).not.toHaveText('');
   }
   await page.evaluate(()=>document.fonts.ready);
   const rows=await tiles.evaluateAll(nodes=>{
    const groups=new Map<number,{labels:number[];descriptions:number[]}>();
    for(const n of nodes){const key=Math.round(n.getBoundingClientRect().top),row=groups.get(key)||{labels:[],descriptions:[]};row.labels.push(n.querySelector('h3')!.getBoundingClientRect().top);row.descriptions.push(n.querySelector('p')!.getBoundingClientRect().top);groups.set(key,row);}
    return [...groups.values()];
   });
   for(const row of rows) for(const positions of [row.labels,row.descriptions]) expect(Math.max(...positions)-Math.min(...positions)).toBeLessThan(1);
  }
  if(width>900) {
   for(const split of await view.locator('.split').all()) {
    const delta=await split.evaluate(n=>{const a=n.querySelector(':scope > div')!.getBoundingClientRect(),b=n.querySelector('picture')!.getBoundingClientRect();return Math.abs(a.top+a.height/2-b.top-b.height/2);});
    expect(delta).toBeLessThan(1);
   }
   if(kind==='oem-odm') {
    const tops = await view.locator('.matrix > div > .eyebrow').evaluateAll(nodes => nodes.map(node => node.getBoundingClientRect().top));
    expect(Math.abs(tops[0] - tops[1])).toBeLessThanOrEqual(2);
   }
  }
  if(kind==='oem-odm'&&width===1440) {
   const heights=await view.locator('.matrix > div').evaluateAll(nodes=>nodes.map(n=>n.getBoundingClientRect().height));
   expect(heights[0]).toBeCloseTo(heights[1],0);
  }
  if(locale==='ru'&&kind==='oem-odm'&&width===390) {
   const lastPhraseTogether=await view.locator('.quote h2').evaluate(n=>{
    const text=n.firstChild!,s=text.textContent!,last=s.lastIndexOf(' '),before=s.lastIndexOf(' ',last-1);
    const a=document.createRange(),b=document.createRange();
    a.setStart(text,before+1);a.setEnd(text,last);b.setStart(text,last+1);b.setEnd(text,s.length);
    return Math.abs(a.getBoundingClientRect().y-b.getBoundingClientRect().y)<1;
   });
   expect(lastPhraseTogether).toBe(true);
  }
  for(const grid of await view.locator('.three,.four,.techniques').all()) {
   const gap=await grid.evaluate(n=>n.getBoundingClientRect().top-n.previousElementSibling!.getBoundingClientRect().bottom);
   expect(gap).toBeCloseTo(48,0);
  }
 }
 await expect(view.locator('h1')).toHaveCount(1);
 const cta=view.locator('[data-service-rfq]');
 await expect(cta).toHaveAttribute('href',`${prefix}/contact/${kind==='sampling'?'?type=sample':''}#rfq-form`);
 const quoteWidth=await cta.evaluate(n=>({button:n.getBoundingClientRect().width,container:n.parentElement!.getBoundingClientRect().width}));
 expect(quoteWidth.button).toBeLessThan(quoteWidth.container);
 if(kind!=='shipping') await expect(view.locator('[data-capability-group]')).toHaveCount(4);
 if(kind==='oem-odm') {
  await expect(view.locator('[data-techniques] picture')).toHaveCount(16);
  for(const image of media.filter(row=>row.kind==='technique')) {
   const tile=view.locator(`[data-service-media="${image.id}"]`);
   await expect(tile.locator('img')).toHaveAttribute('src',image.src);
   await expect(tile.locator('img')).toHaveAttribute('alt',dictionaries[locale][image.altKey]);
   await expect(tile.locator('img')).toHaveAttribute('srcset',`${image.src.replace('.webp','-640.webp')} 640w, ${image.src} 1024w`);
   await expect(tile).toHaveText('');
  }
 }
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 await cta.click();
 await expect(page.locator('#rfq-form')).toBeVisible();
 if(kind==='sampling') await expect(page.locator('textarea[name="message"]')).toHaveValue(dictionaries[locale].requestSample);
});
