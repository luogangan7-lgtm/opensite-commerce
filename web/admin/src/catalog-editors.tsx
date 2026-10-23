/** Responsibility: staff price and locale publication forms. Depends on generated catalog API and existing UI.
 * Called by product/SKU pages. Invariants: opaque ETags, no automatic conflict retry, exact minor units,
 * server-owned permissions/currency/time; cancelling or changing identity invalidates the loaded draft. */
import { useEffect, useRef, useState } from 'react';
import { t } from '@opensite/component-contracts';
import { Button, FormField, Select, TextInput } from '@opensite/ui';
import { APIError, priceIssue, type CatalogAPI, type Content, type Listing, type PriceList, type Tagged } from './api';

function Notice({ error }: { error: unknown }) {
  return error ? <p className="admin-error" role="alert">{error instanceof APIError ? error.userHint : 'The request failed. Please try again.'}</p> : null;
}
function useAction() {
  const abort = useRef(new AbortController());
  const [busy, setBusy] = useState(false), [error, setError] = useState<unknown>();
  const [conflict, setConflict] = useState(false);
  useEffect(() => { abort.current = new AbortController(); return () => abort.current.abort(); }, []);
  async function run<T>(operation: (signal: AbortSignal) => Promise<T>, saved: (value: T) => void) {
    if (busy) return;
    const signal = abort.current.signal;
    setBusy(true); setError(undefined);
    try { const value = await operation(signal); if (!signal.aborted) saved(value); }
    catch (value) { if (!signal.aborted) { setError(value); if (value instanceof APIError && value.status === 412) setConflict(true); } }
    finally { if (!signal.aborted) setBusy(false); }
  }
  return { busy, error, run, conflict, reloaded: () => setConflict(false) };
}

export function PriceEditor({ api, variantId, archived }: { api: CatalogAPI; variantId: string; archived: boolean }) {
  const action = useAction();
  const [lists, setLists] = useState<PriceList[]>([]), [next, setNext] = useState<string>();
  const [selected, setSelected] = useState(''), [list, setList] = useState<PriceList>();
  const [minor, setMinor] = useState(''), [saved, setSaved] = useState(false);
  const [denied, setDenied] = useState(false);
  useEffect(() => {
    const abort = new AbortController();
    void api.priceLists(undefined, abort.signal).then(value => { if (!abort.signal.aborted) { setLists(value.items); setNext(value.next_cursor); } })
      .catch(error => { if (!abort.signal.aborted && error instanceof APIError && error.status === 403) setDenied(true); });
    return () => abort.abort();
  }, [api]);
  if (denied) return null;
  function load(id: string) {
    setSelected(id); setList(undefined); setMinor(''); setSaved(false);
    if (id) void action.run(signal => api.priceList(id, signal), value => { setList(value); action.reloaded(); });
  }
  return <section className="admin-panel" aria-labelledby="price-heading"><h3 id="price-heading">{t('catalog.variant-price')}</h3>
    <p className="admin-muted">{t('catalog.base-tier-minimum-quantity-1-currency')}</p>
    <form className="admin-form" aria-busy={action.busy} onSubmit={event => { event.preventDefault(); if (list) void action.run(signal => api.savePrice(list, variantId, minor, signal), value => { setList(value); setSaved(true); }); }}>
      <FormField id="price-list" label={t('catalog.price-list')}><Select value={selected} disabled={action.busy || archived} onChange={event => load(event.target.value)}>
        <option value="">{t('catalog.choose-a-price-list')}</option>{lists.map(value => <option key={value.id} value={value.id}>{value.currency} · {value.status} · {value.id}</option>)}
      </Select></FormField>
      <div className="admin-actions"><Button variant="secondary" disabled={action.busy} onClick={() => void action.run(signal => api.priceLists(undefined, signal), value => { setLists(value.items); setNext(value.next_cursor); })}>{t('catalog.reload-price-lists')}</Button>
        {next && <Button variant="secondary" disabled={action.busy} onClick={() => void action.run(signal => api.priceLists(next, signal), value => { setLists(old => [...old, ...value.items.filter(item => !old.some(existing => existing.id === item.id))]); setNext(value.next_cursor); })}>{t('catalog.more-price-lists')}</Button>}</div>
      <FormField id="price-minor" label={t('catalog.price-in-minor-units')} help={list ? `${list.currency} · Whole minor units; for example 3456 means 34.56 for USD.` : 'Choose a list to load its currency and current version.'}>
        <TextInput inputMode="numeric" required pattern="(0|[1-9][0-9]{0,18})" maxLength={19} value={minor} disabled={!list || action.busy || archived} onChange={event => { setMinor(event.target.value); setSaved(false); event.target.setCustomValidity(priceIssue(event.target.value)); }} />
      </FormField>
      <Notice error={action.error} />
      {action.conflict && <p>{t('catalog.your-price-draft-is-retained-reload')}</p>}
      {saved && <p role="status">Price saved: {list?.currency} {minor} minor units.</p>}
      <div className="admin-actions"><Button type="submit" loading={action.busy} disabled={!list || archived || action.conflict || list.status === 'archived'}>{t('catalog.save-price')}</Button>
        <Button variant="secondary" disabled={!selected || action.busy} onClick={() => load(selected)}>{t('catalog.reload-price-from-server')}</Button></div>
    </form>
  </section>;
}

export function PublicationEditor({ api, productId, archived }: { api: CatalogAPI; productId: string; archived: boolean }) {
  const [permission, setPermission] = useState<'loading' | 'allowed' | 'denied' | 'error'>('loading');
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    const abort = new AbortController();
    void api.priceLists(undefined, abort.signal).then(() => { if (!abort.signal.aborted) setPermission('allowed'); })
      .catch(error => { if (!abort.signal.aborted) setPermission(error instanceof APIError && error.status === 403 ? 'denied' : 'error'); });
    return () => abort.abort();
  }, [api, attempt]);
  if (permission === 'loading') return <p role="status">{t('catalog.checking-catalog-editing-access')}</p>;
  if (permission === 'denied') return <p role="status">{t('catalog.catalog-editing-is-unavailable-for-this')}</p>;
  if (permission === 'error') return <p role="alert">Unable to check catalog editing access. <Button variant="secondary" onClick={() => setAttempt(value => value + 1)}>{t('catalog.retry-editing-access')}</Button></p>;
  return <LocaleEditor api={api} productId={productId} archived={archived} />;
}
function LocaleEditor({ api, productId, archived }: { api: CatalogAPI; productId: string; archived: boolean }) {
  const contentAction = useAction(), listingAction = useAction();
  const [locale, setLocale] = useState('en'), [site, setSite] = useState('');
  const [content, setContent] = useState<Tagged<Content>>(), [listing, setListing] = useState<Tagged<Listing>>();
  const [title, setTitle] = useState(''), [body, setBody] = useState('[]'), [seo, setSEO] = useState('{}');
  const [slug, setSlug] = useState(''), [listed, setListed] = useState(false);
  const [contentSaved, setContentSaved] = useState(false), [listingSaved, setListingSaved] = useState(false);
  const busy = contentAction.busy || listingAction.busy;
  return <section className="admin-panel" aria-labelledby="publication-heading"><h2 id="publication-heading">{t('catalog.content-and-listing')}</h2>
    <p className="admin-muted">{t('catalog.edit-one-locale-at-a-time')}</p>
    <div className="admin-form"><FormField id="content-locale" label={t('catalog.locale')}><TextInput value={locale} maxLength={64} disabled={busy} onChange={event => { setLocale(event.target.value); setContent(undefined); setListing(undefined); setTitle(''); setBody('[]'); setSEO('{}'); setContentSaved(false); setListingSaved(false); }} /></FormField>
      <Button variant="secondary" disabled={busy} onClick={() => { setContentSaved(false); void contentAction.run(signal => api.content(productId, locale, signal), value => { contentAction.reloaded(); setContent(value); setTitle(value.data.title); setBody(JSON.stringify(value.data.body_blocks, null, 2)); setSEO(JSON.stringify(value.data.seo, null, 2)); setListing(undefined); }); }}>{t('catalog.load-content')}</Button></div>
    <form className="admin-form" aria-busy={contentAction.busy} onSubmit={event => { event.preventDefault(); if (!content) return; setContentSaved(false); void contentAction.run(async signal => {
      let bodyBlocks: unknown, seoValue: unknown;
      try { bodyBlocks = JSON.parse(body); seoValue = JSON.parse(seo); } catch { throw new APIError(422, 'invalid_input', 'Body and SEO must be valid JSON.'); }
      if (!Array.isArray(bodyBlocks) || bodyBlocks.length > 128 || bodyBlocks.some(value => !value || typeof value !== 'object' || Array.isArray(value)) || !seoValue || typeof seoValue !== 'object' || Array.isArray(seoValue)) throw new APIError(422, 'invalid_input', 'Body must be an array of objects; SEO must be an object.');
      return api.saveContent(productId, locale, content.etag, { title, body_blocks: bodyBlocks, seo: seoValue as Record<string, unknown> }, signal);
    }, value => { setContent(value); setContentSaved(true); setListing(undefined); setListingSaved(false); }); }}>
      <FormField id="content-title" label={t('catalog.title')}><TextInput required maxLength={4096} value={title} disabled={!content || busy || archived} onChange={event => { setTitle(event.target.value); setContentSaved(false); }} /></FormField>
      <FormField id="content-body" label={t('catalog.body-blocks-json')} help={t('catalog.array-of-content-blocks-up-to')}><TextInput required maxLength={131072} value={body} disabled={!content || busy || archived} onChange={event => { setBody(event.target.value); setContentSaved(false); }} /></FormField>
      <FormField id="content-seo" label={t('catalog.seo-json')} help={t('catalog.seo-object-such-as-a-description')}><TextInput required maxLength={32768} value={seo} disabled={!content || busy || archived} onChange={event => { setSEO(event.target.value); setContentSaved(false); }} /></FormField>
      <Notice error={contentAction.error} />
      {contentAction.conflict && <p>{t('catalog.your-content-draft-is-retained-load')}</p>}
      {contentSaved && <p role="status">{t('catalog.content-saved')}</p>}
      <Button type="submit" loading={contentAction.busy} disabled={!content || archived || contentAction.conflict || busy}>{t('catalog.save-content')}</Button>
    </form>
    <div className="admin-form"><FormField id="listing-site" label={t('catalog.site-id')}><TextInput value={site} disabled={busy} onChange={event => { setSite(event.target.value); setListing(undefined); setListingSaved(false); }} /></FormField>
      <Button variant="secondary" disabled={busy || !content?.data.revision} onClick={() => { setListingSaved(false); void listingAction.run(signal => api.listing(productId, site, locale, signal), value => { listingAction.reloaded(); setListing(value); setSlug(value.data.slug); setListed(value.data.listed); }); }}>{t('catalog.load-listing')}</Button></div>
    <form className="admin-form" aria-busy={listingAction.busy} onSubmit={event => { event.preventDefault(); if (!listing || !content) return; setListingSaved(false); void listingAction.run(signal => api.saveListing(productId, site, locale, listing.etag, { slug, listed, content_revision: content.data.revision }, signal), value => { setListing(value); setListingSaved(true); }); }}>
      <FormField id="listing-slug" label={t('catalog.slug')}><TextInput required maxLength={255} pattern="[a-z0-9]+(-[a-z0-9]+)*" value={slug} disabled={!listing || busy || archived} onChange={event => { setSlug(event.target.value); setListingSaved(false); }} /></FormField>
      <FormField id="listing-state" label={t('catalog.listing-state')}><Select value={listed ? 'listed' : 'unlisted'} disabled={!listing || busy || archived} onChange={event => { setListed(event.target.value === 'listed'); setListingSaved(false); }}><option value="unlisted">{t('catalog.unlisted')}</option><option value="listed">{t('catalog.listed')}</option></Select></FormField>
      <Notice error={listingAction.error} />
      {listingAction.conflict && <p>{t('catalog.your-listing-draft-is-retained-load')}</p>}
      {listingSaved && <p role="status">{t('catalog.listing-saved')}</p>}
      <Button type="submit" loading={listingAction.busy} disabled={!listing || !content?.data.revision || archived || listingAction.conflict || busy}>{t('catalog.save-listing')}</Button>
    </form>
  </section>;
}
