import { chmod, mkdir, open } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { createHash, randomUUID } from 'node:crypto';
import { DatabaseSync } from 'node:sqlite';
import type { Harness, Observation, AdapterResult, ScanOptions } from './observations.ts';
import { metadata, MetadataError, reconcile } from './metadata.ts';
import { relativePath, validateState } from './state.ts';

export interface Pause { start: string; end: string | null }
export interface SourceState {
  root: string;
  device_id: string;
  cutoff: string;
  exclusions: string[];
  pauses: Pause[];
  records: Record<string, Observation>;
  excluded_ids: string[];
  superseded_ids: string[];
  conflicting_ids: string[];
  identity_aliases: { from: string; to: string }[];
  /** Alias sources through which this device recorded or merged a request-strength record; only these need server-side supersession. */
  material_alias_ids: string[];
  /** Observations rejected by the metadata allowlist, keyed by a SHA-256 of their canonical ID (never the raw values), naming the offending field. */
  rejected_observations: Record<string, string>;
  cursor?: ScanOptions['cursor'];
  continuation?: ScanOptions['continuation'];
  progress: { scans: number; observations: number; diagnostics: Record<string, number>; at: string | null };
  /** `acknowledged.requests` counts endpoint-acknowledged staged requests in order: begin, then chunks; commit clears `pending`. */
  pending?: { snapshot_id: string; observations: Observation[]; superseded_ids: string[]; identity_aliases: { from: string; to: string }[]; acknowledged?: { requests: number; chunk_count: number } };
  delivered_at?: string;
}
export interface LedgerState { version: 1; sources: Partial<Record<Harness, SourceState>> }

/** SQLite publishes normalized metadata and progress in one crash-safe transaction. */
export class Ledger {
  readonly directory: string;
  constructor(directory: string) { this.directory = resolve(directory); }
  async transaction<T>(change: (state: LedgerState) => T | Promise<T>): Promise<T> {
    await mkdir(this.directory, { recursive: true, mode: 0o700 });
    await chmod(this.directory, 0o700);
    const path = join(this.directory, 'ledger.sqlite');
    const file = await open(path, 'a', 0o600);
    await file.close();
    await chmod(path, 0o600);
    const database = new DatabaseSync(path);
    try {
      database.exec('PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS ledger (id INTEGER PRIMARY KEY CHECK(id=1), state TEXT NOT NULL); BEGIN IMMEDIATE;');
      const row = database.prepare('SELECT state FROM ledger WHERE id=1').get() as { state: string } | undefined;
      const state = validateState(row ? JSON.parse(row.state) : { version: 1, sources: {} });
      const result = await change(state);
      database.prepare('INSERT INTO ledger(id,state) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state').run(JSON.stringify(validateState(state)));
      database.exec('COMMIT');
      return result;
    } catch (error) {
      if (database.isTransaction) database.exec('ROLLBACK');
      throw error;
    } finally {
      database.close();
    }
  }
  async activate(harness: Harness, root: string, at = new Date().toISOString(), exclusions: string[] = []): Promise<SourceState> {
    if (!root || /[\u0000-\u001f]/.test(root)) throw new Error('Select an explicit valid source root');
    exclusions = [...new Set(exclusions.map(path => relativePath(path.replaceAll('\\', '/').replace(/^\.\//, '').replace(/\/+$/, ''))))].sort();
    return this.transaction(state => {
      const existing = state.sources[harness];
      if (existing) {
        if (existing.root !== resolve(root)) throw new Error('Source root is immutable; use a separate protected state directory for another installation');
        if (JSON.stringify(existing.exclusions) !== JSON.stringify([...new Set(exclusions)].sort())) throw new Error('Exclusions are immutable after activation; restore the original selection');
        return existing;
      }
      const cutoff = new Date(at).toISOString();
      return state.sources[harness] = { root: resolve(root), device_id: randomUUID(), cutoff, exclusions, pauses: [], records: {}, excluded_ids: [], superseded_ids: [], identity_aliases: [], material_alias_ids: [], rejected_observations: {}, conflicting_ids: [], progress: { scans: 0, observations: 0, diagnostics: {}, at: null } };
    });
  }
  async record(harness: Harness, result: AdapterResult, at = new Date().toISOString()): Promise<{ duplicates: number; excluded: number; rejected: number }> {
    return this.transaction(state => {
      const source = state.sources[harness];
      if (!source) throw new Error('Activate source first');
      const excluded = new Set(source.excluded_ids);
      const superseded = new Set(source.superseded_ids);
      const conflicting = new Set([...source.conflicting_ids, ...(result.conflictingCanonicalIds ?? [])]);
      source.conflicting_ids = [...conflicting].sort();
      for (const key of conflicting) if (Object.hasOwn(source.records, key)) source.records[key].diagnostics = [...new Set([...source.records[key].diagnostics, 'CONFLICTING_RESPONSE'])].sort();
      const aliases = new Map(source.identity_aliases.map(alias => [alias.from, alias.to]));
      const targets = new Set(aliases.values());
      const material = new Set(source.material_alias_ids);
      // A request naming a second response is skipped, never merged: both responses stay separate and the durable mapping wins.
      const aliasConflicts = new Set<string>();
      let skippedAliases = 0;
      for (const alias of result.identityAliases ?? []) {
        if (typeof alias.from !== 'string' || typeof alias.to !== 'string' || !alias.from || !alias.to || alias.from === alias.to || alias.from.length > 512 || alias.to.length > 512 || aliases.has(alias.to) || targets.has(alias.from)) throw new Error('Invalid identity alias');
        const existing = aliases.get(alias.from);
        if (existing === alias.to) continue;
        if (existing !== undefined) { skippedAliases++; aliasConflicts.add(existing); aliasConflicts.add(alias.to); continue; }
        const previous = Object.hasOwn(source.records, alias.from) ? source.records[alias.from] : undefined;
        if (previous && previous.identity_strength !== 'request') throw new Error('Only request identities may upgrade to response identities');
        aliases.set(alias.from, alias.to);
        targets.add(alias.to);
        if (excluded.has(alias.from)) excluded.add(alias.to);
        if (previous) {
          material.add(alias.from);
          const upgraded = { ...previous, canonical_id: alias.to, identity_strength: 'response' as const };
          Object.defineProperty(source.records, alias.to, { value: Object.hasOwn(source.records, alias.to) ? reconcile(source.records[alias.to], upgraded) : upgraded, enumerable: true, writable: true, configurable: true });
          delete source.records[alias.from];
        }
      }
      for (const key of result.supersededCanonicalIds ?? []) {
        const previous = Object.hasOwn(source.records, key) ? source.records[key] : undefined;
        if (previous && previous.grain !== 'reconstructed_interval') throw new Error('Only reconstructed intervals may be superseded');
        if (typeof key !== 'string' || !key || key.length > 512) throw new Error('Invalid superseded canonical ID');
        superseded.add(key);
        delete source.records[key];
      }
      let duplicates = 0, invalid = 0;
      for (const input of result.observations) {
        let observation: Observation;
        try { observation = metadata(input); }
        catch (error) {
          // One malformed source row must not block every later scan: reject it alone, durably, and never upload it.
          if (!(error instanceof MetadataError)) throw error;
          invalid++;
          const identity = createHash('sha256').update(typeof input.canonical_id === 'string' ? input.canonical_id : String(JSON.stringify(input.canonical_id))).digest('hex');
          Object.defineProperty(source.rejected_observations, identity, { value: error.field, enumerable: true, writable: true, configurable: true });
          continue;
        }
        if (observation.harness !== harness) throw new Error('Adapter returned another harness');
        const from = observation.canonical_id;
        const target = aliases.get(from);
        const merged = target !== undefined && observation.identity_strength === 'request';
        if (target) { observation.canonical_id = target; observation.identity_strength = 'response'; }
        const key = observation.canonical_id;
        if (conflicting.has(key)) observation.diagnostics = [...new Set([...observation.diagnostics, 'CONFLICTING_RESPONSE'])].sort();
        if (superseded.has(key)) continue;
        const time = observation.timestamp === null ? null : Date.parse(observation.timestamp);
        const start = Date.parse(observation.interval_start ?? observation.timestamp ?? '');
        const end = Date.parse(observation.interval_end ?? observation.timestamp ?? '');
        const paused = source.pauses.some(pause => end >= Date.parse(pause.start) && (pause.end === null || start < Date.parse(pause.end)));
        if (excluded.has(key) || (time !== null && paused)) { excluded.add(key); delete source.records[key]; continue; }
        if (merged) material.add(from);
        if (Object.hasOwn(source.records, key)) { duplicates++; source.records[key] = reconcile(source.records[key], observation); }
        else Object.defineProperty(source.records, key, { value: observation, enumerable: true, writable: true, configurable: true });
      }
      for (const key of aliasConflicts) if (Object.hasOwn(source.records, key)) source.records[key].diagnostics = [...new Set([...source.records[key].diagnostics, 'IDENTITY_ALIAS_CONFLICT'])].sort();
      source.excluded_ids = [...excluded].sort();
      source.superseded_ids = [...superseded].sort();
      source.identity_aliases = [...aliases.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([from, to]) => ({ from, to }));
      source.material_alias_ids = [...material].sort();
      source.cursor = undefined;
      if (result.cursor) {
        const { file, offset, line, device, inode, discardLine } = result.cursor;
        relativePath(file);
        if ([offset, line, device, inode].some(value => !Number.isSafeInteger(value) || value < 0) || (discardLine !== undefined && typeof discardLine !== 'boolean')) throw new Error('Invalid scan cursor');
        source.cursor = { file, offset, line, device, inode, ...(discardLine === undefined ? {} : { discardLine }) };
      }
      source.continuation = undefined;
      if (result.continuation?.nextFile) {
        const nextFile = result.continuation.nextFile;
        relativePath(nextFile);
        source.continuation = { nextFile };
      }
      const diagnostics: Record<string, number> = {};
      for (const diagnostic of result.diagnostics) if (/^[A-Z][A-Z0-9_]{0,95}$/.test(diagnostic.code)) diagnostics[diagnostic.code] = (diagnostics[diagnostic.code] ?? 0) + 1;
      if (skippedAliases > 0) diagnostics.IDENTITY_ALIAS_CONFLICT = (diagnostics.IDENTITY_ALIAS_CONFLICT ?? 0) + skippedAliases;
      if (invalid > 0) diagnostics.INVALID_OBSERVATION = (diagnostics.INVALID_OBSERVATION ?? 0) + invalid;
      source.progress = { scans: source.progress.scans + 1, observations: result.observations.length, diagnostics, at: new Date(at).toISOString() };
      return { duplicates, excluded: excluded.size, rejected: invalid };
    });
  }
  async pause(harness: Harness, paused: boolean, at = new Date().toISOString()): Promise<void> {
    await this.transaction(state => {
      const source = state.sources[harness];
      if (!source) throw new Error('Activate source first');
      const openPause = source.pauses.find(pause => pause.end === null);
      if (paused && !openPause) source.pauses.push({ start: new Date(at).toISOString(), end: null });
      if (!paused && openPause) {
        if (Date.parse(at) < Date.parse(openPause.start)) throw new Error('Clock moved backwards during pause');
        openPause.end = new Date(at).toISOString();
      }
    });
  }
}
