/** Exercises failure boundaries on the actual typed public API client; no network is used to accept untrusted origins. */
import { expect,test } from 'bun:test';
import { readPublishedSite,PublicSiteNotFoundError } from '../../packages/api-client/src';
for(const origin of ['file:///private','http://user:pass@localhost','http://localhost/path','http://localhost?host=evil','http://localhost#x']) {
  test(`read API rejects non-origin ${new URL(origin).protocol}`,async()=>{await expect(readPublishedSite(origin,'127.0.0.1')).rejects.toThrow('HTTP origin');});
}
test('a missing published site has a typed error',()=>{expect(new PublicSiteNotFoundError().name).toBe('PublicSiteNotFoundError');});
