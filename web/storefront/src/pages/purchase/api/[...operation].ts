/** Responsibility: same-origin purchasing fixture transport; depends on guarded server adapter.
 * Callers: purchasing React islands; invariants: production closed, all response details generic/no-store.
 */
import type { APIRoute } from 'astro';
import { handlePurchaseRequest } from '../../../lib/commerce-server';
export const ALL: APIRoute = ({ request, cookies, params }) => handlePurchaseRequest(request, cookies, params.operation ?? '');
