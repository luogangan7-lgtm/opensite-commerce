/** Shared F3 shell acceptance: all sizes, EN/AR/ZH, navigation and unchanged consent state. */
import { test, expect } from '@playwright/test';
for (const width of [390,768,1280]) for (const locale of ['en','ar','zh']) test(`${locale} shell ${width}`, async ({page}) => {
 await page.setViewportSize({width,height:900});
 const prefix=locale==='en'?'':`/${locale}`;
 await page.goto(prefix+'/');
 await page.evaluate(()=>document.fonts.ready);
 await expect(page.locator('header')).toHaveClass('header-over-image');
 await expect(page.locator('header .brand img')).toHaveAttribute('src','/brand/reverse.svg');
 const brand=await page.locator('header .brand img').boundingBox(); expect(brand!.width).toBe(160);
 const banner=page.locator('[data-cookie-banner]');
 await expect(banner).toBeVisible();
 expect(await banner.evaluate(el=>getComputedStyle(el).position)).toBe('fixed');
 const styles=await banner.locator('button').evaluateAll(els=>els.map(el=>{const s=getComputedStyle(el);return [s.backgroundColor,s.color,s.border,s.fontSize]}));
 expect(styles[0]).toEqual(styles[1]);
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 const dir=process.env.CP2_EVIDENCE;
 if(dir) await page.screenshot({path:`${dir}/after-${locale}-${width}-home-cookie.png`});
 await page.locator('[data-cookie-choice=essential]').click();
 if(width<1100) {
  await page.locator('[data-menu-toggle]').click();
  await expect(page.locator('#mobile-navigation')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('[data-menu-toggle]')).toHaveAttribute('aria-expanded','false');
 }
 await page.goto(prefix+'/collections/');
 await expect(page.locator('header')).not.toHaveClass('header-over-image');
 await expect(page.locator('header .brand img')).toHaveAttribute('src','/brand/primary.svg');
 if(dir) await page.screenshot({path:`${dir}/after-${locale}-${width}-normal.png`});
 await page.locator('footer').scrollIntoViewIfNeeded();
 expect(await page.locator('.footer').evaluate(el=>getComputedStyle(el).gridTemplateColumns.split(' ').length)).toBe(width===1280?4:3);
 if(dir) await page.screenshot({path:`${dir}/after-${locale}-${width}-footer.png`});
 await page.goto(prefix+'/privacy/#cookies');
 await expect(banner).toBeVisible();
 await page.locator('[data-cookie-choice=all]').click();
 expect(await page.evaluate(()=>localStorage.getItem('sourcyo-cookie-consent-v1'))).toBe('all');
});
