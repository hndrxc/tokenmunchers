import { randomUUID } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import type { Harness } from './observations.ts';
import { Ledger } from './ledger.ts';
import { outbound, type Contribution } from './metadata.ts';
import { attributionIssue } from './aggregate.ts';

export interface DeliveryOptions {
  endpoint: string;
  token: string;
  confirmed: boolean;
  fetchImpl?: typeof fetch;
  sleep?: (milliseconds: number) => Promise<void>;
  /** Short transient-failure (network, 5xx, unlabelled 429) attempts per request. Rate-limit reset waits never consume them. */
  attempts?: number;
  /** Aborts in-flight requests and waits; acknowledged progress is persisted before the error propagates. */
  signal?: AbortSignal;
}
export interface DeliveryResult { accepted: number; snapshot_id: string; resumed_at_request: number; rate_limit_waits: number }
const maxBodyBytes = 256 * 1024;
const maxChunkEntries = 500;
/** Endpoint contract: every request costs 10 of a shared 600/minute per-key quota, so at most 60 requests fit one UTC minute window. */
const requestsPerWindow = 60;
const maxRetryAfterSeconds = 60;
const maxConsecutiveRateWaits = 3;
const resetMarginMs = 250;
/** The staged snapshot is gone server-side (expired after 7 days) or never matched; only meaningful when resuming past `begin`. */
class StagingMissing extends Error {}
function retryAfterSeconds(response: Response, body: Record<string, unknown>): number | null {
  const header = response.headers.get('retry-after');
  const value = header !== null && /^\d+$/.test(header.trim()) ? Number(header.trim()) : body.retry_after_seconds;
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 1 && value <= maxRetryAfterSeconds ? value : null;
}
async function errorBody(response: Response): Promise<Record<string, unknown>> {
  try {
    const value: unknown = await response.json();
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
  } catch { return {}; }
}
/** Stage the entire retention-safe ledger, never a currently retained-files replacement. */
export async function deliver(ledger: Ledger, harness: Harness, options: DeliveryOptions): Promise<DeliveryResult> {
  if (!options.confirmed) throw new Error('Upload requires explicit --confirm-upload after local preview');
  if (!options.token) throw new Error('Set TOKENMUNCHERS_API_KEY (not persisted)');
  const url = new URL(options.endpoint);
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname))) throw new Error('Delivery requires HTTPS (HTTP allowed for localhost smoke tests only)');
  const attempts = options.attempts ?? 4;
  if (!Number.isSafeInteger(attempts) || attempts < 1 || attempts > 10) throw new Error('Invalid delivery retry count');
  const source = await ledger.transaction(state => {
    const selected = state.sources[harness];
    if (!selected) throw new Error('Activate source first');
    if (selected.pauses.some(pause => pause.end === null)) throw new Error('Source paused; resume before delivery');
    if (!selected.pending) {
      const excluded = new Set(selected.excluded_ids);
      const observations = Object.values(selected.records).filter(observation => !attributionIssue(observation, selected.cutoff) && !excluded.has(observation.canonical_id)).sort((a, b) => a.canonical_id.localeCompare(b.canonical_id));
      const ids = new Set(observations.map(observation => observation.canonical_id));
      // Response-only aliases stay local: the server never saw their request identity from this device, and the durable
      // local alias already folds late fallbacks into the response before any upload.
      const material = new Set(selected.material_alias_ids);
      selected.pending = { snapshot_id: randomUUID(), observations, superseded_ids: [...selected.superseded_ids], identity_aliases: selected.identity_aliases.filter(alias => material.has(alias.from) && ids.has(alias.to)) };
    }
    return selected;
  });
  const pending = source.pending!;
  const base = { device_id: source.device_id, harness, snapshot_id: pending.snapshot_id };
  type Chunk = { observations: Contribution[]; superseded_canonical_ids: string[]; identity_aliases: { from: string; to: string }[] };
  const field = { observation: 'observations', superseded: 'superseded_canonical_ids', alias: 'identity_aliases' } as const;
  const entries = [
    ...pending.observations.map(observation => ({ kind: 'observation' as const, value: outbound(observation) })),
    ...pending.superseded_ids.map(id => ({ kind: 'superseded' as const, value: id })),
    ...pending.identity_aliases.map(alias => ({ kind: 'alias' as const, value: alias })),
  ];
  // Deterministic partition (resume depends on it). Request bytes are tracked incrementally: arrays serialize inline, so
  // an empty chunk envelope plus each entry and its separating comma is the exact encoded size.
  const chunks: Chunk[] = [];
  let chunk: Chunk | undefined;
  let count = 0, bytes = 0;
  for (const entry of entries) {
    const size = Buffer.byteLength(JSON.stringify(entry.value), 'utf8');
    const added = chunk ? size + (chunk[field[entry.kind]].length > 0 ? 1 : 0) : 0;
    if (!chunk || count + 1 > maxChunkEntries || bytes + added > maxBodyBytes) {
      chunk = { observations: [], superseded_canonical_ids: [], identity_aliases: [] };
      chunks.push(chunk);
      count = 0;
      bytes = Buffer.byteLength(JSON.stringify({ action: 'chunk', ...base, index: chunks.length - 1, ...chunk }), 'utf8') + size;
      if (bytes > maxBodyBytes) throw new Error('One contribution exceeds the endpoint body limit');
    } else bytes += added;
    (chunk[field[entry.kind]] as unknown[]).push(entry.value);
    count++;
  }
  if (chunks.length > 1000) throw new Error('Snapshot exceeds 1000 chunks; endpoint capacity prerequisite required');
  const fetchImpl = options.fetchImpl ?? fetch;
  const sleep = options.sleep ?? ((milliseconds: number): Promise<void> => delay(milliseconds, undefined, { signal: options.signal }));
  const total = chunks.length + 2; // begin, chunks, commit
  // Hard bound: the windows this snapshot needs at 60 requests/minute, plus two windows of slack for traffic sharing the key.
  const rateWaitLimit = Math.ceil(total / requestsPerWindow) + 2;
  let rateWaits = 0;
  async function send(body: { action: string; [key: string]: unknown }, resumed: boolean): Promise<Record<string, unknown>> {
    const encoded = JSON.stringify(body);
    let failures = 0, consecutiveRateWaits = 0;
    for (;;) {
      options.signal?.throwIfAborted();
      let response: Response;
      try {
        const signal = options.signal ? AbortSignal.any([options.signal, AbortSignal.timeout(15_000)]) : AbortSignal.timeout(15_000);
        response = await fetchImpl(url, { method: 'POST', headers: { Authorization: `Bearer ${options.token}`, 'Content-Type': 'application/json' }, body: encoded, signal, redirect: 'error' });
      } catch {
        options.signal?.throwIfAborted();
        if (++failures >= attempts) throw new Error('Network delivery failed; staged snapshot retained for retry');
        await sleep(Math.min(30_000, 500 * 2 ** (failures - 1)));
        continue;
      }
      if (response.status === 401 || response.status === 403) throw new Error('Collector API key unauthorized; staged snapshot retained');
      if (response.status === 409 || response.status === 400) {
        const code = (await errorBody(response)).error;
        if (resumed && (code === 'snapshot_conflict' || code === 'incomplete_snapshot')) throw new StagingMissing(String(code));
        if (response.status === 409) throw new Error(`Collector ownership/cutoff/snapshot conflict (409${typeof code === 'string' ? ` ${code}` : ''}); do not reset device identity or silently import copied archives`);
        throw new Error(`Snapshot rejected (400${typeof code === 'string' ? ` ${code}` : ''}); staged snapshot retained`);
      }
      if (response.status === 429) {
        const result = await errorBody(response);
        if (result.error === 'staging_limit') throw new Error('Endpoint staging limit: too many uncommitted snapshots for this account; staged snapshot retained, sync again after other installations commit');
        const seconds = retryAfterSeconds(response, result);
        if (seconds !== null) {
          if (consecutiveRateWaits >= maxConsecutiveRateWaits || rateWaits >= rateWaitLimit) throw new Error(`Endpoint rate limit persisted after ${rateWaits} reset waits; staged snapshot retained and the next sync resumes at the limited request`);
          rateWaits++;
          consecutiveRateWaits++;
          await sleep(seconds * 1000 + resetMarginMs); // Reset waits do not consume the transient-failure allowance.
          continue;
        }
      }
      if (response.status === 429 || response.status >= 500) {
        if (++failures >= attempts) throw new Error(`Endpoint temporarily unavailable (${response.status}); retry same staged snapshot`);
        const retryAfter = Number(response.headers.get('retry-after'));
        await sleep(Number.isFinite(retryAfter) && retryAfter > 0 ? Math.min(30_000, retryAfter * 1000) : Math.min(30_000, 500 * 2 ** (failures - 1)));
        continue;
      }
      if (!response.ok) throw new Error(`Snapshot rejected (${response.status}); staged snapshot retained`);
      const result = await response.json() as Record<string, unknown>;
      if (result.ok !== true || result.snapshot_id !== pending.snapshot_id || result.action !== body.action) throw new Error('Invalid endpoint acknowledgment; staged snapshot retained');
      return result;
    }
  }
  // Resume after the last acknowledged request of this exact partition; staged chunks are idempotent server-side.
  const resumedAt = pending.acknowledged?.chunk_count === chunks.length ? pending.acknowledged.requests : 0;
  let acknowledged = resumedAt;
  let replayed = false;
  let committed: Record<string, unknown> | undefined;
  try {
    for (let step = acknowledged; step < total; step++) {
      const body = step === 0 ? { action: 'begin', ...base, cutoff: source.cutoff, chunk_count: chunks.length }
        : step <= chunks.length ? { action: 'chunk', ...base, index: step - 1, ...chunks[step - 1] }
          : { action: 'commit', ...base };
      let result: Record<string, unknown>;
      try { result = await send(body, !replayed && resumedAt > 0 && step === resumedAt); }
      catch (error) {
        if (!(error instanceof StagingMissing)) throw error;
        acknowledged = 0; // Server no longer holds the staged prefix (expired); replay once from begin.
        replayed = true;
        step = -1;
        continue;
      }
      if (step === total - 1) committed = result;
      else acknowledged = step + 1;
    }
  } catch (error) {
    try {
      await ledger.transaction(state => {
        const current = state.sources[harness]?.pending;
        if (current?.snapshot_id === pending.snapshot_id) current.acknowledged = { requests: acknowledged, chunk_count: chunks.length };
      });
    } catch { /* Progress is an optimization; the original delivery error is the actionable one. */ }
    throw error;
  }
  if (!committed || committed.committed !== true || !Number.isSafeInteger(committed.accepted) || (committed.accepted as number) < 0) throw new Error('Invalid commit acknowledgment; retry retained snapshot');
  await ledger.transaction(state => {
    const current = state.sources[harness]!;
    if (current.pending?.snapshot_id === pending.snapshot_id) { delete current.pending; current.delivered_at = new Date().toISOString(); }
  });
  return { accepted: committed.accepted as number, snapshot_id: pending.snapshot_id, resumed_at_request: resumedAt, rate_limit_waits: rateWaits };
}
