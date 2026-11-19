/** WAVE9 P1 verifies single-view image coverage and the restored Home hero image. */
import { expect, test, type Locator, type Page } from '@playwright/test';
import products from '../src/data/products.json' with { type: 'json' };

const packaged = process.env.APPAREL_PACKAGED_IMAGES === '1';
const widths = [390, 768, 1440];
const sample = 'hoodie-set-k02';

async function expectFilled(image: Locator, label: string) {
  await expect(image).toBeVisible();
  if (packaged) await image.evaluate((node: HTMLImageElement) => node.decode());
  const coverage = await image.evaluate((node: HTMLImageElement) => {
    const frame = node.closest('picture')!.getBoundingClientRect();
    const box = node.getBoundingClientRect();
    const { objectFit, objectPosition } = getComputedStyle(node);
    const scale = (objectFit === 'cover' ? Math.max : Math.min)(
      box.width / node.naturalWidth, box.height / node.naturalHeight,
    );
    return {
      objectFit, objectPosition,
      gap: Math.max(
        box.left - frame.left, frame.right - box.right,
        box.top - frame.top, frame.bottom - box.bottom,
        frame.width - node.naturalWidth * scale, frame.height - node.naturalHeight * scale,
      ),
    };
  });
  expect.soft(coverage.objectFit, label).toBe('cover');
  expect.soft(coverage.objectPosition, label).toBe('50% 50%');
  if (packaged) expect.soft(coverage.gap, label).toBeLessThanOrEqual(1);
}

async function checkGallery(page: Page, prefix: string, slug: string, width: number) {
  await page.goto(`${prefix}/products/${slug}/`);
  const image = page.locator('[data-gallery-main]');
  for (const view of ['side', 'details']) {
    await page.locator(`[data-gallery-view=${view}]`).click();
    if (await image.isVisible()) await expectFilled(image, `${slug} ${view}`);
    else await expect(page.locator('[data-primary-unavailable]')).toBeVisible();
  }
  if (width >= 768) {
    for (const link of await page.locator('.gallery-thumbs:not([hidden]) a:not([data-role=front]):not([data-role=back])').all()) {
      const src = await link.getAttribute('href');
      await link.click();
      await expect(image).toHaveAttribute('src', src!);
      await expectFilled(image, `${slug} ${src}`);
    }
  }
}

for (const width of widths) for (const locale of ['en', 'ar', 'zh']) {
  test(`${locale} ${width}: single-view photos fill and Home restores slide 2`, async ({ page }) => {
    const prefix = locale === 'en' ? '' : '/' + locale;
    await page.setViewportSize({ width, height: 900 });
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await checkGallery(page, prefix, sample, width);

    await page.goto(prefix + '/');
    await page.locator('[data-slide-to="1"]').click();
    const active = page.locator('[data-slide].is-active img');
    await expect(active).toHaveAttribute('src', '/img/home/hero-overhead.webp');
    if (packaged) await active.evaluate((node: HTMLImageElement) => node.decode());
    await expect(page.locator('main .home-process [data-home-media="factory-floor"]')).toHaveCount(1);

    await page.evaluate(() => document.fonts.ready);
    const rows = await page.locator('.home-card-grid > [data-product-card]').evaluateAll(cards => {
      const rows = new Map<number, number[][]>();
      for (const card of cards) {
        const top = Math.round(card.getBoundingClientRect().top);
        const parts = ['.shop-photo', '.shop-card-body', '.shop-quote'].map(selector =>
          card.querySelector(selector)!.getBoundingClientRect(),
        );
        const [photo, copy, cta] = parts;
        const row = rows.get(top) ?? [];
        row.push([photo.width, photo.height, copy.top, copy.bottom, cta.top, cta.bottom]);
        rows.set(top, row);
      }
      return [...rows.values()];
    });
    expect(rows.flat()).toHaveLength(4);
    expect(rows[0]).toHaveLength(width === 1440 ? 4 : 2);
    for (const [rowIndex, row] of rows.entries()) {
      for (const [coordinate] of row[0].entries()) {
        const values = row.map(card => card[coordinate]);
        expect(Math.max(...values) - Math.min(...values), `card row ${rowIndex + 1}, coordinate ${coordinate}`).toBeLessThanOrEqual(2);
      }
    }
  });
}

for (const width of widths) {
  test(`packaged catalog single-view coverage at ${width}px`, async ({ page }) => {
    test.skip(!packaged, 'Requires the copy-images output');
    test.setTimeout(240_000);
    await page.setViewportSize({ width, height: 900 });
    for (const { slug } of products) await checkGallery(page, '', slug, width);
  });
}
