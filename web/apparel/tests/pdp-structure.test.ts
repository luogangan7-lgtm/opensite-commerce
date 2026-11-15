/** PDP supplier-image regression; published tables must use structured measurements. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
test('PDP never renders a supplier chart image', () => {
 const source = readFileSync(new URL('../src/views/Product.astro', import.meta.url), 'utf8');
 expect(source).not.toContain('product.size_chart.map');
 expect(source).toContain('data-size-table');
});
import { productSizeChart, sizeChartBindings } from '../src/data/size-charts';
import charts from '../../../docs/apparel-b2b/size-charts.json';
test('every explicit binding returns the unmodified source measurements', () => {
 for (const [slug,id] of Object.entries(sizeChartBindings)) {
  const chart = productSizeChart(slug)!;
  expect(chart.rows).toEqual(charts.charts[id].rows);
  expect(chart.sizes).toEqual(charts.charts[id].sizes);
  for (const row of Object.values(chart.rows)) expect(row.length).toBe(chart.sizes.length);
 }
 expect(productSizeChart('unmapped-product')).toBeUndefined();
 expect(productSizeChart('cotton430-hoodie-58065')?.sizes).not.toContain('S');
});
