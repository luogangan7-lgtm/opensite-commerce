/** Collection facets read explicit catalog descriptions; unknown weights never match a bucket.
 * Used by CollectionResults in the browser and contract tests; no inferred composition facts.
 */
import type { Product } from './catalog';
export function matchesFilters(product: Pick<Product,'fabric_weight_fit'>, query: URLSearchParams): boolean {
  const facts=product.fabric_weight_fit.toLowerCase();
  const lining=query.getAll('lining'), weight=query.getAll('weight');
  const fleece=/fleece[- ]lined|silver-fox fleece/.test(facts);
  const unlined=/\bunlined\b/.test(facts);
  const explicit=facts.match(/(?:weight:\s*(?:approx\.?\s*)?)?(\d{3})\s*(?:gsm|g\b)/);
  const gsm=explicit?Number(explicit[1]):undefined;
  const bucket=gsm===undefined?'':gsm<280?'light':gsm<=350?'mid':'heavy';
  return (!lining.length||lining.some(v=>v==='fleece'?fleece:v==='unlined'&&unlined)) && (!weight.length||weight.includes(bucket));
}
