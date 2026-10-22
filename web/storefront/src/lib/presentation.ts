/** Responsibility: validate storefront boundaries and exact monetary presentation.
 * Dependencies: standard URL/Intl; callers: SSR pages and React compositions.
 * Invariants: origins are server configuration, hrefs stay local, minor units never use float arithmetic.
 */
export function readAPIOrigin(value: string): string {
  const url = new URL(value);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password ||
      url.pathname !== '/' || url.search || url.hash) throw new Error('Invalid read API origin');
  return url.origin;
}

export function normalizeHostname(value: string): string {
  const hostname = value.toLowerCase().replace(/\.$/, '');
  if (!hostname || hostname.length > 253 || !/^[a-z\d.-]+$/.test(hostname) ||
      hostname.split('.').some(label => !label || label.startsWith('-') || label.endsWith('-'))) {
    throw new Error('Invalid storefront hostname');
  }
  return hostname;
}

/** Navigation is root-relative; reject encoded separators as well as literal browser URL escapes. */
export function safeHref(value: string): string | null {
  if (!value.startsWith('/') || value.startsWith('//') || /[\\\u0000-\u0020\u007f]/.test(value) || /%(?:2f|5c|0[0-9a-f]|1[0-9a-f]|20|7f)/i.test(value)) return null;
  return value;
}

export function formatAmount(amount: string, currency: string, locale: string): string {
  if (!/^\d+$/.test(amount) || !/^[A-Z]{3}$/.test(currency)) throw new Error('Invalid published price');
  const formatter = new Intl.NumberFormat(locale, { style: 'currency', currency });
  const digits = formatter.resolvedOptions().maximumFractionDigits ?? 2;
  const minor = BigInt(amount);
  const scale = 10n ** BigInt(digits);
  const fraction = (minor % scale).toString().padStart(digits, '0');
  // Intl localizes grouping/currency position for a BigInt, then we replace its zero fraction.
  const numbers = new Intl.NumberFormat(locale, { useGrouping: false });
  const localizedFraction = [...fraction].map(digit => numbers.format(Number(digit))).join('');
  return formatter.formatToParts(minor / scale).map(part => part.type === 'fraction' ? localizedFraction : part.value).join('');
}

/** Display price: the lowest available SKU price (exact minor-unit compare); none when nothing is sellable. */
export function lowestSellablePrice(skus: { price_minor: string; currency: string; available: boolean }[], locale: string): string | undefined {
  let best: { price_minor: string; currency: string } | undefined;
  for (const sku of skus) {
    if (!sku.available) continue;
    if (!best || BigInt(sku.price_minor) < BigInt(best.price_minor)) best = sku;
  }
  return best && formatAmount(best.price_minor, best.currency, locale);
}
