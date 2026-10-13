/** Responsibility: explicit SSR host policy and canonical request identity.
 * Dependencies: hostname validator/Astro request type; callers: build config, published/error routes.
 * Invariants: no wildcard hosts, adapter fallback cannot select a tenant, no trusted proxy in this MVP.
 * The build-time allowlist is compiled in through Vite `define`; unbundled tests read the env per call.
 */
import type { AstroGlobal } from 'astro';
import { normalizeHostname } from './presentation';

export function readAllowedHosts(value: string | undefined): string[] {
  return (value ?? '127.0.0.1,localhost').split(',').map(host => normalizeHostname(host.trim()));
}

declare const __OSC_STOREFRONT_ALLOWED_HOSTS__: string[] | undefined;
export function buildAllowedHosts(): string[] {
  return typeof __OSC_STOREFRONT_ALLOWED_HOSTS__ !== 'undefined' ? __OSC_STOREFRONT_ALLOWED_HOSTS__
    : readAllowedHosts(process.env.OSC_STOREFRONT_ALLOWED_HOSTS);
}

export function requestCanonical(context: Pick<AstroGlobal, 'url' | 'request'>, allowedHosts = buildAllowedHosts()): string | undefined {
  const headers = context.request.headers;
  if (['forwarded', 'x-forwarded-host', 'x-forwarded-proto', 'x-forwarded-port'].some(name => headers.has(name))) return;
  const rawHost = headers.get('host');
  if (!rawHost || !/^[a-z\d.-]+(?::\d{1,5})?$/i.test(rawHost)) return;
  try {
    const host = new URL(`${context.url.protocol}//${rawHost}`);
    // Astro's adapter can fall back to localhost for a rejected Host; admit only the deployment list.
    if (!allowedHosts.includes(normalizeHostname(host.hostname))) return;
    if (host.username || host.password || host.pathname !== '/' || host.search || host.hash ||
        normalizeHostname(host.hostname) !== normalizeHostname(context.url.hostname) || host.port !== context.url.port) return;
    const canonical = new URL(context.url.origin);
    canonical.pathname = context.url.pathname;
    return canonical.href;
  } catch { return; }
}
