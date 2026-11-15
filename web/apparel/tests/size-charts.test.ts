/** Supplier chart coverage contract; reads catalog + chart data only, run by apparel CI.
 * Existing catalog image assignments are authoritative; new assignments require exact codes.
 * Null measurements require an explicit unreadable/disputed cell annotation; no inferred digits.
 */
import { test, expect } from 'bun:test';
import data from '../../../docs/apparel-b2b/size-charts.json';
import assignments from '../../../docs/apparel-b2b/size-chart-mapping.json';
import products from '../src/data/products.json';

type Chart = { sizes: string[]; rows: Record<string, (number | string | null)[]>; source_image?: string; style_code?: string; title_zh: string; row_annotations?: Record<string, { unreadable_cells?: string[]; disputed_cells?: string[] }> };
type Mapping = { status: string; charts?: string[]; reason?: string; basis?: string; source_images?: string[]; matching_style_code?: string };
const charts: Record<string, Chart> = data.charts;
const mappings: Record<string, Mapping> = assignments.products;
function coverage(entries: Record<string, Mapping>) {
 for (const product of products) {
  const entry = entries[product.slug];
  if (!entry) throw new Error(`Missing chart decision: ${product.slug}`);
  if (entry.status === 'untranscribable') {
   if (!entry.reason?.trim()) throw new Error(`Missing reason: ${product.slug}`);
   continue;
  }
  if (entry.status !== 'structured' || !entry.charts?.length) throw new Error(`Missing structured chart: ${product.slug}`);
  for (const key of entry.charts) if (!charts[key]) throw new Error(`Unknown chart: ${key}`);
  if (product.size_chart.length) {
   if (JSON.stringify(entry.source_images) !== JSON.stringify(product.size_chart)) throw new Error(`Changed source assignment: ${product.slug}`);
   for (const source of product.size_chart) if (!entry.charts.some(key => charts[key].source_image === source)) throw new Error(`Unmapped image: ${source}`);
  } else if (entry.basis === 'exact_style_code') {
   if (!entry.matching_style_code || !product.style_codes.includes(entry.matching_style_code) || !entry.charts.every(key => charts[key].style_code === entry.matching_style_code)) throw new Error(`Style mismatch: ${product.slug}`);
  } else if (!entry.charts.every(key => product.name_en === charts[key].title_zh)) {
   throw new Error(`No exact title match: ${product.slug}`);
  }
 }
}

test('every catalog product and every size_chart image has a structured chart or explicit reason', () => {
 coverage(mappings);
 expect(Object.keys(mappings).sort()).toEqual(products.map(p => p.slug).sort());
});
test('coverage rejects missing decisions, missing reasons, swapped source charts and substring style guesses', () => {
 const missing = { ...mappings }; delete missing[products[0].slug];
 expect(() => coverage(missing)).toThrow(/Missing chart decision/);
 expect(() => coverage({ ...mappings, [products[0].slug]: {status:'untranscribable'} })).toThrow(/Missing reason/);
 expect(() => coverage({ ...mappings, [products[0].slug]: { ...mappings[products[0].slug], charts:['tee-240-gd52021'] } })).toThrow(/Unmapped image/);
 expect(() => coverage({ ...mappings, 'hoodie-set-k01-0256': { status:'structured', basis:'exact_style_code', charts:['zip-025'], matching_style_code:'025' } })).toThrow(/Style mismatch/);
});
test('measurement columns preserve row alignment and explicitly explain every null', () => {
 for (const chart of Object.values(charts)) {
  expect(new Set(chart.sizes).size).toBe(chart.sizes.length);
  for (const [key, values] of Object.entries(chart.rows)) {
   expect(values.length).toBe(chart.sizes.length);
   values.forEach((value, i) => {
    if (value === null) {
     const note = chart.row_annotations?.[chart.sizes[i]];
     expect([...(note?.unreadable_cells || []), ...(note?.disputed_cells || [])]).toContain(key);
    } else if (key !== 'weight_jin') {
     expect(typeof value).toBe('number'); expect(Number(value)).toBeGreaterThan(0);
    }
   });
  }
 }
 expect(data.charts['hoodie-702'].row_annotations.S.crossed_in_source).toBe(true);
 expect(data.charts['hoodie-702'].rows.sleeve[0]).toBeNull();
});
