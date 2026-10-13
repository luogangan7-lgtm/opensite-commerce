/** Public Read Context transport; depends only on generated OpenAPI types/fetch; SSR callers supply a fixed internal API origin, never PG credentials. */
import createClient from 'openapi-fetch';
import type { paths } from './schema';
export type PublishedSite=paths['/api/v1/public/sites/current']['get']['responses'][200]['content']['application/json'];
export class PublicSiteNotFoundError extends Error {
  constructor(){super('Published site not found');this.name='PublicSiteNotFoundError';}
}
export async function readPublishedSite(origin:string,hostname:string):Promise<PublishedSite> {
  const base=new URL(origin);
  if(!['http:','https:'].includes(base.protocol)||base.username||base.password||base.pathname!=='/'||base.search||base.hash) throw new Error('Read API requires an HTTP origin');
  const client=createClient<paths>({baseUrl:base.origin});
  const {data,response}=await client.GET('/api/v1/public/sites/current',{params:{query:{host:hostname}},signal:AbortSignal.timeout(5000),redirect:'error'});
  if(response.status===404) throw new PublicSiteNotFoundError();
  if(!response.ok||!data) throw new Error('Published site read failed');
  return data;
}
export { createCommerceClient } from './commerce';
export type { components as APIComponents } from './schema';
