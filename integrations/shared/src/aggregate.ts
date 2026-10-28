import type { Observation } from './observations.ts';
import { numericFields, reconcile } from './metadata.ts';

export interface DailyTotal {
  day: string;
  harness: string;
  provider: string;
  model: string;
  is_subagent: boolean | null;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  call_count: number | null;
  cost_usd: number | null;
  known_subtotals: Record<(typeof numericFields)[number], number>;
  qualities: string[];
  cost_bases: string[];
}
export function attributionIssue(observation: Observation, cutoff?: string): string | null {
  if (observation.diagnostics.includes('CONFLICTING_RESPONSE')) return 'CONFLICTING_RESPONSE';
  if (observation.grain === 'session_summary') return 'NONADDITIVE_SUMMARY';
  if (observation.timestamp === null) return 'MISSING_TIMESTAMP';
  if (observation.grain === 'reconstructed_interval' || observation.interval_start !== null || observation.interval_end !== null) {
    const { interval_start: start, interval_end: end, timestamp } = observation;
    if (!start || !end || start > end || start.slice(0, 10) !== end.slice(0, 10) || start.slice(0, 10) !== timestamp.slice(0, 10)) return 'AMBIGUOUS_INTERVAL_DAY';
    if (cutoff && start < cutoff && end >= cutoff) return 'INTERVAL_STRADDLES_CUTOFF';
  }
  return null;
}
/** Daily totals are nullable exact sums plus explicitly labeled known subtotals. */
export function aggregate(observations: Iterable<Observation>, cutoff?: string): { days: DailyTotal[]; withheld: Record<string, number> } {
  const unique = new Map<string, Observation>();
  for (const observation of observations) {
    const key = `${observation.harness}\0${observation.canonical_id}`;
    const previous = unique.get(key);
    unique.set(key, previous ? reconcile(previous, observation) : observation);
  }
  const groups = new Map<string, DailyTotal>();
  const withheld: Record<string, number> = {};
  for (const observation of unique.values()) {
    const issue = attributionIssue(observation, cutoff);
    if (issue) { withheld[issue] = (withheld[issue] ?? 0) + 1; continue; }
    const day = new Date(observation.timestamp!).toISOString().slice(0, 10);
    const key = JSON.stringify([day, observation.harness, observation.provider, observation.model, observation.is_subagent]);
    let total = groups.get(key);
    if (!total) {
      total = { day, harness: observation.harness, provider: observation.provider, model: observation.model, is_subagent: observation.is_subagent, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, call_count: 0, cost_usd: 0, known_subtotals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, call_count: 0, cost_usd: 0 }, qualities: [], cost_bases: [] };
      groups.set(key, total);
    }
    for (const field of numericFields) {
      const value = observation[field];
      if (value === null) total[field] = null;
      else {
        total.known_subtotals[field] += value;
        if (total[field] !== null) total[field] += value;
        if (!Number.isFinite(total.known_subtotals[field]) || (field !== 'cost_usd' && !Number.isSafeInteger(total.known_subtotals[field]))) throw new Error('Aggregate exceeds safe numeric range');
      }
    }
    total.qualities = [...new Set([...total.qualities, observation.quality])].sort();
    total.cost_bases = [...new Set([...total.cost_bases, observation.cost_basis])].sort();
  }
  return { days: [...groups.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([, total]) => total), withheld };
}
