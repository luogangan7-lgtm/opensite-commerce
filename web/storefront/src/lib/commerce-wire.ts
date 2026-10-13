/** Responsibility: lossless JSON integer lexemes and exact commerce bounds.
 * Dependencies: standard JSON; callers: server-only purchasing transport.
 * Invariants: int64 amounts never pass through Number; browser quantity is bounded to the API limit.
 */
export function parseIntegerJSON(text: string): unknown {
  if (text.length > 1_048_576) throw new Error('response_too_large');
  let result = '', quoted = false, escaped = false;
  for (let i = 0; i < text.length; i++) {
    const char = text[i]!;
    if (quoted) {
      result += char;
      if (escaped) escaped = false;
      else if (char === '\\') escaped = true;
      else if (char === '"') quoted = false;
    } else if (char === '"') { quoted = true; result += char; }
    else if (char === '-' || /[0-9]/.test(char)) {
      const match = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(text.slice(i));
      if (!match) throw new Error('invalid_json');
      result += JSON.stringify(match[0]); i += match[0].length - 1;
    } else result += char;
  }
  return JSON.parse(result);
}
export function minorAmount(value: unknown): string {
  if (typeof value !== 'string' || !/^(0|[1-9]\d{0,18})$/.test(value) || BigInt(value) > 9_223_372_036_854_775_807n) throw new Error('invalid_amount');
  return value;
}
export function safeInteger(value: unknown, min: number, max: number): number {
  if (typeof value !== 'string' || !/^(0|[1-9]\d*)$/.test(value)) throw new Error('invalid_integer');
  const integer = BigInt(value);
  if (integer < BigInt(min) || integer > BigInt(max)) throw new Error('invalid_integer');
  return Number(integer);
}
