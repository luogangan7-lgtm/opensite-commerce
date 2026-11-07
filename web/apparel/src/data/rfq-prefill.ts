/** Contact query boundary; catalog owns products, only contract enums enter the form. */
import { products } from './catalog';
export function parseRfqPrefill(query: URLSearchParams) {
  const single = (key: string) => query.getAll(key).length === 1 ? query.get(key) : null;
  const product = products.find(p => p.slug === single('product'));
  const type = single('type');
  const subject = single('subject');
  return {
    ...(product ? { product } : {}),
    ...(type === 'sample' ? { type } : {}),
    ...(subject && ['fabric-photos', 'lining-photos', 'full-range', 'reference-photo'].includes(subject) ? { subject } : {}),
  };
}
