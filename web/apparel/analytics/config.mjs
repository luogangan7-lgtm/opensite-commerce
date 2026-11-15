/** Build-only GA policy; consumed by Shell and header hook, never bundled without an ID. */
import runtime from './runtime-source.mjs';
export function measurementId(value) {
  if (!value) return '';
  if (!/^G-[A-Z0-9]+$/.test(value)) throw new Error('Invalid PUBLIC_GA4_ID format');
  return value;
}
// Serialized as a classic script so denied is queued before any page scripts run.

export function analyticsScript(id) { return id ? runtime.replace('__GA4_ID__', JSON.stringify(id)) : ''; }
export function contentSecurityPolicy(id, hashes = [], turnstile = false) {
  const challenge = turnstile ? ' https://challenges.cloudflare.com' : '';
  const hash = hashes.length ? ' ' + hashes.join(' ') : '';
  return `default-src 'self'; script-src 'self'${hash}${challenge}${id ? ' https://www.googletagmanager.com' : ''}; style-src 'self' 'unsafe-inline'; img-src 'self' data:${id ? ' https://*.google-analytics.com https://*.googletagmanager.com' : ''}; connect-src 'self'${challenge}${id ? ' https://*.google-analytics.com https://*.analytics.google.com https://*.googletagmanager.com' : ''}; frame-src 'self'${challenge}; font-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'`;
}
