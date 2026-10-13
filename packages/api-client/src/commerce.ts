/** Responsibility: typed, generated-contract storefront transport for the server BFF.
 * Dependencies: openapi-fetch and generated paths; callers: Astro commerce boundary.
 * Invariants: fixed server origin, no browser capabilities, response text preserves int64 lexemes.
 */
import createClient from 'openapi-fetch';
import type { paths } from './schema';

export function createCommerceClient(origin: string, fetcher?: (request: Request) => Promise<Response>) {
  const url = new URL(origin);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.pathname !== '/' || url.search || url.hash) {
    throw new Error('Commerce API requires an HTTP origin');
  }
  return createClient<paths>({baseUrl: url.origin,...(fetcher ? {fetch:fetcher} : {})});
}
