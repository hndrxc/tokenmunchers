import type { Adapter, Harness, ScanOptions } from '../../shared/src/observations.ts';
import { Ledger } from '../../shared/src/ledger.ts';
import { aggregate, attributionIssue } from '../../shared/src/aggregate.ts';

export class Collector {
  readonly ledger: Ledger;
  readonly adapters: Record<Harness, Adapter>;
  constructor(ledger: Ledger, adapters: Record<Harness, Adapter>) { this.ledger = ledger; this.adapters = adapters; }
  async scan(harness: Harness, options: ScanOptions = {}): Promise<Record<string, unknown>> {
    const source = await this.ledger.transaction(state => state.sources[harness]);
    if (!source) throw new Error('Activate the explicitly chosen source root first');
    if (options.exclude?.some(path => !source.exclusions.includes(path))) throw new Error('Exclusions are fixed at activation; do not change them during a scan');
    const result = await this.adapters[harness](source.root, { cursor: source.cursor, continuation: source.continuation, ...options, exclude: source.exclusions, maxBytes: options.maxBytes ?? 64 * 1024 * 1024, maxLineBytes: options.maxLineBytes ?? 1024 * 1024 });
    const recorded = await this.ledger.record(harness, result);
    return { ...(await this.preview(harness)), latest_scan: { ...recorded, diagnostics: result.diagnostics.map(diagnostic => ({ code: diagnostic.code, ...(diagnostic.file ? { file: diagnostic.file } : {}), ...(diagnostic.line ? { line: diagnostic.line } : {}) })), projects: Array.isArray(result.projects) ? result.projects : [], progress: result.progress ?? null } };
  }
  async preview(harness: Harness): Promise<Record<string, unknown>> {
    return this.ledger.transaction(state => {
      const source = state.sources[harness];
      if (!source) throw new Error('Activate source first');
      const records = Object.values(source.records);
      const additive = records.filter(record => !attributionIssue(record, source.cutoff));
      const timestamps = records.flatMap(record => record.timestamp ? [record.timestamp] : []).sort();
      const unknown: Record<string, number> = {};
      for (const field of ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'call_count', 'cost_usd', 'timestamp', 'is_subagent'] as const) unknown[field] = records.filter(record => record[field] === null).length;
      unknown.provider = records.filter(record => record.provider === 'unknown').length;
      const qualities: Record<string, number> = {};
      for (const record of records) qualities[record.quality] = (qualities[record.quality] ?? 0) + 1;
      const rejected: Record<string, number> = {};
      for (const field of Object.values(source.rejected_observations)) rejected[field] = (rejected[field] ?? 0) + 1;
      return {
        harness, root: source.root, cutoff: source.cutoff, device_id: source.device_id,
        paused: source.pauses.some(pause => pause.end === null), exclusions: source.exclusions,
        range_utc: { first: timestamps[0] ?? null, last: timestamps[timestamps.length - 1] ?? null },
        retained_contributions: records.length,
        history: additive.filter(record => record.timestamp! < source.cutoff).length,
        live: additive.filter(record => record.timestamp! >= source.cutoff).length,
        excluded: source.excluded_ids.length, unknown, qualities,
        weak_identity_count: records.filter(record => record.identity_strength === 'scoped').length,
        ownership_policy: 'One owner per source archive. Strong copied identities conflict server-side; independently rewritten weak identities cannot be globally matched.',
        ...aggregate(records, source.cutoff), progress: source.progress,
        scan_pending: { cursor: source.cursor ?? null, continuation: source.continuation ?? null },
        canonical_conflicts: records.filter(record => record.diagnostics.includes('CANONICAL_CONFLICT')).length,
        identity_alias_conflicts: records.filter(record => record.diagnostics.includes('IDENTITY_ALIAS_CONFLICT')).length,
        rejected_observations: rejected,
        superseded_intervals: source.superseded_ids.length,
        pending_snapshot: source.pending?.snapshot_id ?? null,
        pending_acknowledged: source.pending?.acknowledged ?? null,
      };
    });
  }
}
