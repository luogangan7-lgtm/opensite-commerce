/** Structured chart adapter for Product. Root catalog bindings are explicit; no category or filename inference.
 * Depends on the owner-supplied measurement JSON and published slug bindings; no runtime source-image access. */
import source from '../../../../docs/apparel-b2b/size-charts.json';
import bindings from './size-chart-bindings.json';
export const sizeCharts = source.charts;
export type SizeChartId = keyof typeof sizeCharts;
export const sizeChartBindings: Record<string, SizeChartId> = bindings as Record<string, SizeChartId>;
export function productSizeChart(slug: string) {
 const id = sizeChartBindings[slug];
 return id && Object.hasOwn(sizeCharts, id) ? { id, ...sizeCharts[id] } : undefined;
}
