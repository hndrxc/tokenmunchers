import { isAbsolute } from 'node:path';
import type { LedgerState, SourceState } from './ledger.ts';
import type { Harness, Observation } from './observations.ts';
import { metadata } from './metadata.ts';
import { attributionIssue } from './aggregate.ts';

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function fail(): never { throw new Error('Invalid protected ledger state'); }
function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return fail();
  return value as Record<string, unknown>;
}
function keys(value: Record<string, unknown>, allowed: string[]): void {
  if (Object.keys(value).some(key => !allowed.includes(key))) fail();
}
function time(value: unknown): string {
  if (typeof value !== 'string' || !Number.isFinite(Date.parse(value)) || Date.parse(value) < 0 || new Date(value).toISOString() !== value) return fail();
  return value;
}
function integer(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) return fail();
  return value;
}
function id(value: unknown): string {
  if (typeof value !== 'string' || !value || value.length > 512 || /[\u0000-\u001f]/.test(value)) return fail();
  return value;
}
export function relativePath(value: unknown): string {
  if (typeof value !== 'string' || !value || isAbsolute(value) || /^[A-Za-z]:/.test(value) || value.startsWith('\\') || value.split(/[\\/]/).some(part => part === '..' || part === '') || /[\u0000-\u001f]/.test(value)) return fail();
  return value;
}
function list<T>(value: unknown, item: (value: unknown) => T): T[] {
  if (!Array.isArray(value)) return fail();
  return value.map(item);
}
function ids(value: unknown): string[] {
  const result = list(value, id);
  if (new Set(result).size !== result.length) fail();
  return result;
}
function aliases(value: unknown): { from: string; to: string }[] {
  const result = list(value, entry => {
    const row = object(entry); keys(row, ['from', 'to']);
    return { from: id(row.from), to: id(row.to) };
  });
  const from = new Set(result.map(alias => alias.from));
  if (from.size !== result.length || result.some(alias => alias.from === alias.to || from.has(alias.to))) fail();
  return result;
}
function observation(value: unknown, harness: Harness): Observation {
  const result = metadata(object(value) as unknown as Observation);
  if (result.harness !== harness) fail();
  return result;
}
/** Validate on both read and write; only the metadata projection survives persistence. */
export function validateState(value: unknown): LedgerState {
  const state = object(value); keys(state, ['version', 'sources']);
  if (state.version !== 1) fail();
  const sources = object(state.sources); keys(sources, ['claude-code', 'codex']);
  const result: LedgerState = { version: 1, sources: {} };
  for (const [name, entry] of Object.entries(sources)) {
    const harness = name as Harness;
    const source = object(entry);
    keys(source, ['root', 'device_id', 'cutoff', 'exclusions', 'pauses', 'records', 'excluded_ids', 'superseded_ids', 'identity_aliases', 'material_alias_ids', 'rejected_observations', 'conflicting_ids', 'cursor', 'continuation', 'progress', 'pending', 'delivered_at']);
    if (typeof source.root !== 'string' || !isAbsolute(source.root) || /[\u0000-\u001f]/.test(source.root) || typeof source.device_id !== 'string' || !uuid.test(source.device_id)) fail();
    const records = Object.fromEntries(Object.entries(object(source.records)).map(([key, entry]) => {
      const row = observation(entry, harness);
      if (key !== row.canonical_id) fail();
      return [key, row];
    }));
    const pauses = list(source.pauses, entry => {
      const row = object(entry); keys(row, ['start', 'end']);
      const start = time(row.start), end = row.end === null ? null : time(row.end);
      if (end !== null && end < start) fail();
      return { start, end };
    });
    if (pauses.some((pause, index) => index > 0 && (pauses[index - 1].end === null || pauses[index - 1].end! > pause.start))) fail();
    const progress = object(source.progress); keys(progress, ['scans', 'observations', 'diagnostics', 'at']);
    const diagnostics = Object.fromEntries(Object.entries(object(progress.diagnostics)).map(([code, count]) => {
      if (!/^[A-Z][A-Z0-9_]{0,95}$/.test(code)) fail();
      return [code, integer(count)];
    }));
    const identityAliases = aliases(source.identity_aliases);
    const aliasSources = new Set(identityAliases.map(alias => alias.from));
    // State written before materiality tracking cannot prove which aliases were local-only, so it keeps uploading all of them.
    const materialAliases = source.material_alias_ids === undefined ? [...aliasSources] : ids(source.material_alias_ids);
    if (materialAliases.some(key => !aliasSources.has(key))) fail();
    const rejected = Object.fromEntries(Object.entries(object(source.rejected_observations ?? {})).map(([digest, field]) => {
      if (!/^[0-9a-f]{64}$/.test(digest) || typeof field !== 'string' || !/^[a-z_]{1,32}$/.test(field)) fail();
      return [digest, field];
    }));
    const normalized: SourceState = {
      root: source.root, device_id: source.device_id, cutoff: time(source.cutoff),
      exclusions: list(source.exclusions, relativePath), pauses, records,
      excluded_ids: ids(source.excluded_ids), superseded_ids: ids(source.superseded_ids), identity_aliases: identityAliases, material_alias_ids: materialAliases, rejected_observations: rejected,
      conflicting_ids: ids(source.conflicting_ids ?? []),
      progress: { scans: integer(progress.scans), observations: integer(progress.observations), diagnostics, at: progress.at === null ? null : time(progress.at) },
    };
    if (normalized.excluded_ids.some(key => Object.hasOwn(records, key)) || normalized.superseded_ids.some(key => Object.hasOwn(records, key)) || normalized.identity_aliases.some(alias => Object.hasOwn(records, alias.from))) fail();
    if (source.cursor !== undefined && source.cursor !== null) {
      const cursor = object(source.cursor); keys(cursor, ['file', 'offset', 'line', 'device', 'inode', 'discardLine']);
      if (cursor.discardLine !== undefined && typeof cursor.discardLine !== 'boolean') fail();
      normalized.cursor = { file: relativePath(cursor.file), offset: integer(cursor.offset), line: integer(cursor.line), device: integer(cursor.device), inode: integer(cursor.inode), ...(cursor.discardLine === undefined ? {} : { discardLine: cursor.discardLine }) };
    }
    if (source.continuation !== undefined) {
      const continuation = object(source.continuation); keys(continuation, ['nextFile']);
      normalized.continuation = { nextFile: relativePath(continuation.nextFile) };
    }
    if (source.pending !== undefined) {
      const pending = object(source.pending); keys(pending, ['snapshot_id', 'observations', 'superseded_ids', 'identity_aliases', 'acknowledged']);
      if (typeof pending.snapshot_id !== 'string' || !uuid.test(pending.snapshot_id)) fail();
      const observations = list(pending.observations, entry => observation(entry, harness));
      if (new Set(observations.map(row => row.canonical_id)).size !== observations.length) fail();
      normalized.pending = { snapshot_id: pending.snapshot_id, observations, superseded_ids: ids(pending.superseded_ids), identity_aliases: aliases(pending.identity_aliases) };
      const pendingIds = new Map(observations.map(row => [row.canonical_id, row]));
      const retired = new Set(normalized.pending.superseded_ids);
      if (observations.some(row => attributionIssue(row, normalized.cutoff) !== null || retired.has(row.canonical_id)) || normalized.pending.identity_aliases.some(alias => pendingIds.has(alias.from) || pendingIds.get(alias.to)?.identity_strength !== 'response' || retired.has(alias.from) || retired.has(alias.to))) fail();
      if (pending.acknowledged !== undefined) {
        const acknowledged = object(pending.acknowledged); keys(acknowledged, ['requests', 'chunk_count']);
        const requests = integer(acknowledged.requests), chunkCount = integer(acknowledged.chunk_count);
        if (chunkCount > 1000 || requests > chunkCount + 1) fail();
        normalized.pending.acknowledged = { requests, chunk_count: chunkCount };
      }
    }
    if (source.delivered_at !== undefined) normalized.delivered_at = time(source.delivered_at);
    result.sources[harness] = normalized;
  }
  return result;
}
