/** Responsibility: explicit SSR host policy and canonical request identity.
 * Dependencies: hostname validator/Astro request type; callers: build config, published/error routes.
 * Invariants: no wildcard hosts, adapter fallback cannot select a tenant, no trusted proxy in this MVP.
 */
import type { AstroGlobal } from 'astro';
import { normalizeHostname } from './presentation';

export function readAllowedHosts(value: string | undefined): string[] {
  return (value ?? '127.0.0.1,localhost').split(',').map(host => normalizeHostname(host.trim()));
}

export function requestCanonical(context: Pick<AstroGlobal, 'url' | 'request'>): string | undefined {
  const headers = context.request.headers;
  if (['forwarded', 'x-forwarded-host', 'x-forwarded-proto', 'x-forwarded-port'].some(name => headers.has(name))) return;
  const rawHost = headers.get('host');
  if (!rawHost || !/^[a-z\d.-]+(?::\d{1,5})?$/i.test(rawHost)) return;
  try {
    const host = new URL(`${context.url.protocol}//${rawHost}`);
    if (host.username || host.password || host.pathname !== '/' || host.search || host.hash ||
        normalizeHostname(host.hostname) !== normalizeHostname(context.url.hostname) || host.port !== context.url.port) return;
    return new URL(context.url.pathname, context.url.origin).href;
  } catch { return; }
}
