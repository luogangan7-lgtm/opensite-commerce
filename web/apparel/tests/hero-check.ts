/** Shared live/fixture hero assertions. Synthetic tests cover both review decisions without publishing. */
import { expect, type Page } from '@playwright/test';
export async function checkHero(page: Page, manifest: any, request: any) {
 const review = manifest.slotReviews.find((row: {id:string}) => row.id === 'home/hero');
 expect(review).toBeDefined();
 if (!review.pass) {
  await expect(page.locator('[data-slot="home/hero"]')).toHaveCount(0);
  await expect(page.locator('link[rel="preload"][href*="illustrative_home_hero"]')).toHaveCount(0);
  expect(await page.evaluate(() => performance.getEntriesByType('resource').some(e => e.name.includes('illustrative_home_hero')))).toBe(false);
  expect(manifest.files.some((row: {url:string}) => row.url.includes('illustrative_home_hero'))).toBe(false);
  return null;
 }
 const hero = manifest.files.find((file: {url:string}) => file.url === '/img/site/illustrative_home_hero-1024.avif');
 await expect.poll(() => page.locator('[data-slot="home/hero"] img').evaluate(node => new URL((node as HTMLImageElement).currentSrc).pathname)).toBe(hero.url);
 const response = await request.get(hero.url), bytes = (await response.body()).length;
 expect(bytes).toBe(hero.bytes); expect(bytes).toBeLessThan(300_000);
 return {file:hero.url,bytes};
}
