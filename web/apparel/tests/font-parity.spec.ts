/** Native font loading check for CP0: family, weight and loaded FontFace must agree.
 * Uses built pages and self-hosted assets; weights match the 2026-11-16 native soft-B snapshot. */
import {test, expect} from '@playwright/test';
for (const [route, body, heading, weight] of [
 ['/', 'Manrope', 'Literata', '400'],
 ['/ar/', 'Noto Sans Arabic', 'Noto Sans Arabic', '500'],
 ['/zh/', 'Noto Sans SC', 'Noto Serif SC', '400'],
]) test(`frozen typography loads ${route}`, async ({page}) => {
 await page.goto(route);
 const actual = await page.evaluate(async () => {
  const h = document.querySelector('h1')!;
  const b = getComputedStyle(document.body), title = getComputedStyle(h);
  const family = (s: string) => s.split(',')[0].replaceAll('"', '').trim();
  const sample = document.documentElement.lang === 'zh' ? '工厂' : document.documentElement.lang === 'ar' ? 'مصنع' : 'Factory';
  const faces = await Promise.all([
   document.fonts.load(`${b.fontWeight} 16px "${family(b.fontFamily)}"`, sample),
   document.fonts.load(`${title.fontWeight} 32px "${family(title.fontFamily)}"`, sample),
  ]);
  return {body:family(b.fontFamily),heading:family(title.fontFamily),weight:title.fontWeight,loaded:faces.map(list => list.length > 0 && list.every(f => f.status === 'loaded'))};
 });
 expect(actual).toEqual({body,heading,weight,loaded:[true,true]});
});
