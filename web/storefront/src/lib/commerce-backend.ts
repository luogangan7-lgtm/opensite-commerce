/** Responsibility: map BFF operations onto the generated backend contract.
 * Dependencies: generated API client/types; caller: commerce-server only.
 * Invariants: literal OpenAPI paths, bounded responses, no float decoding or auth in a browser bundle.
 */
import { createCommerceClient, type APIComponents } from '@opensite/api-client';

type Options = {method?: string; body?: unknown; rawBody?: string; headers?: Record<string,string>};
export async function backendRequest(origin: string, path: string, token: string | undefined, options: Options = {}) {
  const client = createCommerceClient(origin, async request => {
    const response = await fetch(request);
    const reader = response.body?.getReader();
    if (!reader) return response;
    const chunks: Uint8Array[]=[]; let length=0;
    while (true) {
      const next=await reader.read();if(next.done)break;
      length+=next.value.byteLength;if(length>1_048_576){await reader.cancel();throw new Error('Response too large');}
      chunks.push(next.value);
    }
    return new Response(Buffer.concat(chunks),{status:response.status,headers:response.headers});
  });
  const headers: Record<string,string> = {Accept: 'application/json', ...(token ? {'X-Cart-Token':token} : {}), ...options.headers};
  const common = {headers, signal: AbortSignal.timeout(5000), redirect:'error' as const, parseAs:'text' as const};
  if (path === '/api/v1/testing/payments/fake/webhook' && options.method === 'POST' && options.rawBody) {
    const result = await client.POST('/api/v1/testing/payments/fake/webhook', {...common,
      params:{header:{'X-Fake-Signature':headers['X-Fake-Signature']!}},
      body:options.body as APIComponents['schemas']['FakePaymentEvent'],bodySerializer:()=>options.rawBody!});
    return {response:result.response,text:result.data ?? JSON.stringify(result.error)};
  }
  const checkoutHeader = {Origin:headers.Origin!, 'X-CSRF-Token':headers['X-CSRF-Token']!, 'X-Cart-ID':headers['X-Cart-ID']!, 'X-Cart-Token':token!};
  const matched = /^\/api\/v1\/t\/([a-f0-9-]+)\/(carts|checkout)(?:\/([a-f0-9-]+|preview))?(?:\/lines\/([a-f0-9-]+))?$/.exec(path);
  if (!matched) throw new Error('Unsupported commerce route');
  const tenant_id = matched[1]!, cart_id = matched[3]!, sku_id = matched[4]!;
  let result;
  if (matched[2] === 'carts') {
    if (!cart_id && options.method === 'POST') {
      result = await client.POST('/api/v1/t/{tenant_id}/carts', {...common,params:{path:{tenant_id}},body:options.body as APIComponents['schemas']['CreateCart']});
    } else if (cart_id && !sku_id && !options.method) {
      result = await client.GET('/api/v1/t/{tenant_id}/carts/{cart_id}', {...common,params:{path:{tenant_id,cart_id}}});
    } else if (sku_id && options.method === 'PUT') {
      result = await client.PUT('/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}', {...common,params:{path:{tenant_id,cart_id,sku_id},header:{'If-Match':headers['If-Match']!}},body:options.body as APIComponents['schemas']['SetCartLine']});
    } else if (sku_id && options.method === 'DELETE') {
      result = await client.DELETE('/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}', {...common,params:{path:{tenant_id,cart_id,sku_id},header:{'If-Match':headers['If-Match']!}}});
    }
  } else if (cart_id === 'preview' && options.method === 'POST') {
    result = await client.POST('/api/v1/t/{tenant_id}/checkout/preview', {...common,params:{path:{tenant_id},header:checkoutHeader},body:options.body as APIComponents['schemas']['CheckoutPreviewInput']});
  } else if (cart_id && !options.method) {
    result = await client.GET('/api/v1/t/{tenant_id}/checkout/{checkout_id}', {...common,params:{path:{tenant_id,checkout_id:cart_id},header:checkoutHeader}});
  } else if (!cart_id && options.method === 'POST') {
    result = await client.POST('/api/v1/t/{tenant_id}/checkout', {...common,params:{path:{tenant_id},header:{...checkoutHeader,'Idempotency-Key':headers['Idempotency-Key']!}},body:options.body as APIComponents['schemas']['CheckoutStart'],bodySerializer:()=>options.rawBody ?? JSON.stringify(options.body)});
  }
  if (!result) throw new Error('Unsupported commerce method');
  return {response:result.response, text:result.data ?? JSON.stringify(result.error)};
}
