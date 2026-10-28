import type { Observation } from './observations.ts';

export const numericFields = ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'call_count', 'cost_usd'] as const;
const fields = ['harness', 'canonical_id', 'identity_strength', 'grain', 'timestamp', 'interval_start', 'interval_end', 'provider', 'model', 'session_id', 'parent_session_id', 'is_subagent', ...numericFields, 'cost_basis', 'quality'] as const;
export type Contribution = Omit<Observation, 'diagnostics'>;
function text(value: unknown, max: number): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= max && !/[\u0000-\u001f]/.test(value);
}
/** A single observation failed the allowlist; `field` names the offending field and never carries its value. */
export class MetadataError extends Error {
  readonly field: string;
  constructor(message: string, field: string) { super(message); this.field = field; }
}
/** The single persistence and outbound allowlist. Never copy an adapter record wholesale. */
export function metadata(value: Observation): Observation {
  const projected = Object.fromEntries(fields.map(key => [key, value[key]])) as Contribution;
  if (!['claude-code', 'codex'].includes(projected.harness)) throw new MetadataError('Invalid canonical identity', 'harness');
  if (!text(projected.canonical_id, 512)) throw new MetadataError('Invalid canonical identity', 'canonical_id');
  if (!['response', 'request', 'scoped'].includes(projected.identity_strength)) throw new MetadataError('Invalid accounting grain', 'identity_strength');
  if (!['request', 'reconstructed_interval', 'session_summary'].includes(projected.grain)) throw new MetadataError('Invalid accounting grain', 'grain');
  if (!['reported', 'estimated', 'unknown'].includes(projected.cost_basis)) throw new MetadataError('Invalid accounting quality', 'cost_basis');
  if (!['observed', 'reconstructed', 'partial', 'summary'].includes(projected.quality)) throw new MetadataError('Invalid accounting quality', 'quality');
  for (const key of numericFields) {
    const number = projected[key];
    if (number !== null && (typeof number !== 'number' || !Number.isFinite(number) || number < 0 || number > (key === 'cost_usd' ? 10_000 : 1e9) || (key !== 'cost_usd' && !Number.isSafeInteger(number)))) throw new MetadataError(`Invalid ${key}`, key);
  }
  for (const key of ['timestamp', 'interval_start', 'interval_end'] as const) {
    const time = projected[key];
    if (time !== null) {
      if (typeof time !== 'string' || !Number.isFinite(Date.parse(time)) || Date.parse(time) < 0) throw new MetadataError('Invalid timestamp', key);
      projected[key] = new Date(time).toISOString();
    }
  }
  for (const [key, max] of [['provider', 64], ['model', 128]] as const) if (!text(projected[key], max)) throw new MetadataError(`Invalid ${key}`, key);
  for (const key of ['session_id', 'parent_session_id'] as const) if (projected[key] !== null && !text(projected[key], 128)) throw new MetadataError('Invalid session identity', key);
  if (projected.is_subagent !== null && typeof projected.is_subagent !== 'boolean') throw new MetadataError('Invalid subagent flag', 'is_subagent');
  if ((projected.cost_usd === null) !== (projected.cost_basis === 'unknown')) throw new MetadataError('Invalid cost basis', 'cost_basis');
  if (projected.grain === 'reconstructed_interval' && (projected.call_count !== null || projected.quality !== 'reconstructed')) throw new MetadataError('Invalid reconstructed quality/calls', projected.call_count !== null ? 'call_count' : 'quality');
  if (value.diagnostics !== undefined && !Array.isArray(value.diagnostics)) throw new MetadataError('Invalid diagnostics', 'diagnostics');
  return { ...projected, diagnostics: [...new Set((value.diagnostics ?? []).filter(code => /^[A-Z][A-Z0-9_]{0,95}$/.test(code)))].sort() };
}
export function outbound(value: Observation): Contribution {
  const { diagnostics: _, ...allowed } = metadata(value);
  return allowed;
}

/** Select a complete frame, never sum streaming frames or invent category-wise maxima. */
export function reconcile(left: Observation, right: Observation): Observation {
  const a = metadata(left), b = metadata(right);
  if (a.harness !== b.harness || a.canonical_id !== b.canonical_id || a.grain !== b.grain) throw new Error('Cannot reconcile different contributions');
  const rank = (value: Observation): number[] => [
    { observed: 4, reconstructed: 3, partial: 2, summary: 1 }[value.quality],
    value.grain === 'session_summary' ? Date.parse(value.timestamp ?? value.interval_end ?? '1970-01-01') : 0,
    numericFields.filter(key => value[key] !== null).length,
    numericFields.filter(key => key !== 'cost_usd').reduce((sum, key) => sum + (value[key] ?? 0), 0),
  ];
  const aRank = rank(a), bRank = rank(b);
  const difference = aRank.map((value, index) => value - bRank[index]).find(value => value !== 0) ?? 0;
  const winner = difference > 0 ? a : difference < 0 ? b : JSON.stringify(outbound(a)).localeCompare(JSON.stringify(outbound(b))) <= 0 ? a : b;
  const conflict = numericFields.some(key => a[key] !== null && b[key] !== null && a[key] !== b[key]);
  const timestamp = a.grain === 'session_summary' ? winner.timestamp : [a.timestamp, b.timestamp].filter((value): value is string => value !== null).sort()[0] ?? null;
  return { ...winner, timestamp, diagnostics: [...new Set([...a.diagnostics, ...b.diagnostics, ...(conflict ? ['CANONICAL_CONFLICT'] : [])])].sort() };
}
