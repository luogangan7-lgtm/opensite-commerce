/** Rendered control geometry for the twelve WAVE9 templates after cookie consent. */
import { expect, test, type Page } from '@playwright/test';

const templates = {
  home: '/', 'all-styles': '/collections/', category: '/collections/cotton430-hoodie/',
  pdp: '/products/cotton430-hoodie-58065/', 'oem-odm': '/oem-odm/', contact: '/contact/',
  'about-us': '/about-us/', sampling: '/sampling/', shipping: '/shipping/',
  privacy: '/privacy/', terms: '/terms/', '404': '/404/',
};

async function overlaps(page: Page) {
  return page.evaluate(() => {
    const controls = [...document.querySelectorAll('a, button, [role=button], input, select, summary')];
    // Measure the whole rendered page at this viewport width, clipping scrollers
    // and off-canvas content to the part of each control that can actually be seen.
    const boxes = controls.flatMap(element => {
      if (!element.checkVisibility({ opacityProperty: true, visibilityProperty: true })) return [];
      const clips = [];
      for (let parent = element.parentElement; parent && parent !== document.body; parent = parent.parentElement) {
        const { overflowX, overflowY } = getComputedStyle(parent);
        if (overflowX === 'visible' && overflowY === 'visible') continue;
        const rect = parent.getBoundingClientRect();
        clips.push({ rect, parent, overflowX, overflowY });
      }
      return [...element.getClientRects()].flatMap(rect => {
        let left = Math.max(0, rect.left), right = Math.min(window.innerWidth, rect.right);
        let top = Math.max(0, rect.top), bottom = rect.bottom;
        for (const { rect, parent, overflowX, overflowY } of clips) {
          if (overflowX !== 'visible') {
            left = Math.max(left, rect.left + parent.clientLeft);
            right = Math.min(right, rect.left + parent.clientLeft + parent.clientWidth);
          }
          if (overflowY !== 'visible') {
            top = Math.max(top, rect.top + parent.clientTop);
            bottom = Math.min(bottom, rect.top + parent.clientTop + parent.clientHeight);
          }
        }
        return right > left && bottom > top ? [{ element, left, right, top, bottom }] : [];
      });
    }).sort((a, b) => a.top - b.top);
    const label = (element: Element) => {
      const name = element.getAttribute('aria-label') || element.textContent?.trim() || element.getAttribute('name') || '';
      return `${element.localName} ${name.replace(/\s+/g, ' ').slice(0, 80)}`;
    };
    const failures = [];
    for (const [i, a] of boxes.entries()) {
      for (let j = i + 1; j < boxes.length; j++) {
        const b = boxes[j];
        const height = Math.min(a.bottom, b.bottom) - b.top;
        if (b.top >= a.bottom - 1) break;
        if (a.element.contains(b.element) || b.element.contains(a.element)) continue;
        const width = Math.min(a.right, b.right) - Math.max(a.left, b.left);
        if (height > 1 && width > 1) {
          failures.push(`${label(a.element)} / ${label(b.element)}: ${width.toFixed(2)} × ${height.toFixed(2)} px`);
        }
      }
    }
    return failures;
  });
}

for (const width of [390, 768, 1440]) for (const locale of ['en', 'ar', 'zh']) {
  for (const [name, path] of Object.entries(templates)) {
    test(`${name} ${locale} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(name === '404' && locale === 'en' ? '/404.html' : (locale === 'en' ? '' : `/${locale}`) + path);
      await page.locator('[data-cookie-choice="essential"]').click();
      await expect(page.locator('[data-cookie-banner]')).toBeHidden();
      await page.evaluate(() => document.fonts.ready);
      if (name === 'home') {
        const hero = page.locator('[data-home-carousel]');
        const slides = hero.locator('[data-slide-to]');
        await expect(slides).toHaveCount(3);
        for (const index of [0, 1, 2]) {
          await slides.nth(index).focus();
          await expect(hero).toHaveAttribute('data-active', String(index));
          await page.evaluate(() => window.scrollTo(0, 0));
          expect.soft(await overlaps(page), `slide ${index + 1}`).toEqual([]);
        }
      } else {
        expect(await overlaps(page)).toEqual([]);
      }
    });
  }
}
