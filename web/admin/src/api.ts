/** Cookie-only admin transport; depends on generated main OpenAPI schema and native
 * fetch/WebCrypto. Called by session provider and catalog forms; CSRF lives in memory,
 * redirects are rejected, tenant routing never substitutes for server authorization. */
import type { components } from '../../../packages/api-client/src/schema';
import { validateCode, validateMediaFile, validateUUID } from './validation';
type Schemas = components['schemas'];
export type Product = Schemas['Product'];
export type Variant = Schemas['Variant'];
export type CreateProduct = Schemas['CreateProduct'];
export type UpdateProduct = Schemas['UpdateProduct'];
export type CreateVariant = Schemas['CreateVariant'];
export type UpdateVariant = Schemas['UpdateVariant'];
export type ProductPage = Schemas['ProductPage'];
export type VariantList = Schemas['VariantList'];
export type UploadSession = Schemas['UploadSession'];
export type LinkProductMedia = Schemas['LinkProductMedia'];
export type ProductMedia = Schemas['ProductMedia'];
// PR43 df68d7c identity contract (c02b-login-v3); replace these DTOs when main
// generated schema includes the four actual login operations, not planned sessions.
export type LoginChallenge = { csrf_token: string };
export type AdminLogin = { email: string; password: string };
export type AdminSessionView = { user_id: string; session_id: string; expires_at: string; csrf_token: string };
export type MutationOptions = { idempotencyKey?: string; signal?: AbortSignal };
export type PageOptions = { pageSize?: number; cursor?: string; signal?: AbortSignal };

const hints: Record<number, string> = {
  400: 'The request is invalid. Check your input.', 401: 'Your session has expired. Sign in again.',
  403: 'This action is not allowed for your current session.', 404: 'The requested resource is unavailable.',
  409: 'This action conflicts with the current resource state.', 412: 'This resource changed. Reload it before saving.',
  413: 'The upload is too large.', 415: 'This file type is not supported.', 422: 'Check the submitted values.',
  428: 'Reload the resource before saving.', 429: 'Too many requests. Wait and try again.', 503: 'The service is temporarily unavailable. Try again later.',
};
const knownCodes = new Set(['version_conflict', 'resource_archived', 'duplicate_code', 'idempotency_in_progress', 'idempotency_key_reused', 'invalid_catalog', 'invalid_media', 'precondition_required']);
export class APIError extends Error {
  readonly userHint: string;
  constructor(readonly status: number, readonly code = `http_${status}`, hint = hints[status] ?? 'The request failed. Try again later.') {
    super(hint); this.name = 'APIError'; this.userHint = hint;
  }
}
function id(value: string): string { if (!validateUUID(value)) throw new APIError(400, 'invalid_id'); return value; }
function versionHeader(version: number): string {
  if (!Number.isSafeInteger(version) || version < 1) throw new APIError(400, 'invalid_version');
  return `"${version}"`;
}
function csrf(value: unknown): string {
  if (typeof value !== 'string' || !value || value.length > 4096 || /[\r\n]/.test(value)) throw new APIError(502, 'invalid_response');
  return value;
}
export type RequestOptions = MutationOptions & { version?: number; raw?: boolean; anonymous?: boolean; responseRaw?: boolean };
export class AdminAPI {
  private csrfToken = '';
  private authGeneration = 0;
  private expiredNotified = false;
  private readonly fetcher: typeof fetch;
  private readonly onSessionExpired?: () => void;
  constructor(options: { fetch?: typeof fetch; onSessionExpired?: () => void } = {}) {
    this.fetcher = options.fetch ?? globalThis.fetch.bind(globalThis); this.onSessionExpired = options.onSessionExpired;
  }
  clearSession(): void { this.authGeneration++; this.csrfToken = ''; }
  private requireCurrent(generation: number, signal?: AbortSignal): void {
    signal?.throwIfAborted();
    if (generation !== this.authGeneration) throw new DOMException('Authentication state changed.', 'AbortError');
  }
  async request<T>(path: string, method = 'GET', body?: unknown, options: RequestOptions = {}): Promise<T> {
    const generation = this.authGeneration;
    options.signal?.throwIfAborted();
    if (!path.startsWith('/api/v1/') || /[\\\r\n#]/.test(path)) throw new APIError(400, 'invalid_path');
    const mutation = method !== 'GET';
    const headers = new Headers({ Accept: 'application/json, application/problem+json' });
    if (mutation) {
      if (!this.csrfToken) throw new APIError(401, 'missing_csrf');
      headers.set('X-CSRF-Token', this.csrfToken);
      if (!options.anonymous && path !== '/api/v1/auth/logout') {
        const key = options.idempotencyKey ?? crypto.randomUUID();
        if (!key || key.length > 128 || /[^\x21-\x7e]/.test(key)) throw new APIError(400, 'invalid_idempotency_key');
        headers.set('Idempotency-Key', key);
      }
    }
    if (options.version !== undefined) headers.set('If-Match', versionHeader(options.version));
    if (body !== undefined) headers.set('Content-Type', options.raw ? 'application/octet-stream' : 'application/json');
    const timeout = AbortSignal.timeout(15000);
    const signal = options.signal ? AbortSignal.any([options.signal, timeout]) : timeout;
    const response = await this.fetcher(path, { method, headers, credentials: 'same-origin', redirect: 'error', cache: 'no-store', signal, body: body === undefined ? undefined : options.raw ? body as BodyInit : JSON.stringify(body) });
    // Old requests must not expire a newer session or return its predecessor's data.
    this.requireCurrent(generation, signal);
    if (!response.ok) {
      if (response.status === 401 && !options.anonymous) {
        this.clearSession();
        if (!this.expiredNotified) { this.expiredNotified = true; this.onSessionExpired?.(); }
      }
      let code: string | undefined;
      if (response.headers.get('Content-Type')?.split(';')[0].trim() === 'application/problem+json') {
        try { const problem: unknown = await response.json(); if (problem && typeof problem === 'object' && 'code' in problem && typeof problem.code === 'string' && knownCodes.has(problem.code)) code = problem.code; } catch { /* status still determines safe hint */ }
      }
      throw new APIError(response.status, code, options.anonymous && response.status === 401 ? 'Sign-in failed. Check your credentials and try again.' : undefined);
    }
    if (options.responseRaw) return response as T;
    if (response.status === 204) return undefined as T;
    let data: T;
    try { data = await response.json() as T; } catch { this.requireCurrent(generation, signal); throw new APIError(502, 'invalid_response'); }
    this.requireCurrent(generation, signal); return data;
  }
  async loginChallenge(signal?: AbortSignal): Promise<LoginChallenge> {
    const generation = this.authGeneration;
    const challenge = await this.request<LoginChallenge>('/api/v1/auth/login', 'GET', undefined, { anonymous: true, signal });
    this.requireCurrent(generation, signal);
    this.csrfToken = csrf(challenge.csrf_token); return challenge;
  }
  async login(credentials: AdminLogin, signal?: AbortSignal): Promise<AdminSessionView> {
    this.clearSession();
    const generation = this.authGeneration;
    if (!credentials.email || credentials.email.length > 254 || !credentials.password || credentials.password.length > 1024) throw new APIError(400, 'invalid_login', 'Enter your email and password.');
    try {
      await this.loginChallenge(signal);
      await this.request<AdminSessionView>('/api/v1/auth/login', 'POST', { email: credentials.email, password: credentials.password }, { anonymous: true, signal });
      return await this.session(signal);
    } catch (error) { if (generation === this.authGeneration) this.clearSession(); throw error; }
  }
  async session(signal?: AbortSignal): Promise<AdminSessionView> {
    const generation = this.authGeneration;
    const view = await this.request<AdminSessionView>('/api/v1/auth/session', 'GET', undefined, { signal });
    this.requireCurrent(generation, signal);
    if (!view || !validateUUID(view.user_id) || !validateUUID(view.session_id) || !Number.isFinite(Date.parse(view.expires_at))) throw new APIError(502, 'invalid_response');
    this.csrfToken = csrf(view.csrf_token); this.expiredNotified = false; return view;
  }
  async logout(signal?: AbortSignal): Promise<void> {
    const generation = this.authGeneration;
    await this.request<void>('/api/v1/auth/logout', 'POST', undefined, { signal });
    this.requireCurrent(generation, signal);
    this.clearSession();
  }
}
export class CatalogAPI {
  private readonly base: string;
  constructor(private readonly client: AdminAPI, tenantId: string) { this.base = `/api/v1/t/${id(tenantId)}`; }
  private productPath(productId: string): string { return `${this.base}/products/${id(productId)}`; }
  private variantPath(productId: string, variantId: string): string { return `${this.productPath(productId)}/variants/${id(variantId)}`; }
  private page(options: PageOptions): string {
    const query = new URLSearchParams();
    if (options.pageSize !== undefined) { if (!Number.isInteger(options.pageSize) || options.pageSize < 1 || options.pageSize > 100) throw new APIError(400, 'invalid_page_size'); query.set('page_size', String(options.pageSize)); }
    if (options.cursor) query.set('cursor', options.cursor);
    return query.size ? `?${query}` : '';
  }
  listProducts(options: PageOptions = {}): Promise<ProductPage> { return this.client.request(`${this.base}/products${this.page(options)}`, 'GET', undefined, options); }
  getProduct(productId: string, signal?: AbortSignal): Promise<Product> { return this.client.request(this.productPath(productId), 'GET', undefined, { signal }); }
  async createProduct(body: CreateProduct, options: MutationOptions = {}): Promise<Product> {
    if (validateCode(body.product_code)) throw new APIError(422, 'invalid_catalog');
    return this.client.request(`${this.base}/products`, 'POST', body, options);
  }
  async updateProduct(productId: string, body: UpdateProduct, version: number, options: MutationOptions = {}): Promise<Product> { return this.client.request(this.productPath(productId), 'PATCH', body, { ...options, version }); }
  async archiveProduct(productId: string, version: number, options: MutationOptions = {}): Promise<void> { return this.client.request(this.productPath(productId), 'DELETE', undefined, { ...options, version }); }
  listVariants(productId: string, options: PageOptions = {}): Promise<VariantList> { return this.client.request(`${this.productPath(productId)}/variants${this.page(options)}`, 'GET', undefined, options); }
  getVariant(productId: string, variantId: string, signal?: AbortSignal): Promise<Variant> { return this.client.request(this.variantPath(productId, variantId), 'GET', undefined, { signal }); }
  async createVariant(productId: string, body: CreateVariant, options: MutationOptions = {}): Promise<Variant> { return this.client.request(`${this.productPath(productId)}/variants`, 'POST', body, options); }
  async updateVariant(productId: string, variantId: string, body: UpdateVariant, version: number, options: MutationOptions = {}): Promise<Variant> { return this.client.request(this.variantPath(productId, variantId), 'PATCH', body, { ...options, version }); }
  async archiveVariant(productId: string, variantId: string, version: number, options: MutationOptions = {}): Promise<void> { return this.client.request(this.variantPath(productId, variantId), 'DELETE', undefined, { ...options, version }); }
  async uploadMedia(file: File, options: MutationOptions = {}): Promise<UploadSession> {
    const issue = validateMediaFile(file); if (issue) throw new APIError(422, 'invalid_media', issue);
    options.signal?.throwIfAborted();
    const hash = [...new Uint8Array(await crypto.subtle.digest('SHA-256', await file.arrayBuffer()))].map(byte => byte.toString(16).padStart(2, '0')).join('');
    options.signal?.throwIfAborted();
    const reserved = await this.client.request<UploadSession>(`${this.base}/media/uploads`, 'POST', { filename: file.name, size_bytes: file.size, sha256: hash, declared_mime: file.type }, options);
    const upload = `${this.base}/media/uploads/${id(reserved.id)}`;
    await this.client.request<void>(`${upload}/content`, 'PUT', file, { ...options, raw: true });
    // Current complete operation persists synchronously; there is no GET status
    // operation. Surface its declared status rather than synthesizing a poll API.
    return this.client.request(`${upload}/complete`, 'POST', undefined, options);
  }
  linkProductMedia(productId: string, body: LinkProductMedia, options: MutationOptions = {}): Promise<ProductMedia> { id(body.media_id); return this.client.request(`${this.productPath(productId)}/media`, 'POST', body, options); }
  deleteMedia(mediaId: string, options: MutationOptions = {}): Promise<void> { return this.client.request(`${this.base}/media/${id(mediaId)}`, 'DELETE', undefined, options); }
}
