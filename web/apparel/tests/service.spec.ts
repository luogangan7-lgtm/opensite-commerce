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
 await expect(view.locator('h1')).toHaveCount(1);
 const cta=view.locator('[data-service-rfq]');
 await expect(cta).toHaveAttribute('href',`${prefix}/contact/${kind==='sampling'?'?type=sample':''}#rfq-form`);
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
