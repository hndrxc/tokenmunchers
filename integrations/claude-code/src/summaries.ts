import { createHash } from 'node:crypto';
import type { Observation } from '../../shared/src/observations.ts';
import type { Transcript } from './discovery.ts';
import { COST_LIMIT, TOKEN_LIMIT, object, provider, timestamp, token } from './normalizer.ts';

/** Pinned 2.1.312 cost-state and SDK result shapes; never repair request output from aggregates. */
export function summaries(entry: Record<string, unknown>, file: Transcript): Observation[] {
  if (entry.type !== 'cost-state' && entry.type !== 'result') return [];
  if (entry.type === 'result' && entry.subtype === 'error_during_execution') return [];
  const session = typeof entry.sessionId === 'string' ? entry.sessionId : typeof entry.session_id === 'string' ? entry.session_id : file.sessionId;
  const modelUsage = object(entry.modelUsage);
  const models = modelUsage ? Object.entries(modelUsage) : [];
  const source = entry.type;
  const start = typeof entry.startTime === 'number' && Number.isFinite(entry.startTime) && entry.startTime >= 0 && entry.startTime <= 8.64e15 ? new Date(entry.startTime).toISOString() : null;
  const result: Observation[] = [];
  for (const [model, value] of models.length ? models : [['unknown', null] as const]) {
    const usage = object(value);
    const cost = usage?.costUSD ?? (models.length ? undefined : source === 'cost-state' ? entry.totalCostUSD : entry.total_cost_usd);
    const knownCost = typeof cost === 'number' && Number.isFinite(cost) && cost >= 0 && cost <= COST_LIMIT ? cost : null;
    const infrastructure = provider(usage?.provider);
    const diagnostics = ['CUMULATIVE_SUMMARY_NONADDITIVE', 'SUMMARY_OVERLAP_UNRESOLVED', 'HELPER_COVERAGE_INCOMPLETE'];
    if (infrastructure === 'unknown') diagnostics.push('PROVIDER_UNVERIFIED');
    if (entry.hasUnknownModelCost === true || usage?.costBasis === 'unknown') diagnostics.push('COST_PRICING_UNVERIFIED');
    // Cumulative totals can exceed the shared metadata bounds; withhold them rather than fail the whole ledger record.
    const raw = [usage?.inputTokens, usage?.outputTokens, usage?.cacheReadInputTokens, usage?.cacheCreationInputTokens];
    if (raw.some(item => typeof item === 'number' && item > TOKEN_LIMIT) || (typeof cost === 'number' && cost > COST_LIMIT)) diagnostics.push('SUMMARY_VALUE_OUT_OF_RANGE');
    const digest = createHash('sha256').update(JSON.stringify([source, session, model])).digest('hex');
    result.push({
      harness: 'claude-code', canonical_id: `summary:${digest}`, identity_strength: 'scoped', grain: 'session_summary',
      timestamp: null, interval_start: start, interval_end: timestamp(entry.timestamp),
      provider: infrastructure, model, session_id: session, parent_session_id: file.parentSessionId, is_subagent: file.isSubagent,
      input_tokens: token(usage?.inputTokens), output_tokens: token(usage?.outputTokens),
      cache_read_tokens: token(usage?.cacheReadInputTokens), cache_write_tokens: token(usage?.cacheCreationInputTokens),
      call_count: null, cost_usd: knownCost, cost_basis: knownCost === null ? 'unknown' : 'estimated',
      quality: 'summary', diagnostics,
    });
  }
  return result;
}
