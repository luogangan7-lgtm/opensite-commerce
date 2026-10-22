/** Responsibility: same-origin purchasing/customer transport; depends on guarded server adapter.
 * Callers: purchasing React islands; invariants: explicit backend configuration, bounded problems and no-store.
 */
import type { APIRoute } from 'astro';
import { handlePurchaseRequest } from '../../../lib/commerce-server';
export const ALL: APIRoute = ({ request, cookies, params }) => handlePurchaseRequest(request, cookies, params.operation ?? '');
