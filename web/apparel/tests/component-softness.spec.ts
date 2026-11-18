/** SOFT-ROLL component contract against served pages, run by apparel CI.
 * Uses real templates and computed styles; full-bleed hero images are outside these component classes. */
import { test, expect } from '@playwright/test';

const classes = {
 button: '.button, button, .shop-quote',
 card: '.card, .home-model, .home-form, .rfq, .cookie-banner, .success-card, .recovery-card, .pdp-detail-card',
 'image-card': '.image-card, .gallery > picture, .wholesale-photo',
 thumb: '.gallery-thumbs a, .gallery-colours button',
 'info-box': '.info-box, .moq-line, .home-promo, .collection-promo, .capability-panel, .brief-panel, .capabilities-strip, .form-status',
 input: 'input:not([type=hidden]), textarea',
 tab: '[role=tab], .tab, .category-links a, .shop-tabs a',
 chip: '.chip, .facet, .home-badge, [data-shop-facet]',
};
const routes = ['/', '/collections/', '/collections/hoodie-set/', '/products/hoodie-set-k02/', '/oem-odm/', '/sampling/', '/about-us/', '/about/', '/contact/', '/privacy/', '/404.html', '/zh/', '/ar/about-us/'];

function lastLineWords(element: Element): string[] {
 const lines = new Map<number, string[]>();
 const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
 for (let node = walker.nextNode(); node; node = walker.nextNode()) {
  for (const match of (node.textContent || '').matchAll(/\S+/g)) {
   const range = document.createRange();
   range.setStart(node, match.index!); range.setEnd(node, match.index! + match[0].length);
   const y = Math.round(range.getBoundingClientRect().y);
   lines.set(y, [...(lines.get(y) || []), match[0]]);
  }
 }
 return lines.get(Math.max(...lines.keys())) || [];
}

for (const width of [1440, 768, 390]) test(`soft component radii and secondary hierarchy ${width}`, async ({ page }, testInfo) => {
 test.setTimeout(120000);
 await page.setViewportSize({ width, height: 900 });
 const totals = Object.fromEntries(Object.keys(classes).map(key => [key, 0]));
 const evidence: unknown[] = [];
 for (const route of routes) {
  await page.goto(route);
  await page.evaluate(() => document.fonts.ready);
  const banner = page.locator('[data-cookie-banner]');
  if (await banner.isVisible()) await page.locator('[data-cookie-choice=essential]').click();
  const result = await page.evaluate(selectors => {
   const corners = ['borderTopLeftRadius', 'borderTopRightRadius', 'borderBottomRightRadius', 'borderBottomLeftRadius'] as const;
   const counts: Record<string, number> = {}, failures: unknown[] = [];
   for (const [role, selector] of Object.entries(selectors)) {
    const nodes = [...document.querySelectorAll(selector)];
    counts[role] = nodes.length;
    for (const node of nodes) {
     const s = getComputedStyle(node), radii = corners.map(k => s[k]);
     if (radii.some(r => !(parseFloat(r) > 0))) failures.push({ role, element: node.tagName + '.' + node.className, radii });
    }
   }
   for (const node of document.querySelectorAll('.secondary')) {
    const s = getComputedStyle(node);
    if (s.backgroundColor !== 'rgba(0, 0, 0, 0)' || s.backgroundImage !== 'none') failures.push({ role: 'secondary', text: node.textContent?.trim(), background: s.backgroundColor, image: s.backgroundImage });
   }
   for (const [selector, wrap] of [['h1,h2,h3,h4', 'balance'], ['p,.lead', 'pretty']]) {
    for (const node of document.querySelectorAll(selector)) {
     if (node.closest('.sr-only')) continue;
     const actual = getComputedStyle(node).textWrap;
     if (actual !== wrap) failures.push({ role: 'typography', element: node.tagName, text: node.textContent?.trim().slice(0, 80), actual, expected: wrap });
    }
   }
   return { counts, failures };
  }, classes);
  evidence.push({ route, ...result });
  for (const [role, count] of Object.entries(result.counts)) totals[role] += count;
  expect.soft(result.failures, `${route}: component corners and transparent secondary fills`).toEqual([]);
  for (const secondary of await page.locator('.secondary').all()) {
   if (!await secondary.isVisible()) continue;
   await secondary.hover();
   await expect.soft(secondary, `${route}: secondary hover stays transparent`).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
   await secondary.focus();
   await expect.soft(secondary, `${route}: secondary focus stays transparent`).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
  }
  if (route === '/') {
   const photo = page.locator('.wholesale-photo'), grid = page.locator('.home-category-grid');
   const box = (await photo.boundingBox())!, alignment = (await grid.boundingBox())!;
   expect.soft(box.x, 'wholesale photo follows the content grid').toBeCloseTo(alignment.x, 0);
   expect.soft(box.x).toBeGreaterThan(0);
   for (const property of ['border-top-left-radius', 'border-top-right-radius', 'border-bottom-right-radius', 'border-bottom-left-radius']) {
    await expect.soft(photo).toHaveCSS(property, '20px');
    await expect.soft(photo.locator('img')).toHaveCSS(property, '20px');
   }
   const cards = await page.locator('.home-models .home-model').evaluateAll(els => els.map(el => {
    const r = el.getBoundingClientRect();
    return { model: (el as HTMLElement).dataset.homeModel, x: r.x, y: r.y, width: r.width, height: r.height };
   }));
   expect(cards).toHaveLength(3);
   const gaps = [];
   for (let i = 0; i < cards.length; i++) for (let j = i + 1; j < cards.length; j++) {
    const a = cards[i], b = cards[j];
    const gap = Math.max(b.x - a.x - a.width, a.x - b.x - b.width, b.y - a.y - a.height, a.y - b.y - b.height);
    gaps.push({ pair: [a.model, b.model], gap });
    expect.soft(gap, `${a.model} / ${b.model}: separate rounded cards`).toBeGreaterThanOrEqual(16);
   }
   if (width >= 768) {
    expect.soft(cards[0].width).toBeGreaterThan(cards[1].width);
    expect.soft(cards[1].y).toBeCloseTo(cards[2].y, 0);
   }
   await testInfo.attach(`home-card-gaps-${width}`, { body: JSON.stringify({ cards, gaps }, null, 2), contentType: 'application/json' });
   for (const copy of await page.locator('.home-models .model-copy').all()) {
    for (const side of ['top', 'right', 'bottom', 'left']) await expect.soft(copy).toHaveCSS(`padding-${side}`, '24px');
   }
   const lastWords = await page.locator('[data-home-model=custom] p').last().evaluate(lastLineWords);
   expect.soft(lastWords.length, 'custom body has no single-word final line').toBeGreaterThan(1);
   await testInfo.attach(`home-body-last-line-${width}`, { body: JSON.stringify(lastWords), contentType: 'application/json' });
  }
  if (route === '/sampling/') {
   const headings = page.getByRole('heading', { name: 'Labels & packaging', exact: true });
   expect(await headings.count()).toBeGreaterThan(0);
   for (const heading of await headings.all()) {
    const lastWords = await heading.evaluate(lastLineWords);
    expect.soft(lastWords.length, '#650 Labels & packaging has no single-word last line').toBeGreaterThan(1);
    await testInfo.attach(`sampling-last-line-${width}`, { body: JSON.stringify(lastWords), contentType: 'application/json' });
   }
  }
  if (route.includes('/products/') && width>=768) {
   const thumb = page.locator('.gallery-thumbs:not([hidden]) a').first();
   await thumb.click();
   await expect(thumb).toHaveAttribute('aria-current', 'true');
   await expect.soft(thumb.locator('img')).toHaveCSS('outline-style', 'solid');
   for (const property of ['border-top-left-radius', 'border-top-right-radius', 'border-bottom-right-radius', 'border-bottom-left-radius']) {
    await expect.soft(thumb).toHaveCSS(property, '12px');
    await expect.soft(page.locator('.moq-line')).toHaveCSS(property, '20px');
   }
  }
 }
 for (const [role, count] of Object.entries(totals)) expect(count, `${role} has real template coverage`).toBeGreaterThan(0);
 await testInfo.attach(`component-styles-${width}`, { body: JSON.stringify({ totals, pages: evidence }, null, 2), contentType: 'application/json' });
});
