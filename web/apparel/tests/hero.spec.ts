/** Actual browser assertions for omitted/passing hero, with synthetic markup and no private assets. */
import { test, expect } from '@playwright/test';
import { checkHero } from './hero-check';
for (const pass of [false,true]) test(`hero review ${pass}: network and markup agree`, async ({page}) => {
 const url='/img/site/illustrative_home_hero-1024.avif';
 await page.goto('/privacy/');
 await page.route('**/img/site/**', route => route.fulfill({status:200,contentType:'image/avif',body:Buffer.from('fixture')}));
 await page.setContent(pass ? `<picture data-slot="home/hero"><img src="${url}"></picture>` : '<main>Hero omitted</main>');
 let calls=0;
 const receipt=await checkHero(page,{slotReviews:[{id:'home/hero',pass}],files:pass?[{url,bytes:7}]:[]},{get:async()=>{calls++;return {body:async()=>Buffer.from('fixture')};}});
 expect(receipt).toEqual(pass?{file:url,bytes:7}:null);
 expect(calls).toBe(pass?1:0);
});
