/** Admin input preflight. Depends on native File/BigInt; called by forms and API.
 * Tenant IDs select routes, never grant authority. Server validation is definitive. */
export const validateUUID = (value: string): boolean => /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
export function validateCode(value: string): string | undefined {
  return /^[a-zA-Z0-9._-]{1,128}$/.test(value.replace(/^[ \t\r\n\v\f]+|[ \t\r\n\v\f]+$/g, '')) ? undefined : 'Use 1–128 ASCII letters, digits, dots, underscores or hyphens.';
}
export function parseOptionValueIds(value: string): string[] {
  if (!value.trim()) return [];
  const ids = value.split(/[\s,]+/).filter(Boolean).map(id => id.toLowerCase());
  if (ids.some(id => !validateUUID(id)) || new Set(ids).size !== ids.length) throw new Error('Option values must be unique UUIDs.');
  return ids;
}
export function validateMediaFile(file: File): string | undefined {
  if (file.size < 1 || file.size > 10 * 1024 * 1024) return 'Choose a nonempty image up to 10 MiB.';
  if (!file.name || new TextEncoder().encode(file.name).length > 255 || /[\/\\\x00-\x1f\x7f]/.test(file.name) || file.name === '.' || file.name === '..') return 'Choose an image with a safe filename.';
  // Blank type: some platforms cannot map the extension; the server verifies the real bytes.
  if (file.type && !['image/jpeg', 'image/png', 'image/webp', 'image/avif'].includes(file.type)) return 'Supported images: JPEG, PNG, WebP and AVIF.';
}
/** Decimal display without floating-point division. Currency/exponent comes from contract. */
export function formatMinorMoney(value: string | number | bigint, exponent: number): string {
  if (!Number.isInteger(exponent) || exponent < 0 || exponent > 6 || (typeof value === 'number' && !Number.isSafeInteger(value)) || (typeof value === 'string' && !/^-?\d+$/.test(value))) throw new Error('Invalid minor-unit amount.');
  const amount = BigInt(value), negative = amount < 0n;
  const digits = (negative ? -amount : amount).toString().padStart(exponent + 1, '0');
  return (negative ? '-' : '') + (exponent ? `${digits.slice(0, -exponent)}.${digits.slice(-exponent)}` : digits);
}
/** Runs fire at wall-clock time end (ms). setTimeout caps near 24.8 days, so wake early and re-arm. Returns cancel. */
export function scheduleAt(end: number, fire: () => void, now: () => number = Date.now): () => void {
  let timer: ReturnType<typeof setTimeout>;
  const arm = () => { const left = end - now(); if (left > 2147483647) timer = setTimeout(arm, 2147483647); else timer = setTimeout(fire, Math.max(0, left)); };
  arm(); return () => clearTimeout(timer);
}
