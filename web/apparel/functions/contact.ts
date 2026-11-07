/** Contact GET/HEAD only; Pages asset binding serves the existing page without an external redirect. */
type Context = { request: Request; env: { ASSETS: { fetch(request: Request): Promise<Response> } } };
export function onRequestGet({ request, env }: Context): Promise<Response> {
  const url = new URL(request.url);
  url.pathname = '/contact/';
  return env.ASSETS.fetch(new Request(url, request));
}
export const onRequestHead = onRequestGet;
