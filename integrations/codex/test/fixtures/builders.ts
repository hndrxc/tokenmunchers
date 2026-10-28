import type { WireUsage } from '../../src/token-normalizer.ts';
// Synthetic accounting projections of pinned upstream structs, never real sessions.
export const thread = '11111111-1111-4111-8111-111111111111';
export function usage(input = 0, output = 0, read = 0, write = 0, reasoning = 0) {
  return { input_tokens: input, output_tokens: output, cached_input_tokens: read,
    cache_write_input_tokens: write, reasoning_output_tokens: reasoning, total_tokens: input + output };
}
export function row(type: string, payload: Record<string, unknown>, second = 0, ordinal?: number) {
  return { timestamp: `2026-10-15T10:00:${String(second).padStart(2, '0')}Z`, type, payload,
    ...(ordinal === undefined ? {} : { ordinal }) };
}
export function meta(extra: Record<string, unknown> = {}) {
  return row('session_meta', { id: thread, session_id: thread, source: 'cli', ...extra });
}
export function modern(id: string, amount: WireUsage, cumulative = amount, second = 2) {
  return row('token_usage_record', { response_id: id, thread_id: thread, session_id: thread,
    turn_id: 'fixture-turn', root_turn_id: 'fixture-turn', usage: amount,
    turn_token_usage: cumulative, thread_token_usage: cumulative }, second);
}
export function snapshot(total: WireUsage, last = total, second = 1) {
  return row('event_msg', { type: 'token_count', info: { total_token_usage: total,
    last_token_usage: last, model_context_window: 200000 }, rate_limits: null }, second);
}
