/** CP3 runtime gates from frozen Home/HeroCarousel interaction annotations.
 * Native built Home; clock control checks exact 6s timing without wall-clock sleeps. */
import {test, expect} from '@playwright/test';
const hero = '[data-home-carousel]';
test('carousel timing, hover/focus pause, persistent pause and keyboard', async ({page}) => {
 test.setTimeout(60000);
 await page.clock.install(); await page.goto('/');
 await page.locator('[data-cookie-choice=essential]').click();
 const carousel=page.locator(hero), dots=carousel.locator('[data-slide-to]');
 await expect(dots).toHaveCount(3);
 await expect(carousel).toHaveAttribute('data-active','0');
 // The installed clock keeps flowing during load: freeze it, then restart autoplay from a known instant.
 await page.clock.pauseAt(await page.evaluate(()=>Date.now()+1000));
 await carousel.locator('[data-playback]').evaluate((button:HTMLElement)=>{button.click();button.click();}); await page.mouse.move(0,890);
 await page.clock.fastForward(5999); await expect(carousel).toHaveAttribute('data-active','0');
 await page.clock.fastForward(1); await expect(carousel).toHaveAttribute('data-active','1');
 await dots.nth(2).hover({force:true}); await expect(carousel).toHaveAttribute('data-active','2');
 await page.clock.fastForward(12000); await expect(carousel).toHaveAttribute('data-active','2');
 await page.mouse.move(0,890); await page.clock.fastForward(6000); await expect(carousel).toHaveAttribute('data-active','0');
 await dots.nth(1).focus(); await expect(carousel).toHaveAttribute('data-active','1');
 await page.clock.fastForward(12000); await expect(carousel).toHaveAttribute('data-active','1');
 await page.keyboard.press('ArrowRight'); await expect(carousel).toHaveAttribute('data-active','2');
 await page.keyboard.press('ArrowLeft'); await expect(carousel).toHaveAttribute('data-active','1');
 await carousel.locator('[data-playback]').focus(); await page.keyboard.press('Space');
 await page.keyboard.press('Tab'); await page.mouse.move(0,890);
 await page.clock.fastForward(12000); await expect(carousel).toHaveAttribute('data-active','1');
 await expect(carousel.locator('[data-playback]')).toHaveText('Play');
 await carousel.locator('[data-playback]').focus(); await page.keyboard.press('Enter');
 await page.keyboard.press('Tab'); await page.mouse.move(0,890);
 await page.clock.fastForward(6000); await expect(carousel).toHaveAttribute('data-active','2');
});
test('reduced motion disables autoplay and crossfade; RTL reverses arrows', async ({page}) => {
 await page.emulateMedia({reducedMotion:'reduce'}); await page.clock.install(); await page.goto('/ar/');
 const carousel=page.locator(hero); await expect(carousel).toHaveAttribute('data-active','0');
 await page.clock.fastForward(18000); await expect(carousel).toHaveAttribute('data-active','0');
 await expect(carousel.locator('[data-playback]')).toHaveText('تشغيل');
 await carousel.locator('[data-slide-to]').first().focus(); await page.keyboard.press('ArrowRight');
 await expect(carousel).toHaveAttribute('data-active','2');
 expect(await carousel.locator('[data-slide]').first().evaluate(el=>getComputedStyle(el).transitionDuration)).toBe('0s');
});
for (const locale of ['en','ar','zh']) for (const width of [390,768,1280]) test(`Home layout ${locale} ${width}`, async ({page})=>{
 await page.setViewportSize({width,height:900}); await page.emulateMedia({reducedMotion:'reduce'});
 await page.goto(locale==='en'?'/':`/${locale}/`); await page.evaluate(()=>document.fonts.ready);
 if(process.env.CP3_SCREENSHOTS) {
  // Lazy images only load near the viewport: walk the page, then wait for every image to decode.
  await page.evaluate(async()=>{for(let y=0;y<document.body.scrollHeight;y+=500){scrollTo(0,y);await new Promise(r=>setTimeout(r,80));}scrollTo(0,0);await Promise.all([...document.images].map(img=>img.decode().catch(()=>{})));});
  const path=process.env.CP3_SCREENSHOTS;
  await page.screenshot({path:`${path}/${locale}-${width}-full.png`,fullPage:true});
  const sections=page.locator('main > section');
  for(let i=0;i<await sections.count();i++) await sections.nth(i).screenshot({path:`${path}/${locale}-${width}-section-${i}.png`});
 }
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 await expect(page.locator('[data-home-media="fabric"] img')).toHaveCSS('object-fit', 'cover');
 await expect(page.locator('[data-home-category]')).toHaveCount(4);
 await expect(page.locator('[data-home-category] img').nth(3)).toHaveAttribute('src',/hoodie-set-k01-0256-tz2800-10\/digital-model-03/);
 if(width===390) expect(await page.locator('[data-home-categories]').evaluate(el=>getComputedStyle(el).gridTemplateColumns.split(' ').length)).toBe(2);
});
for (const locale of ['en','zh','es','fr','de','pt','ru','ar']) test(`HOME-BM models and private-label journey ${locale}`, async ({page}) => {
 await page.emulateMedia({reducedMotion:'reduce'});
 const prefix=locale==='en'?'':`/${locale}`;
 await page.goto(`${prefix}/`);
 await page.locator('[data-cookie-choice=essential]').click();
 const models=page.locator('[data-home-model]');
 await expect(models).toHaveCount(3);
 expect(await models.evaluateAll(rows=>rows.map(row=>row.getAttribute('data-home-model')))).toEqual(['wholesale','private','custom']);
 for (const [i,route] of ['/collections/','/collections/?intent=private-label','/oem-odm/'].entries()) await expect(models.nth(i).locator('a')).toHaveAttribute('href',prefix+route);
 const sections=await page.locator('main > section').evaluateAll(rows=>rows.map(row=>[...row.classList].find(c=>/^home-(hero|tiles|categories|products|band|process|fabric|terms|closing)$/.test(c))));
 expect(sections).toEqual(['home-hero','home-tiles','home-categories','home-products','home-band','home-process','home-fabric','home-terms','home-closing']);
 const carousel=page.locator(hero);
 await expect(carousel.locator('[data-hero-cta]')).toHaveAttribute('href',prefix+'/collections/');
 const ctaBox=await carousel.locator('[data-hero-cta]').boundingBox(), dotsBox=await carousel.locator('.hero-dots').boundingBox();
 expect(ctaBox!.y+ctaBox!.height).toBeLessThanOrEqual(dotsBox!.y);
 await carousel.locator('[data-slide-to="1"]').click();
 await expect(carousel.locator('[data-hero-cta]')).toHaveAttribute('href',prefix+'/collections/?intent=private-label');
 await carousel.locator('[data-slide-to="2"]').click();
 await expect(carousel.locator('[data-hero-cta]')).toHaveAttribute('href',prefix+'/oem-odm/');
 const privateNote=(await models.nth(1).locator('h3').innerText()).replace(/^02 · /,'');
 await models.nth(1).locator('a').click();
 await expect(page).toHaveURL(new RegExp(`${prefix}/collections/\\?intent=private-label$`));
 const product=page.locator('.shop-photo > a').first();
 await expect(product).toHaveAttribute('href',/intent=private-label/);
 await product.click();
 await expect(page.locator('[data-rfq-intent=quote]')).toHaveAttribute('href',/intent=private-label/);
 await page.locator('[data-rfq-intent=quote]').click();
 await expect(page.locator('[name=message]')).toHaveValue(new RegExp(privateNote.replace(/[.*+?^${}()|[\]\\]/g,'\\$&')));
 await expect(page.locator('[name=quantity_note]')).toHaveValue('');
});
