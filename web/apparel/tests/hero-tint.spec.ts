/** Hero tint regression: Figma alpha, transmitted image pixels and worst-case WCAG contrast.
 * Runs on the built Home; a black/white source removes dependency on external image packaging. */
import { test, expect } from '@playwright/test';
import tint from '../design/hero-tint.json' with { type: 'json' };
const luminance = (rgb: number[]) => rgb.map(v => v / 255).map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4).reduce((sum,v,i) => sum + v * [.2126,.7152,.0722][i],0);
for (const width of [390,768,1280]) test(`hero transmits image and preserves AA contrast ${width}`, async ({ page }) => {
 await page.setViewportSize({width,height:900});
 await page.route('**/img/site/illustrative_home_hero-*', route => route.fulfill({contentType:'image/svg+xml',body:'<svg xmlns="http://www.w3.org/2000/svg" width="1600" height="1200"><path fill="white" d="M0 0h800v1200H0z"/><path fill="black" d="M800 0h800v1200H800z"/></svg>'}));
 await page.goto('/');
 const image = page.locator('.hero-picture img');
 await expect.poll(() => image.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth > 0)).toBe(true);
 const state = await page.locator('.hero-shade').evaluate(el => {
  const style = getComputedStyle(el),text = getComputedStyle(document.querySelector('.hero p')!);
  return {opacity:Number(style.opacity),background:style.backgroundColor,text:text.color};
 });
 expect(state.opacity).toBeCloseTo(tint.tint.opacity,5);
 const bg = state.background.match(/[\d.]+/g)!.map(Number),fg = state.text.match(/[\d.]+/g)!.map(Number);
 const alpha = state.opacity * (bg[3] ?? 1);
 expect(alpha).toBeGreaterThan(0);
 expect(alpha).toBeLessThan(1);
 // White is the brightest possible source pixel and therefore the lowest contrast
 // for this light foreground over a uniform dark tint. All darker pixels improve it.
 const worst = bg.slice(0,3).map(v => v * alpha + 255 * (1-alpha));
 expect((luminance(fg.slice(0,3))+.05)/(luminance(worst)+.05)).toBeGreaterThanOrEqual(4.5);
 const hero = await page.locator('.hero').boundingBox();
 const clip = {x:hero!.x+8,y:hero!.y+8,width:64,height:64};
 const visible = await page.screenshot({clip,animations:'disabled'});
 await page.locator('.hero-picture').evaluate(el => (el as HTMLElement).style.visibility='hidden');
 const hidden = await page.screenshot({clip,animations:'disabled'});
 expect(visible.equals(hidden),'hero image must change actual rendered pixels under the tint').toBe(false);
});
