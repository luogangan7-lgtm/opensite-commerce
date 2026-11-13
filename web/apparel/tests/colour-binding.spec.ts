/** C10: catalog-derived expectations exercise the existing PDP gallery on five styles. */
import { test, expect } from '@playwright/test';
import bindings from './colour-bindings.json' with { type: 'json' };
const styles = ['apparel-59011', 'apparel-59043', 'pullover-hoodie-69008', 'crew-fleece-8900-31', 'hoodie-set-k02'];
for (const slug of styles) test(`catalog colour selects its bound front: ${slug}`, async ({ page }) => {
  await page.goto(`/products/${slug}/`);
  for (const row of bindings.filter(row => row.slug === slug && row.src)) {
    const swatch = page.locator('[data-gallery-swatch]').filter({ has: page.locator(`img[src="${row.src}"]`) });
    await expect(swatch).toHaveAttribute('data-gallery-swatch', row.colour);
    await swatch.click();
    await expect(swatch).toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src', row.src!);
    await expect(page.locator('.gallery-thumbs:not([hidden]) a[data-role="front"]')).toHaveAttribute('href', row.src!);
  }
});

test('#552 cream back-only detail does not enter the red default gallery', async ({ page }) => {
  await page.goto('/products/crew-fleece-8900-53/');
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src', '/img/crew-fleece-8900-53/digital-model-02.webp');
  await expect(page.locator('[data-gallery-swatch="red"]')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('[data-gallery-swatch="cream"]')).toHaveCount(0);
  await expect(page.locator('.gallery-thumbs:not([hidden]) a[data-role="fabric"]')).toHaveCount(0);
});
