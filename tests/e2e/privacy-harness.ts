/** Run-scoped observation, installed automatically by fixture.ts; no spec registration.
 * Depends only on public Playwright APIs; never logs values or changes HTTP behavior.
 * Pending response reads drain before context closure and before collector completion.
 */
import type { APIRequestContext, APIResponse, Browser, BrowserContext, Response } from '@playwright/test';

export function installPrivacy(browser: Browser, request: APIRequestContext, playwright: { request: typeof import('@playwright/test').request },
  save: (...values: string[]) => Promise<void>) {
  const seen = new WeakSet<object>(), values = new Set<string>(), pending = new Set<Promise<void>>();
  const restore: (() => void)[] = [];
  let failed = false;
  const sensitive = /csrf|token|secret|password|proof|key/i;
  const add = (value: string) => { if (value) values.add(value); };
  function url(value: string) {
    for (const raw of value.match(/https?:\/\/[^\s"'<>]+/g) ?? []) {
      const parsed = new URL(raw);
      for (const [name, item] of parsed.searchParams) if (sensitive.test(name)) add(item);
      if (parsed.hash) add(parsed.hash.slice(1));
    }
  }
  function fields(value: unknown, name = '') {
    if (typeof value === 'string') {
      if (sensitive.test(name)) add(value);
      url(value);
    } else if (value instanceof URLSearchParams || value instanceof FormData) {
      for (const [key, item] of value.entries()) fields(item, key);
    } else if (Array.isArray(value)) value.forEach(item => fields(item, name));
    else if (value && typeof value === 'object')
      for (const [key, item] of Object.entries(value)) fields(item, key);
  }
  function body(value: unknown) {
    if (typeof value !== 'string' && !Buffer.isBuffer(value)) return fields(value);
    const text = value.toString();
    try { fields(JSON.parse(text)); }
    catch {
      for (const [key, item] of new URLSearchParams(text)) fields(item, key);
      for (const tag of text.match(/<(?:input|meta)\b[^>]*>/gi) ?? []) {
        const attrs = Object.fromEntries([...tag.matchAll(/([\w-]+)\s*=\s*["']([^"']*)["']/g)].map(match => [match[1].toLowerCase(), match[2]]));
        fields(attrs.value ?? attrs.content, attrs.name ?? attrs.type ?? '');
      }
      url(text);
    }
  }
  function headers(items: { name: string; value: string }[]) {
    for (const { name, value } of items) {
      if (/^set-cookie$/i.test(name)) {
        for (const cookie of value.split(/\n|,(?=\s*[^;,=\s]+=)/)) {
          const pair = cookie.split(';')[0];
          add(pair.slice(pair.indexOf('=') + 1).trim());
        }
      } else if (/^cookie$/i.test(name)) {
        for (const pair of value.split(';')) add(pair.slice(pair.indexOf('=') + 1).trim());
      } else if (/^authorization$/i.test(name)) { add(value); add(value.replace(/^\S+\s+/, '')); }
      else if (sensitive.test(name)) add(value);
    }
  }
  const pairs = (value: Record<string, string> = {}) => Object.entries(value).map(([name, value]) => ({ name, value }));
  async function flush() {
    const batch = [...values]; values.clear();
    for (let i = 0; i < batch.length; i += 128) await save(...batch.slice(i, i + 128));
  }
  function track(work: () => Promise<void>) {
    const task = work().catch(() => { failed = true; }).finally(() => pending.delete(task));
    pending.add(task);
    return task;
  }
  async function drain() {
    while (pending.size) await Promise.all([...pending]);
    await flush();
    if (failed) throw new Error('Private CI network observation failed');
  }
  function patch<T extends object, K extends keyof T>(object: T, key: K, value: T[K]) {
    const original = object[key]; object[key] = value;
    restore.push(() => { object[key] = original; });
  }
  async function response(value: APIResponse | Response) {
    const items = await value.headersArray();
    headers(items);
    const type = items.find(item => item.name.toLowerCase() === 'content-type')?.value ?? '';
    if (/json|text\/html/i.test(type)) body(await value.text());
    await flush();
  }
  function api(context: APIRequestContext) {
    if (seen.has(context)) return;
    seen.add(context);
    const fetch = context.fetch.bind(context);
    patch(context, 'fetch', async (target, options = {}) => {
      url(typeof target === 'string' ? target : target.url());
      if (typeof target !== 'string') { headers(pairs(target.headers())); body(target.postData()); }
      headers(pairs(options.headers)); body(options.data); fields(options.form); fields(options.multipart); fields(options.params);
      await flush(); // Generated credentials are recorded before a possibly failing write.
      const result = await fetch(target, options);
      await track(async () => {
        for (const cookie of (await context.storageState()).cookies) add(cookie.value);
        await response(result);
      });
      if (failed) throw new Error('Private CI API observation failed');
      return result;
    });
  }
  function observe(context: BrowserContext) {
    if (seen.has(context)) return;
    seen.add(context); api(context.request);
    const onRequest = (item: import('@playwright/test').Request) => {
      track(async () => { url(item.url()); headers(pairs(await item.allHeaders())); body(item.postData()); await flush(); });
    };
    const onResponse = (item: Response) => { track(() => response(item)); };
    context.on('request', onRequest); context.on('response', onResponse);
    restore.push(() => { context.off('request', onRequest); context.off('response', onResponse); });
    const close = context.close.bind(context);
    patch(context, 'close', async options => { await drain(); await close(options); });
  }
  const newContext = browser.newContext.bind(browser);
  patch(browser, 'newContext', async options => { const result = await newContext(options); observe(result); return result; });
  const newRequest = playwright.request.newContext.bind(playwright.request);
  patch(playwright.request, 'newContext', async options => {
    fields(options); headers(pairs(options?.extraHTTPHeaders)); await flush();
    const result = await newRequest(options); api(result); return result;
  });
  for (const context of browser.contexts()) observe(context);
  api(request);
  for (const [name, value] of Object.entries(process.env)) if (value && sensitive.test(name)) add(value);
  return async () => { try { await drain(); } finally { restore.reverse().forEach(undo => undo()); } };
}
