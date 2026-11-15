/** Frozen Collection responsive and URL-filter acceptance; runs against static build. */
import { test, expect } from '@playwright/test';
if(process.env.CP6_BROWSER_EXECUTABLE) test.use({launchOptions:{executablePath:process.env.CP6_BROWSER_EXECUTABLE,args:['--disable-features=CDPScreenshotNewSurface']}});
for (const width of [390,768,1280,1440]) for (const locale of ['en','ar','zh']) {
 test(`${locale} collection ${width}`, async ({page})=>{
  await page.setViewportSize({width,height:900});
  const prefix=locale==='en'?'':`/${locale}`;
  for(const route of ['collections','collections/zip-ups-jackets']) {
   if(process.env.CP6_BEFORE && route.includes('zip-ups')) continue;
   await page.goto(`${prefix}/${route}/`);
   if(await page.locator('.cookie-banner button').first().isVisible())await page.locator('.cookie-banner button').first().click();
   await page.evaluate(async()=>{for(const img of document.images)img.loading='eager';await Promise.all([...document.images].map(i=>i.decode().catch(()=>{})));await document.fonts.ready;});
   expect(await page.evaluate(()=>document.fonts.status)).toBe('loaded');

   await page.screenshot({path:`${process.env.CP6_EVIDENCE||'evidence/apparel/collection'}/${process.env.CP6_BEFORE?'before':'after'}-${locale}-${width}-${route.replaceAll('/','-')}.png`,fullPage:true,timeout:5000});
   if(process.env.CP6_BEFORE) continue;
   if(width===1440 && locale==='en' && route==='collections') {
    const hero=page.locator('.collection-hero');
    expect((await hero.boundingBox())!.height).toBe(780);
    expect((await hero.locator(':scope > div').boundingBox())!.width).toBe(680);
    await expect(hero.locator('h1')).toHaveCSS('font-size','88px');
    await expect(hero.locator('h1')).toHaveCSS('line-height','91.52px');
    await expect(page.locator('.tile bdi')).toHaveText(['59043','58065','70171','70175']);
   }
   expect(await page.locator('main img').evaluateAll(imgs=>imgs.every(i=>(i as HTMLImageElement).naturalWidth>0))).toBe(true);
   await expect(page.locator('html')).toHaveAttribute('dir',locale==='ar'?'rtl':'ltr');
   expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
   await page.locator('[data-facet="weight"][value="heavy"]').click();
   await expect(page).toHaveURL(/weight=heavy/);
   await page.reload();
   await expect(page.locator('[data-facet="weight"][value="heavy"]')).toHaveAttribute('aria-pressed','true');
   if(route.includes('zip-ups')) {
    await expect(page.locator('[data-empty]')).toBeVisible();
    await page.locator('[data-clear]').click();
    await expect(page.locator('.product-grid > a:visible')).toHaveCount(10);
   }
  }
 });
}
