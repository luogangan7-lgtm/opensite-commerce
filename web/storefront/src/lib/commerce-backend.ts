/** Responsibility: map BFF operations onto the generated backend contract.
 * Dependencies: generated API client/types; caller: commerce-server only.
 * Invariants: literal OpenAPI paths, bounded responses, no float decoding or auth in a browser bundle.
 */
import { createCommerceClient, type APIComponents } from '@opensite/api-client';

type Schema = APIComponents['schemas'];
type StartWire = Omit<Schema['CheckoutStart'],'accepted_total'> & {accepted_total:{amount_minor:string;currency:string}};
type EventWire = Omit<Schema['FakePaymentEvent'],'amount_minor'|'order_version'> & {amount_minor:bigint;order_version:bigint};
export function commerceBackend(origin: string, token?: string, extraHeaders: Record<string,string> = {}) {
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
  const headers = {Accept: 'application/json', ...(token ? {'X-Cart-Token':token} : {}), ...extraHeaders};
  const common = {headers, signal: AbortSignal.timeout(5000), redirect:'error' as const, parseAs:'text' as const};
  const path = (tenant_id: string) => ({path:{tenant_id}});
  const checkoutHeader = {Origin:extraHeaders.Origin!, 'X-CSRF-Token':extraHeaders['X-CSRF-Token']!, 'X-Cart-ID':extraHeaders['X-Cart-ID']!, 'X-Cart-Token':token!};
  const customerParams = (tenant_id: string) => ({...path(tenant_id),header:{Origin:extraHeaders.Origin!,'X-CSRF-Token':extraHeaders['X-CSRF-Token']!}});
  // Only int64 wire bodies bridge generated number types; serializers retain exact bytes.
  return {
    createCart: (tenant_id: string, body: Schema['CreateCart']) => client.POST('/api/v1/t/{tenant_id}/carts', {...common,params:path(tenant_id),body}),
    readCart: (tenant_id: string, cart_id: string) => client.GET('/api/v1/t/{tenant_id}/carts/{cart_id}', {...common,params:{path:{tenant_id,cart_id}}}),
    setCartLine: (tenant_id: string, cart_id: string, sku_id: string, etag: string, body: Schema['SetCartLine']) => client.PUT('/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}', {...common,params:{path:{tenant_id,cart_id,sku_id},header:{'If-Match':etag}},body}),
    deleteCartLine: (tenant_id: string, cart_id: string, sku_id: string, etag: string) => client.DELETE('/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}', {...common,params:{path:{tenant_id,cart_id,sku_id},header:{'If-Match':etag}}}),
    previewCheckout: (tenant_id: string, body: Schema['CheckoutPreviewInput']) => client.POST('/api/v1/t/{tenant_id}/checkout/preview', {...common,params:{...path(tenant_id),header:checkoutHeader},body}),
    readCheckout: (tenant_id: string, checkout_id: string) => client.GET('/api/v1/t/{tenant_id}/checkout/{checkout_id}', {...common,params:{path:{tenant_id,checkout_id},header:checkoutHeader}}),
    startCheckout: (tenant_id: string, key: string, body: StartWire, rawBody: string) => client.POST('/api/v1/t/{tenant_id}/checkout', {...common,params:{...path(tenant_id),header:{...checkoutHeader,'Idempotency-Key':key}},body:body as unknown as Schema['CheckoutStart'],bodySerializer:()=>rawBody}),
    listOrders: (tenant_id: string, cursor?: string) => client.GET('/api/v1/t/{tenant_id}/customer/orders', {...common,params:{...path(tenant_id),query:{cursor}}}),
    readOrder: (tenant_id: string, order_id: string) => client.GET('/api/v1/t/{tenant_id}/customer/orders/{order_id}', {...common,params:{path:{tenant_id,order_id}}}),
    readSession: (tenant_id: string) => client.GET('/api/v1/t/{tenant_id}/customer/session', {...common,params:path(tenant_id)}),
    createSession: (tenant_id: string, body: Schema['CustomerCredentialsInput']) => client.POST('/api/v1/t/{tenant_id}/customer/session', {...common,params:customerParams(tenant_id),body}),
    deleteSession: (tenant_id: string) => client.DELETE('/api/v1/t/{tenant_id}/customer/session', {...common,params:customerParams(tenant_id)}),
    registerCustomer: (tenant_id: string, body: Schema['CustomerRegistrationInput']) => client.POST('/api/v1/t/{tenant_id}/customer/registrations', {...common,params:customerParams(tenant_id),body}),
    verifyCustomer: (tenant_id: string, body: Schema['CustomerTokenInput']) => client.POST('/api/v1/t/{tenant_id}/customer/verifications', {...common,params:customerParams(tenant_id),body}),
    fakeWebhook: (body: EventWire, rawBody: string, signature: string) => client.POST('/api/v1/testing/payments/fake/webhook', {...common,params:{header:{'X-Fake-Signature':signature}},body:body as unknown as Schema['FakePaymentEvent'],bodySerializer:()=>rawBody}),
  };
}
export type CommerceBackend = ReturnType<typeof commerceBackend>;
export type BackendResult = Awaited<ReturnType<CommerceBackend[keyof CommerceBackend]>>;
