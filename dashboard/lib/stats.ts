import type { DailyRow, Totals, UsageEvent } from "./types";

export const LIVE_WINDOW_MS = 90_000;

/** Unavailable is absorbing: adding a known value cannot repair an unknown total. */
export function addNullable(a: number | null, b: number | null): number | null {
  return a === null || b === null ? null : Number(a) + Number(b);
}

export function utcDay(d: Date | number = Date.now()): string {
  return new Date(d).toISOString().slice(0, 10);
}

export function startOfUtcDay(d: Date | number = Date.now()): string {
  return `${utcDay(d)}T00:00:00.000Z`;
}

/** The last n UTC days, oldest first, ending today. */
export function lastDays(n: number, now: number = Date.now()): string[] {
  const out: string[] = [];
  for (let i = n - 1; i >= 0; i--) out.push(utcDay(now - i * 86_400_000));
  return out;
}

export function emptyTotals(user_id: string): Totals {
  return {
    user_id,
    call_count: 0,
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    total_tokens: 0,
    subagent_tokens: 0,
    cost_usd: 0,
  };
}

export function byUser(rows: Totals[] | null): Record<string, Totals> {
  const out: Record<string, Totals> = {};
  for (const r of rows ?? []) out[r.user_id] = { ...r, cost_usd: r.cost_usd === null ? null : Number(r.cost_usd) };
  return out;
}

/** Returns a new map with the event folded into the user's totals. */
export function addEvent(map: Record<string, Totals>, e: UsageEvent): Record<string, Totals> {
  const prev = map[e.user_id] ?? emptyTotals(e.user_id);
  return {
    ...map,
    [e.user_id]: {
      ...prev,
      call_count: addNullable(prev.call_count, 1),
      input_tokens: addNullable(prev.input_tokens, e.input_tokens),
      output_tokens: addNullable(prev.output_tokens, e.output_tokens),
      cache_read_tokens: addNullable(prev.cache_read_tokens, e.cache_read_tokens),
      cache_write_tokens: addNullable(prev.cache_write_tokens, e.cache_write_tokens),
      total_tokens: addNullable(prev.total_tokens, e.total_tokens),
      subagent_tokens: addNullable(prev.subagent_tokens, e.is_subagent ? e.total_tokens : 0),
      cost_usd: addNullable(prev.cost_usd, Number(e.cost_usd)),
    },
  };
}

export function metricOf(t: Totals | undefined, metric: "tokens" | "cost", includeSubagents: boolean): number | null {
  if (!t) return 0;
  if (metric === "cost") return t.cost_usd;
  if (includeSubagents) return t.total_tokens;
  return t.total_tokens === null || t.subagent_tokens === null ? null : t.total_tokens - t.subagent_tokens;
}

/**
 * Daily token totals keyed user -> day. Rollup rows cover past days; today's
 * bucket comes from exact live totals instead (the rollup lags up to 10 minutes).
 */
export function dailyByUser(
  rows: DailyRow[],
  today: Record<string, Totals>,
  includeSubagents: boolean,
  todayKey: string = utcDay(),
): Record<string, Record<string, number | null>> {
  const out: Record<string, Record<string, number | null>> = {};
  for (const r of rows) {
    if (r.day >= todayKey || (!includeSubagents && r.is_subagent === true)) continue;
    const u = (out[r.user_id] ??= {});
    const value = !includeSubagents && r.is_subagent === null ? null : r.total_tokens;
    u[r.day] = addNullable(Object.hasOwn(u, r.day) ? u[r.day] : 0, value);
  }
  for (const [uid, t] of Object.entries(today)) {
    (out[uid] ??= {})[todayKey] = metricOf(t, "tokens", includeSubagents);
  }
  return out;
}

/** Current and longest runs of consecutive UTC days with any usage. */
export function streaks(activeDays: Iterable<string>, now: number = Date.now()): { current: number; longest: number } {
  const days = new Set(activeDays);
  const sorted = [...days].sort();
  let longest = 0;
  let run = 0;
  let prev: number | undefined;
  for (const d of sorted) {
    const t = Date.parse(`${d}T00:00:00Z`);
    run = prev !== undefined && t - prev === 86_400_000 ? run + 1 : 1;
    longest = Math.max(longest, run);
    prev = t;
  }
  // A streak is still alive if today has no usage yet but yesterday did.
  let cursor = days.has(utcDay(now)) ? now : now - 86_400_000;
  let current = 0;
  while (days.has(utcDay(cursor))) {
    current++;
    cursor -= 86_400_000;
  }
  return { current, longest };
}

/**
 * Realtime payloads come from logical replication, which (on Postgres 17)
 * omits generated columns like total_tokens, so recompute it here.
 */
export function normalizeEvent(raw: Record<string, unknown>): UsageEvent {
  const e = raw as unknown as UsageEvent;
  const n = (v: unknown) => Number(v ?? 0);
  return {
    ...e,
    input_tokens: n(e.input_tokens),
    output_tokens: n(e.output_tokens),
    cache_read_tokens: n(e.cache_read_tokens),
    cache_write_tokens: n(e.cache_write_tokens),
    total_tokens: n(e.input_tokens) + n(e.output_tokens) + n(e.cache_read_tokens) + n(e.cache_write_tokens),
    cost_usd: n(e.cost_usd),
  };
}
