/**
 * Responsibility: owner-specified, read-only orders contract and admin views.
 * Dependencies: generated OpenAPI client over AdminAPI, React Router, shared UI; callers: admin routes.
 * Invariants: PR47 snapshots identify lines by SKU, never inferred product titles;
 * minor amounts stay exact strings/BigInt; no fulfillment/refund writes;
 * contract fixture is visibly labelled and does not establish backend authorization.
 */
import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router';
import { Badge, Button, Card, DataTable, EmptyState, FormField, Select } from '@opensite/ui';
import { AdminAPI, APIError } from './api';
import { createCommerceClient } from '../../../packages/api-client/src';

export const ORDER_STATES = ['pending_payment', 'paid', 'partially_fulfilled', 'fulfilled', 'cancelled', 'partially_refunded', 'refunded'] as const;
export type OrderState = typeof ORDER_STATES[number];
export const ORDERS_FIXTURE_LABEL = 'CONTRACT FIXTURE (orders read API pending PR)';
export interface OrderSummary { id: string; order_number: number; state: OrderState; currency: string; total_minor: string; customer_kind: 'customer' | 'guest'; created_at: string }
export interface OrderPage { items: OrderSummary[]; next_cursor?: string }
export interface OrderDetail { id: string; order_number: number; state: OrderState; version: number; currency: string; lines: {line_id: string; sku_code: string; title: string | null; quantity: number; unit_price_minor: string; tax_minor: string; line_total_minor: string}[]; adjustments: {id: string; reason: string; amount_minor: string}[]; shipping: {amount_minor: string; method: string | null}; tax_total_minor: string; total_minor: string; created_at: string; updated_at: string }
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function invalid(): never { throw new Error('Invalid orders response. Retry or contact support.'); }
function object(value: unknown): Record<string, unknown> { if (!value || typeof value !== 'object' || Array.isArray(value)) return invalid(); return value as Record<string, unknown>; }
function text(value: unknown): string { if (typeof value !== 'string' || value.length === 0) return invalid(); return value; }
// PR47 allows string|null here; method denotes a rate rule, not a carrier.
function nullableText(value: unknown): string | null { if (value === null || typeof value === 'string') return value; return invalid(); }
function uuid(value: unknown): string { const v = text(value); if (!uuidPattern.test(v)) return invalid(); return v.toLowerCase(); }
function minor(value: unknown): string { const v = text(value); if (!/^(0|[1-9]\d*|-[1-9]\d*)$/.test(v)) return invalid(); return v; }
function positive(value: unknown): number { if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1) return invalid(); return value; }
function date(value: unknown): string { const v = text(value); if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(v) || !Number.isFinite(Date.parse(v))) return invalid(); const month = Number(v.slice(5, 7)), day = Number(v.slice(8, 10)); if (month < 1 || month > 12 || day < 1 || day > new Date(Date.UTC(Number(v.slice(0, 4)), month, 0)).getUTCDate()) return invalid(); return v; }
function currency(value: unknown): string { const v = text(value); if (!/^[A-Z]{3}$/.test(v)) return invalid(); return v; }
function state(value: unknown): OrderState { if (!ORDER_STATES.includes(value as OrderState)) return invalid(); return value as OrderState; }
function array<T>(value: unknown, parse: (v: unknown) => T): T[] { if (!Array.isArray(value)) return invalid(); return value.map(parse); }
function base(value: unknown) { const v = object(value); return {id: uuid(v.id), order_number: positive(v.order_number), state: state(v.state), currency: currency(v.currency), total_minor: minor(v.total_minor), created_at: date(v.created_at)}; }
export function parseOrderPage(value: unknown): OrderPage {
  const v = object(value);
  const items = array(v.items, row => { const r = object(row); if (r.customer_kind !== 'customer' && r.customer_kind !== 'guest') return invalid(); return {...base(r), customer_kind: r.customer_kind as OrderSummary['customer_kind']}; });
  if (v.next_cursor !== undefined && typeof v.next_cursor !== 'string') return invalid();
  return {items, ...(v.next_cursor === undefined ? {} : {next_cursor: v.next_cursor as string})};
}
export function parseOrderDetail(value: unknown): OrderDetail {
  const v = object(value), shipping = object(v.shipping);
  return {...base(v), version: positive(v.version), updated_at: date(v.updated_at), tax_total_minor: minor(v.tax_total_minor), shipping: {amount_minor: minor(shipping.amount_minor), method: nullableText(shipping.method)},
    lines: array(v.lines, value => { const r = object(value); return {line_id: uuid(r.line_id), sku_code: text(r.sku_code), title: nullableText(r.title), quantity: positive(r.quantity), unit_price_minor: minor(r.unit_price_minor), tax_minor: minor(r.tax_minor), line_total_minor: minor(r.line_total_minor)}; }),
    adjustments: array(v.adjustments, value => { const r = object(value); return {id: uuid(r.id), reason: text(r.reason), amount_minor: minor(r.amount_minor)}; })};
}
/** Intl supplies the currency exponent; only BigInt touches monetary digits. */
export function formatMoney(amount: string, code: string): string {
  const n = BigInt(minor(amount)); currency(code);
  // Domain Money allows any three uppercase ASCII letters. Never guess an
  // exponent for a code Intl does not recognize; preserve explicit minor units.
  if (!Intl.supportedValuesOf('currency').includes(code)) return `${code} ${n} minor units`;
  const digits = new Intl.NumberFormat('en', {style: 'currency', currency: code}).resolvedOptions().maximumFractionDigits ?? 2;
  const scale = 10n ** BigInt(digits), absolute = n < 0n ? -n : n;
  const whole = new Intl.NumberFormat('en', {maximumFractionDigits: 0}).format(absolute / scale);
  const fraction = digits ? `.${(absolute % scale).toString().padStart(digits, '0')}` : '';
  return `${code} ${n < 0n ? '-' : ''}${whole}${fraction}`;
}
export class OrdersAPI {
  private readonly client;
  constructor(api: AdminAPI) {
    // Only the generated URL path reaches the cookie transport; its origin is
    // never fetched. Keep expiry, aborts, redirect denial and safe errors in AdminAPI.
    this.client = createCommerceClient('http://localhost', request => {
      const url = new URL(request.url);
      return api.request<Response>(url.pathname + url.search, 'GET', undefined,
        { signal: request.signal, responseRaw: true });
    });
  }
  async list(tenantId: string, options: {cursor?: string; limit?: number; state?: OrderState; signal?: AbortSignal} = {}): Promise<OrderPage> {
    const limit = options.limit ?? 25; if (!Number.isSafeInteger(limit) || limit < 1 || limit > 100) throw new Error('Order page limit must be 1–100.');
    const { data } = await this.client.GET('/api/v1/t/{tenant_id}/orders', {
      params: { path: { tenant_id: uuid(tenantId) }, query: { limit, cursor: options.cursor || undefined,
        state: options.state ? state(options.state) : undefined } }, signal: options.signal,
    });
    return parseOrderPage(data);
  }
  async get(tenantId: string, orderId: string, signal?: AbortSignal): Promise<OrderDetail> {
    const { data, response } = await this.client.GET('/api/v1/t/{tenant_id}/orders/{order_id}', {
      params: { path: { tenant_id: uuid(tenantId), order_id: uuid(orderId) } }, signal,
    });
    const order = parseOrderDetail(data);
    if (response.headers.get('ETag') !== `"${order.version}"`) throw new Error('Invalid order version response. Retry or contact support.');
    return order;
  }
}
function errorHint(error: unknown) { return error instanceof APIError ? error.userHint : 'Unable to load orders. Retry or contact support.'; }
function OrderError({message, retry}: {message: string; retry: () => void}) { return <div className="admin-error" role="alert"><p>{message}</p><Button onClick={retry}>Retry</Button></div>; }
interface PageProps {api: OrdersAPI; tenantId: string; fixture?: boolean}
export function OrderListPage({api, tenantId, fixture = false}: PageProps) {
  const [filter, setFilter] = useState<OrderState | ''>(''); const [cursor, setCursor] = useState(''); const [history, setHistory] = useState<string[]>([]);
  const [page, setPage] = useState<OrderPage>(); const [loadedFor, setLoadedFor] = useState(''); const requestKey = JSON.stringify([tenantId, cursor, filter]); const visiblePage = loadedFor === requestKey ? page : undefined; const [error, setError] = useState(''); const [loading, setLoading] = useState(true); const [retry, setRetry] = useState(0);
  useEffect(() => { const controller = new AbortController(); setLoading(true); setPage(undefined); setError('');
    api.list(tenantId, {cursor, state: filter || undefined, signal: controller.signal}).then(value => { if (!controller.signal.aborted) { setPage(value); setLoadedFor(requestKey); } }).catch(error => { if (!controller.signal.aborted) setError(errorHint(error)); }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort(); }, [api, tenantId, cursor, filter, retry, requestKey]);
  // Parent keys tenant-scoped routes; changing the target also clears pagination.
  useEffect(() => { setCursor(''); setHistory([]); }, [tenantId]);
  return <section className="admin-page" aria-labelledby="orders-title"><h1 id="orders-title">Orders</h1>{fixture && <p className="admin-muted">{ORDERS_FIXTURE_LABEL}</p>}
    <div className="admin-form"><FormField id="order-state" label="Order state"><Select value={filter} onChange={event => { setFilter(event.target.value as OrderState | ''); setCursor(''); setHistory([]); }}><option value="">All states</option>{ORDER_STATES.map(s => <option key={s} value={s}>{s.replaceAll('_', ' ')}</option>)}</Select></FormField></div>
    {error ? <OrderError message={error} retry={() => setRetry(n => n + 1)} /> : <Card><DataTable caption="Order list" loading={loading} emptyLabel="No orders match this filter." columns={[{key: 'number', label: 'Order'}, {key: 'state', label: 'Order state'}, {key: 'customer', label: 'Customer'}, {key: 'total', label: 'Total'}, {key: 'created', label: 'Created'}]} rows={(visiblePage?.items ?? []).map(order => ({id: order.id, number: <Link to={order.id}>{order.order_number}</Link>, state: <Badge tone="neutral">{order.state.replaceAll('_', ' ')}</Badge>, customer: order.customer_kind, total: formatMoney(order.total_minor, order.currency), created: <time dateTime={order.created_at}>{order.created_at}</time>}))} /></Card>}
    <div className="admin-actions"><Button variant="secondary" disabled={loading || !history.length} onClick={() => { setCursor(history.at(-1) ?? ''); setHistory(history.slice(0, -1)); }}>Previous page</Button><Button variant="secondary" disabled={loading || !!error || !visiblePage?.next_cursor} onClick={() => { if (visiblePage?.next_cursor) { setHistory([...history, cursor]); setCursor(visiblePage.next_cursor); } }}>Next page</Button></div>
  </section>;
}
export function OrderDetailPage({api, tenantId, fixture = false}: PageProps) {
  const {orderId} = useParams<{orderId: string}>(); const [loadedOrder, setOrder] = useState<OrderDetail>(); const [loadedFor, setLoadedFor] = useState(''); const requestKey = JSON.stringify([tenantId, orderId]); const order = loadedFor === requestKey ? loadedOrder : undefined; const [error, setError] = useState(''); const [loading, setLoading] = useState(true); const [retry, setRetry] = useState(0);
  useEffect(() => { const controller = new AbortController(); setLoading(true); setOrder(undefined); setError('');
    api.get(tenantId, orderId ?? '', controller.signal).then(value => { if (!controller.signal.aborted) { setOrder(value); setLoadedFor(requestKey); } }).catch(error => { if (!controller.signal.aborted) setError(error instanceof APIError && error.status === 404 ? 'Order not found or unavailable in this tenant.' : errorHint(error)); }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort(); }, [api, tenantId, orderId, retry, requestKey]);
  return <section className="admin-page" aria-labelledby="order-title"><Link to=".." relative="path">Back to orders</Link><h1 id="order-title">{order ? `Order ${order.order_number}` : 'Order details'}</h1>{fixture && <p className="admin-muted">{ORDERS_FIXTURE_LABEL}</p>}
    {loading ? <p role="status">Loading order…</p> : error ? <OrderError message={error} retry={() => setRetry(n => n + 1)} /> : !order ? <EmptyState headingLevel={2} title="Order unavailable" /> : <>
      <p><Badge tone="neutral">{order.state.replaceAll('_', ' ')}</Badge> · Version {order.version}</p>
      <Card><DataTable caption="Order lines" emptyLabel="No order lines in this snapshot." columns={[{key: 'sku', label: 'SKU'}, {key: 'quantity', label: 'Quantity'}, {key: 'unit', label: 'Unit price'}, {key: 'tax', label: 'Tax'}, {key: 'total', label: 'Line total'}]} rows={order.lines.map(line => ({id: line.line_id, sku: /\s/.test(line.sku_code) ? <span className="admin-order-text">{line.sku_code}</span> : line.sku_code, quantity: line.quantity, unit: formatMoney(line.unit_price_minor, order.currency), tax: formatMoney(line.tax_minor, order.currency), total: formatMoney(line.line_total_minor, order.currency)}))} /></Card>
      <Card><h2>Totals</h2><dl className="admin-stack"><dt>Shipping rule ({order.shipping.method || 'Not recorded'})</dt><dd>{formatMoney(order.shipping.amount_minor, order.currency)}</dd>{order.adjustments.map(a => <div key={a.id}><dt>{a.reason}</dt><dd>{formatMoney(a.amount_minor, order.currency)}</dd></div>)}<dt>Tax total</dt><dd>{formatMoney(order.tax_total_minor, order.currency)}</dd><dt>Order total</dt><dd>{formatMoney(order.total_minor, order.currency)}</dd></dl></Card>
      <p className="admin-muted">Created <time dateTime={order.created_at}>{order.created_at}</time> · Updated <time dateTime={order.updated_at}>{order.updated_at}</time></p>
    </>}
  </section>;
}
