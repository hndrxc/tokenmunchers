export type ObjectValue = Record<string, unknown>;
export const object = (value: unknown): ObjectValue | null => value !== null && typeof value === 'object' && !Array.isArray(value) ? value as ObjectValue : null;
export const text = (value: unknown): string | null => typeof value === 'string' && value.length > 0 ? value : null;
export const integer = (value: unknown): value is number => Number.isSafeInteger(value) && (value as number) >= 0;
export interface WireUsage {
  input_tokens: number; cached_input_tokens: number; cache_write_input_tokens: number;
  output_tokens: number; reasoning_output_tokens: number; total_tokens: number;
}
export class UsageError extends Error { code: string; constructor(code: string) { super(code); this.code = code; } }
export function wireUsage(value: unknown): WireUsage {
  const source = object(value);
  if (!source) throw new UsageError('MALFORMED_USAGE');
  const result = {
    input_tokens: source.input_tokens, cached_input_tokens: source.cached_input_tokens,
    // Serde's explicit default is zero for the absent cache-write field, not for null.
    cache_write_input_tokens: source.cache_write_input_tokens === undefined ? 0 : source.cache_write_input_tokens,
    output_tokens: source.output_tokens, reasoning_output_tokens: source.reasoning_output_tokens,
    total_tokens: source.total_tokens,
  };
  if (!Object.values(result).every(integer)) throw new UsageError('MALFORMED_USAGE');
  return result as WireUsage;
}
export function normalize(value: unknown) {
  const usage = wireUsage(value);
  if (usage.cached_input_tokens + usage.cache_write_input_tokens > usage.input_tokens) throw new UsageError('MALFORMED_CACHE_SUBSET');
  if (usage.reasoning_output_tokens > usage.output_tokens) throw new UsageError('MALFORMED_REASONING_SUBSET');
  if (!Number.isSafeInteger(usage.input_tokens + usage.output_tokens) || usage.input_tokens + usage.output_tokens !== usage.total_tokens) throw new UsageError('MALFORMED_TOTAL');
  return {
    input_tokens: usage.input_tokens - usage.cached_input_tokens - usage.cache_write_input_tokens,
    output_tokens: usage.output_tokens, cache_read_tokens: usage.cached_input_tokens,
    cache_write_tokens: usage.cache_write_input_tokens,
  };
}
export const keys = ['input_tokens', 'cached_input_tokens', 'cache_write_input_tokens', 'output_tokens', 'reasoning_output_tokens', 'total_tokens'] as const;
export const equalUsage = (a: WireUsage, b: WireUsage): boolean => keys.every(key => a[key] === b[key]);
export function subtract(a: WireUsage, b: WireUsage): WireUsage | null {
  const result = {} as WireUsage;
  for (const key of keys) { const delta = a[key] - b[key]; if (!integer(delta)) return null; result[key] = delta; }
  return result;
}
