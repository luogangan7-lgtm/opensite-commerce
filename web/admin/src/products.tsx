/** Responsibility: tenant product, SKU and upload pages. Dependencies: CatalogAPI, native UI and router.
 * Callers: admin routes. Invariants: retain drafts on conflicts, version every catalog edit, never infer media readiness. */
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { Badge, Button, DataTable, EmptyState, FormField, Select, TextInput } from '@opensite/ui';
import { APIError, type CatalogAPI, type Product, type Variant, type UploadSession, type LinkProductMedia } from './api';
import { parseOptionValueIds, validateCode, validateMediaFile, validateUUID } from './validation';

type PageProps = { api: CatalogAPI; tenantId: string };
type Lifecycle = 'draft' | 'ready';
const codePattern = '[A-Za-z0-9._\\-]+';

function ErrorNotice({ error }: { error: unknown }) {
  if (!error) return null;
  return <p className="admin-error" role="alert">{error instanceof APIError ? error.userHint : 'The request failed. Please try again.'}</p>;
}

function LifecycleBadge({ value }: { value: string }) {
  return <Badge tone={value === 'ready' ? 'success' : value === 'archived' ? 'neutral' : 'warning'}>{value}</Badge>;
}

function LifecycleField({ id, value, onChange, disabled }: { id: string; value: Lifecycle; onChange: (value: Lifecycle) => void; disabled?: boolean }) {
  return <FormField id={id} label="Lifecycle" help="Ready marks catalog preparation; it does not publish a storefront.">
    <Select value={value} onChange={event => onChange(event.target.value as Lifecycle)} disabled={disabled}>
      <option value="draft">Draft</option><option value="ready">Ready</option>
    </Select>
  </FormField>;
}

function CodeField({ id, label, value, onChange, disabled }: { id: string; label: string; value: string; onChange: (value: string) => void; disabled?: boolean }) {
  return <FormField id={id} label={label} help="1–128 ASCII letters, digits, dots, underscores or hyphens. Codes are saved uppercase.">
    <TextInput value={value} onChange={event => onChange(event.target.value)} required maxLength={128} pattern={codePattern} disabled={disabled} autoComplete="off" />
  </FormField>;
}

export function ProductListPage({ api, tenantId }: PageProps) {
  const [cursors, setCursors] = useState<string[]>(['']);
  const [page, setPage] = useState<{ items: Product[]; next_cursor?: string }>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>();
  const [reload, setReload] = useState(0);
  const cursor = cursors.at(-1) ?? '';
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError(undefined);
    void api.listProducts({ cursor: cursor || undefined, pageSize: 20, signal: controller.signal })
      .then(value => { if (!controller.signal.aborted) setPage(value); })
      .catch(value => { if (!controller.signal.aborted) setError(value); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [api, cursor, reload]);
  return <div className="admin-page">
    <header className="admin-page-header"><div><h1>Products</h1><p className="admin-muted">Manage catalog codes, SKUs and product media.</p></div><Link to={`/t/${tenantId}/products/new`}>Create product</Link></header>
    <ErrorNotice error={error} />
    {!!error && <Button variant="secondary" onClick={() => setReload(value => value + 1)}>Retry products</Button>}
    {!loading && !error && page?.items.length === 0 ? <EmptyState headingLevel={2} title="No products on this page" description="Create a product to start preparing your catalog." /> :
      <DataTable caption="Product catalog" loading={loading} columns={[{ key: 'code', label: 'Product code' }, { key: 'status', label: 'Lifecycle' }, { key: 'version', label: 'Version' }]}
        rows={(error ? [] : page?.items ?? []).map(product => ({ id: product.id, code: <Link to={`/t/${tenantId}/products/${product.id}`}>{product.product_code}</Link>, status: <LifecycleBadge value={product.lifecycle_status} />, version: product.version }))} />}
    <nav className="admin-actions" aria-label="Product pagination">
      <Button variant="secondary" disabled={loading || cursors.length === 1} onClick={() => setCursors(value => value.slice(0, -1))}>Previous page</Button>
      <Button variant="secondary" disabled={loading || !!error || !page?.next_cursor} onClick={() => setCursors(value => [...value, page!.next_cursor!])}>Next page</Button>
    </nav>
  </div>;
}

export function ProductCreatePage({ api, tenantId }: PageProps) {
  const navigate = useNavigate();
  const [code, setCode] = useState('');
  const [lifecycle, setLifecycle] = useState<Lifecycle>('draft');
  const [created, setCreated] = useState<Product>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const controller = useRef(new AbortController());
  const requestKey = useRef({ body: '', key: '' });
  useEffect(() => { controller.current = new AbortController(); return () => controller.current.abort(); }, []);
  const conflict = error instanceof APIError && error.status === 412;
  async function reloadCreated() {
    if (!created) return;
    setBusy(true);
    try {
      const value = await api.getProduct(created.id, controller.current.signal);
      if (!controller.current.signal.aborted) { setCreated(value); setCode(value.product_code); setLifecycle(value.lifecycle_status === 'ready' ? 'ready' : 'draft'); setError(undefined); }
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const invalid = validateCode(code);
    if (invalid) { setError(new APIError(422, 'invalid_input', invalid)); return; }
    setBusy(true); setError(undefined);
    try {
      const signal = controller.current.signal;
      let product = created;
      if (!product) {
        if (requestKey.current.body !== code) requestKey.current = { body: code, key: crypto.randomUUID() };
        product = await api.createProduct({ product_code: code }, { idempotencyKey: requestKey.current.key, signal });
        if (signal.aborted) return;
        setCreated(product);
      }
      if (product.lifecycle_status !== lifecycle || product.product_code !== code.toUpperCase()) {
        product = await api.updateProduct(product.id, { product_code: code, lifecycle_status: lifecycle }, product.version, { signal });
      }
      if (!signal.aborted) navigate(`/t/${tenantId}/products/${product.id}`);
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  return <div className="admin-page">
    <header className="admin-page-header"><h1>Create product</h1><Link to={`/t/${tenantId}/products`}>Back to products</Link></header>
    <form className="admin-form" onSubmit={submit} aria-busy={busy}>
      <CodeField id="new-product-code" label="Product code" value={code} onChange={setCode} disabled={busy} />
      <LifecycleField id="new-product-lifecycle" value={lifecycle} onChange={setLifecycle} disabled={busy} />
      <ErrorNotice error={error} />
      {created && <p role="status">Product created as draft. Retry to finish changes, or <Link to={`/t/${tenantId}/products/${created.id}`}>open the product</Link>.</p>}
      {conflict && <p>Unsaved values are retained. Reloading replaces them with current server values.</p>}
      <div className="admin-actions"><Button type="submit" loading={busy} disabled={conflict || created?.lifecycle_status === 'archived'}>{created ? 'Finish product changes' : 'Create product'}</Button>
        {created && <Button variant="secondary" disabled={busy} onClick={() => void reloadCreated()}>Reload created product from server</Button>}</div>
    </form>
  </div>;
}

export function ProductEditPage(props: PageProps) {
  const { productId } = useParams();
  if (!productId || !validateUUID(productId)) return <EmptyState title="Invalid product address" description="Choose a product from the catalog." action={<Link to={`/t/${props.tenantId}/products`}>Back to products</Link>} />;
  return <ProductEditor key={`${props.tenantId}:${productId}`} {...props} productId={productId} />;
}

function ProductEditor({ api, tenantId, productId }: PageProps & { productId: string }) {
  const [product, setProduct] = useState<Product>();
  const [code, setCode] = useState('');
  const [lifecycle, setLifecycle] = useState<Lifecycle>('draft');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [reload, setReload] = useState(0);
  const controller = useRef(new AbortController());
  useEffect(() => { controller.current = new AbortController(); return () => controller.current.abort(); }, []);
  useEffect(() => {
    const read = new AbortController();
    setLoading(true); setError(undefined);
    void api.getProduct(productId, read.signal).then(value => {
      if (read.signal.aborted) return;
      setProduct(value); setCode(value.product_code); setLifecycle(value.lifecycle_status === 'ready' ? 'ready' : 'draft');
    }).catch(value => { if (!read.signal.aborted) setError(value); }).finally(() => { if (!read.signal.aborted) setLoading(false); });
    return () => read.abort();
  }, [api, productId, reload]);
  const conflict = error instanceof APIError && error.status === 412;
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!product) return;
    const invalid = validateCode(code);
    if (invalid) { setError(new APIError(422, 'invalid_input', invalid)); return; }
    setBusy(true); setError(undefined);
    try {
      const value = await api.updateProduct(productId, { product_code: code, lifecycle_status: lifecycle }, product.version, { signal: controller.current.signal });
      if (!controller.current.signal.aborted) { setProduct(value); setCode(value.product_code); }
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  return <div className="admin-page">
    <header className="admin-page-header"><div><h1>Edit product</h1>{product && <p className="admin-muted">{product.product_code} · version {product.version} <LifecycleBadge value={product.lifecycle_status} /></p>}</div><Link to={`/t/${tenantId}/products`}>Back to products</Link></header>
    {loading && <p role="status">Loading product…</p>}
    {product ? <>
      <section className="admin-panel" aria-labelledby="product-details-heading"><h2 id="product-details-heading">Product details</h2>
        <form className="admin-form" onSubmit={save} aria-busy={busy}>
          <CodeField id="product-code" label="Product code" value={code} onChange={setCode} disabled={busy || loading || product.lifecycle_status === 'archived'} />
          <LifecycleField id="product-lifecycle" value={lifecycle} onChange={setLifecycle} disabled={busy || loading || product.lifecycle_status === 'archived'} />
          <ErrorNotice error={error} />
          {conflict && <p>Your unsaved values are retained. Reloading replaces them with the current server values.</p>}
          <div className="admin-actions"><Button type="submit" loading={busy} disabled={loading || conflict || product.lifecycle_status === 'archived'}>Save product</Button>
            <Button variant="secondary" disabled={busy || loading} onClick={() => setReload(value => value + 1)}>Reload product from server</Button></div>
        </form>
      </section>
      <VariantSection api={api} productId={productId} archived={product.lifecycle_status === 'archived'} />
      <MediaSection api={api} productId={productId} archived={product.lifecycle_status === 'archived'} />
    </> : !loading && <><ErrorNotice error={error} /><Button variant="secondary" onClick={() => setReload(value => value + 1)}>Retry product</Button></>}
  </div>;
}

function VariantSection({ api, productId, archived }: { api: CatalogAPI; productId: string; archived: boolean }) {
  const [variants, setVariants] = useState<Variant[]>([]);
  const [cursors, setCursors] = useState<string[]>(['']);
  const [nextCursor, setNextCursor] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [reload, setReload] = useState(0);
  const [listError, setListError] = useState<unknown>();
  const [editing, setEditing] = useState<Variant>();
  const [sku, setSku] = useState('');
  const [optionIds, setOptionIds] = useState('');
  const [lifecycle, setLifecycle] = useState<Lifecycle>('draft');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const controller = useRef(new AbortController());
  const requestKey = useRef({ body: '', key: '' });
  const cursor = cursors.at(-1) ?? '';
  useEffect(() => { controller.current = new AbortController(); return () => controller.current.abort(); }, []);
  useEffect(() => {
    const read = new AbortController();
    setLoading(true); setListError(undefined);
    void api.listVariants(productId, { cursor: cursor || undefined, pageSize: 20, signal: read.signal }).then(value => {
      if (!read.signal.aborted) { setVariants(value.items); setNextCursor(value.next_cursor); }
    }).catch(value => { if (!read.signal.aborted) setListError(value); }).finally(() => { if (!read.signal.aborted) setLoading(false); });
    return () => read.abort();
  }, [api, productId, cursor, reload]);
  const conflict = error instanceof APIError && error.status === 412;
  function select(variant?: Variant) {
    setEditing(variant); setSku(variant?.sku ?? ''); setOptionIds(variant?.option_value_ids.join(', ') ?? '');
    setLifecycle(variant?.lifecycle_status === 'ready' ? 'ready' : 'draft'); setError(undefined);
  }
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true); setError(undefined);
    try {
      const invalid = validateCode(sku);
      if (invalid) throw new APIError(422, 'invalid_input', invalid);
      const signal = controller.current.signal;
      let variant = editing;
      if (!variant) {
        const body = { sku, option_value_ids: parseOptionValueIds(optionIds) };
        const fingerprint = JSON.stringify(body);
        if (requestKey.current.body !== fingerprint) requestKey.current = { body: fingerprint, key: crypto.randomUUID() };
        variant = await api.createVariant(productId, body, { idempotencyKey: requestKey.current.key, signal });
        if (signal.aborted) return;
        setEditing(variant);
      }
      if (editing || variant.lifecycle_status !== lifecycle) variant = await api.updateVariant(productId, variant.id, { sku, lifecycle_status: lifecycle }, variant.version, { signal });
      if (!signal.aborted) { select(); requestKey.current = { body: '', key: '' }; setReload(value => value + 1); }
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  async function reloadVariant() {
    if (!editing) return;
    setBusy(true);
    try { const value = await api.getVariant(productId, editing.id, controller.current.signal); if (!controller.current.signal.aborted) select(value); }
    catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  async function archive(variant: Variant) {
    setBusy(true); setError(undefined);
    try {
      await api.archiveVariant(productId, variant.id, variant.version, { signal: controller.current.signal });
      if (!controller.current.signal.aborted) { if (editing?.id === variant.id) select(); setReload(value => value + 1); }
    } catch (value) { if (!controller.current.signal.aborted) setListError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  return <section className="admin-panel" aria-labelledby="sku-heading"><h2 id="sku-heading">SKUs</h2>
    <ErrorNotice error={listError} />
    {!!listError && <Button variant="secondary" onClick={() => setReload(value => value + 1)}>Reload SKUs from server</Button>}
    <DataTable caption="Product SKUs" loading={loading} emptyLabel="No SKUs on this page." columns={[{ key: 'sku', label: 'SKU' }, { key: 'status', label: 'Lifecycle' }, { key: 'options', label: 'Option values' }, { key: 'actions', label: 'Actions' }]}
      rows={(listError ? [] : variants).map(variant => ({ id: variant.id, sku: variant.sku, status: <LifecycleBadge value={variant.lifecycle_status} />, options: variant.option_value_ids.length ? variant.option_value_ids.join(', ') : 'Default SKU', actions: <div className="admin-actions"><Button variant="secondary" size="sm" disabled={busy} onClick={() => select(variant)}>Edit {variant.sku}</Button><Button variant="danger" size="sm" disabled={busy || archived || variant.lifecycle_status === 'archived'} onClick={() => void archive(variant)}>Archive {variant.sku}</Button></div> }))} />
    <nav className="admin-actions" aria-label="SKU pagination"><Button variant="secondary" disabled={loading || cursors.length === 1} onClick={() => setCursors(value => value.slice(0, -1))}>Previous SKUs</Button><Button variant="secondary" disabled={loading || !!listError || !nextCursor} onClick={() => setCursors(value => [...value, nextCursor!])}>Next SKUs</Button></nav>
    <h3>{editing ? `Edit SKU ${editing.sku}` : 'Create SKU'}</h3>
    <form className="admin-form" onSubmit={save} aria-busy={busy}>
      <CodeField id="sku-code" label="SKU code" value={sku} onChange={setSku} disabled={busy || archived || editing?.lifecycle_status === 'archived'} />
      <FormField id="sku-options" label="Option value UUIDs" help={editing ? 'Option assignments cannot be changed by the current edit API.' : 'Leave blank for a default SKU; otherwise enter comma-separated option UUIDs.'}>
        <TextInput value={optionIds} onChange={event => setOptionIds(event.target.value)} disabled={busy || archived || !!editing} autoComplete="off" />
      </FormField>
      <LifecycleField id="sku-lifecycle" value={lifecycle} onChange={setLifecycle} disabled={busy || archived || editing?.lifecycle_status === 'archived'} />
      <ErrorNotice error={error} />
      {conflict && <p>Unsaved SKU values are retained. Reloading replaces them with server values.</p>}
      <div className="admin-actions"><Button type="submit" loading={busy} disabled={conflict || archived || editing?.lifecycle_status === 'archived'}>{editing ? 'Save SKU' : 'Create SKU'}</Button>
        {editing && <><Button variant="secondary" disabled={busy} onClick={() => void reloadVariant()}>Reload SKU from server</Button><Button variant="ghost" disabled={busy} onClick={() => select()}>Cancel SKU edit</Button></>}</div>
    </form>
  </section>;
}

function MediaSection({ api, productId, archived }: { api: CatalogAPI; productId: string; archived: boolean }) {
  const [file, setFile] = useState<File>();
  const [upload, setUpload] = useState<UploadSession>();
  const [mediaId, setMediaId] = useState('');
  const [role, setRole] = useState<LinkProductMedia['role']>('main');
  const [sortOrder, setSortOrder] = useState('0');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [linked, setLinked] = useState(false);
  const controller = useRef(new AbortController());
  useEffect(() => { controller.current = new AbortController(); return () => controller.current.abort(); }, []);
  async function uploadFile(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!file) return;
    const invalid = validateMediaFile(file);
    if (invalid) { setError(new APIError(422, 'invalid_input', invalid)); return; }
    setBusy(true); setError(undefined); setUpload(undefined); setLinked(false);
    try {
      const value = await api.uploadMedia(file, { signal: controller.current.signal });
      if (!controller.current.signal.aborted) { setUpload(value); setMediaId(value.media_id ?? ''); }
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  async function link(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!validateUUID(mediaId)) { setError(new APIError(422, 'invalid_input', 'Enter a valid media UUID.')); return; }
    setBusy(true); setError(undefined); setLinked(false);
    try {
      const signal = controller.current.signal;
      // The media API has no version precondition. Read current archive state without replacing the product draft.
      const current = await api.getProduct(productId, signal);
      if (current.lifecycle_status === 'archived') throw new APIError(422, 'invalid_input', 'This product is archived. Reload the product before making changes.');
      await api.linkProductMedia(productId, { media_id: mediaId, role, sort_order: Number(sortOrder) }, { signal });
      if (!signal.aborted) setLinked(true);
    } catch (value) { if (!controller.current.signal.aborted) setError(value); }
    finally { if (!controller.current.signal.aborted) setBusy(false); }
  }
  return <section className="admin-panel" aria-labelledby="media-heading"><h2 id="media-heading">Product media</h2>
    <p className="admin-muted">Upload a file, then attach its media ID. Stored or processing media is not proof that processing has finished.</p>
    <form className="admin-form" onSubmit={uploadFile} aria-busy={busy}>
      <FormField id="media-file" label="Media file" help="JPEG, PNG, WebP or AVIF, up to 10 MiB. Upload includes a checksum verification."><TextInput type="file" accept="image/jpeg,image/png,image/webp,image/avif" required disabled={busy || archived} onChange={event => { setFile(event.target.files?.[0]); setUpload(undefined); setMediaId(''); setLinked(false); }} /></FormField>
      <Button type="submit" loading={busy} disabled={!file || archived}>Upload media</Button>
    </form>
    {upload && <p role="status">Upload session: <Badge tone={upload.status === 'failed' ? 'danger' : upload.status === 'ready' ? 'success' : 'info'}>{upload.status}</Badge> {upload.media_id ? `Media ID: ${upload.media_id}` : 'No media ID is available yet.'}</p>}
    {upload?.status === 'failed' && <p className="admin-error" role="alert">Media verification or processing failed. Choose a file and upload it again.</p>}
    <form className="admin-form" onSubmit={link} aria-busy={busy}>
      <FormField id="media-id" label="Media ID"><TextInput value={mediaId} required disabled={busy || archived} onChange={event => { setMediaId(event.target.value); setLinked(false); }} /></FormField>
      <FormField id="media-role" label="Media role"><Select value={role} disabled={busy || archived} onChange={event => { setRole(event.target.value as LinkProductMedia['role']); setLinked(false); }}><option value="main">Main</option><option value="gallery">Gallery</option><option value="detail">Detail</option><option value="video">Video</option><option value="size_chart">Size chart</option></Select></FormField>
      <FormField id="media-order" label="Sort order"><TextInput type="number" min={0} max={2147483647} step={1} required value={sortOrder} disabled={busy || archived} onChange={event => { setSortOrder(event.target.value); setLinked(false); }} /></FormField>
      <ErrorNotice error={error} />
      {linked && <p role="status">Media attached to this product.</p>}
      <Button type="submit" loading={busy} disabled={!mediaId || archived || upload?.status === 'failed'}>Attach media</Button>
    </form>
  </section>;
}
