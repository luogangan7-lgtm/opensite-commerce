/** PDP chart adapter. Depends only on the canonical owner-reviewed chart and mapping files.
 * No category/filename inference; crossed source rows never become offered sizes. */
import source from '../../../../docs/apparel-b2b/size-charts.json';
import assignments from '../../../../docs/apparel-b2b/size-chart-mapping.json';
export const sizeCharts = source.charts;
export type SizeChartId = keyof typeof sizeCharts;
type Assignment = { status: string; charts?: SizeChartId[] };
type Chart = { sizes: string[]; row_annotations?: Record<string, { crossed_in_source?: boolean }> };
export const sizeChartBindings = assignments.products as Record<string, Assignment>;
export function productSizeChart(slug: string) {
 const entry = sizeChartBindings[slug];
 const id = entry?.status === 'structured' ? entry.charts?.[0] : undefined;
 return id && Object.hasOwn(sizeCharts, id) ? { id, ...sizeCharts[id] } : undefined;
}
export function visibleChartSizes(chart: Chart) {
 return chart.sizes.map((size, index) => ({ size, index }))
  .filter(({ size }) => !chart.row_annotations?.[size]?.crossed_in_source);
}

/** Preserve the supplier range endpoints and convert only the weight column. */
export function chartWeight(value: string, locale: string) {
 if (locale === 'zh') return value;
 const range = value.replace(/\d+/g, endpoint => String(Math.round(Number(endpoint) / 2))).replace(/斤(?:以下)?$/, '');
 return value.endsWith('以下') ? '<' + range : range;
}
