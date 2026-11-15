/** PDP supplier-image regression; published tables must use structured measurements. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
test('PDP never renders a supplier chart image', () => {
 const source = readFileSync(new URL('../src/views/Product.astro', import.meta.url), 'utf8');
 expect(source).not.toContain('product.size_chart.map');
 expect(source).toContain('data-size-table');
});
import { productSizeChart, sizeChartBindings, visibleChartSizes } from '../src/data/size-charts';
import charts from '../../../docs/apparel-b2b/size-charts.json';
test('canonical assignments return source measurements and omit untranscribable products', () => {
 for (const [slug,entry] of Object.entries(sizeChartBindings)) {
  const chart = productSizeChart(slug);
  if (entry.status !== 'structured') { expect(chart).toBeUndefined(); continue; }
  const id = entry.charts![0];
  expect(chart!.rows).toEqual(charts.charts[id].rows);
  expect(chart!.sizes).toEqual(charts.charts[id].sizes);
  for (const row of Object.values(chart!.rows)) expect(row.length).toBe(chart!.sizes.length);
 }
 expect(productSizeChart('unmapped-product')).toBeUndefined();
 expect(productSizeChart('hoodie-set-k02')?.id).toBe('zip-025');
 expect(productSizeChart('hoodie-set-k01-0256-tz2804-7')).toBeUndefined();
 expect(productSizeChart('crew-fleece-8900-31')?.rows.sleeve.at(-1)).toBe(60);
 const hoodie = productSizeChart('cotton430-hoodie-58065')!;
 expect(hoodie.sizes).toContain('S');
 expect(hoodie.rows.sleeve[0]).toBeNull();
 expect(visibleChartSizes(hoodie).map(row => row.size)).not.toContain('S');
});
