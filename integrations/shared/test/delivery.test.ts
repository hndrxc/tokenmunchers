import test, { type TestContext } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { DatabaseSync } from 'node:sqlite';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Ledger } from '../src/ledger.ts';
import { deliver } from '../src/delivery.ts';
import type { Observation } from '../src/observations.ts';
const fixture: Observation = JSON.parse(await readFile(new URL('./fixtures/observation.json', import.meta.url), 'utf8'));
const endpointUrl = 'http://localhost/functions/v1/contributions';
type ChunkBody = { observations: Observation[]; superseded_canonical_ids: string[]; identity_aliases: Array<{ from: string; to: string }> };

/** Synthetic endpoint modelled on publish_contributions: 10 of 600 units per request per UTC minute (limited requests included), idempotent staged chunks. */
function stagedEndpoint(clock: { now: number }, hooks: { before?: (body: Record<string, unknown>) => void; limited?: (body: Record<string, unknown>) => boolean } = {}) {
  const hits = new Map<number, number>();
  const staged = new Map<number, string>();
  const log: Array<[string, number | undefined, number]> = [];
  let chunkCount: number | null = null;
  let published = 0;
  const fetchImpl: typeof fetch = async (_input, init) => {
    clock.now += 50;
    const body = JSON.parse(String(init?.body));
    hooks.before?.(body);
    const window = Math.floor(clock.now / 60_000);
    const used = (hits.get(window) ?? 0) + 10;
    hits.set(window, used);
    if (used > 600 || hooks.limited?.(body)) {
      const seconds = Math.max(1, Math.ceil(((window + 1) * 60_000 - clock.now) / 1000));
      log.push([body.action, body.index, 429]);
      return Response.json({ error: 'rate_limited', retry_after_seconds: seconds }, { status: 429, headers: { 'Retry-After': String(seconds) } });
    }
    if (body.action !== 'begin' && chunkCount === null) {
      log.push([body.action, body.index, 409]);
      return Response.json({ error: 'snapshot_conflict' }, { status: 409 });
    }
    log.push([body.action, body.index, 200]);
    if (body.action === 'begin') chunkCount = body.chunk_count;
    if (body.action === 'chunk') {
      const encoded = JSON.stringify(body.observations);
      if (staged.has(body.index)) assert.equal(staged.get(body.index), encoded);
      staged.set(body.index, encoded);
    }
    if (body.action === 'commit') {
      assert.equal(staged.size, chunkCount);
      published = [...staged.values()].reduce((sum, value) => sum + JSON.parse(value).length, 0);
    }
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, ...(body.action === 'commit' ? { committed: true, accepted: published } : {}) });
  };
  return { fetchImpl, log, published: () => published, chunkCount: () => chunkCount, expire: () => { staged.clear(); chunkCount = null; } };
}
async function largeLedger(t: TestContext, count: number): Promise<{ directory: string; ledger: Ledger }> {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-staged-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root', '2026-01-02T00:00:00Z');
  await ledger.record('codex', { observations: Array.from({ length: count }, (_, index) => ({ ...fixture, canonical_id: `synthetic-staged-${String(index).padStart(5, '0')}` })), diagnostics: [] });
  return { directory, ledger };
}

 test('interrupted staged snapshot leaves last-good totals; restart resumes at the failed request', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-delivery-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root', '2026-01-02T00:00:00Z');
  const observations = Array.from({ length: 501 }, (_, index) => ({ ...fixture, canonical_id: `response-${index}` }));
  await ledger.record('codex', { observations, diagnostics: [] });
  const requests: Array<Record<string, unknown>> = [];
  const chunks = new Map<number, unknown[]>();
  let published = 7;
  let interrupt = true;
  const fetchImpl: typeof fetch = async (_input, init) => {
    assert.equal(new Headers(init?.headers).get('authorization'), 'Bearer tm_synthetic');
    const encoded = String(init?.body);
    assert.ok(Buffer.byteLength(encoded) <= 256 * 1024);
    assert.equal(encoded.includes('diagnostics'), false);
    assert.equal(encoded.includes('/selected/root'), false);
    const body = JSON.parse(encoded);
    requests.push(body);
    if (body.action === 'chunk') {
      if (body.index === 1 && interrupt) { interrupt = false; throw new Error('Simulated interruption'); }
      assert.ok(body.observations.length <= 500);
      const previous = chunks.get(body.index);
      if (previous) assert.deepEqual(previous, body.observations);
      chunks.set(body.index, body.observations);
    }
    if (body.action === 'commit') published = [...chunks.values()].flat().length;
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, ...(body.action === 'commit' ? { accepted: published, committed: true } : {}) });
  };
  const options = { endpoint: 'http://localhost/functions/v1/contributions', token: 'tm_synthetic', confirmed: true, fetchImpl, attempts: 1 };
  await assert.rejects(deliver(ledger, 'codex', options), /Network delivery failed/);
  const firstRun = requests.length;
  assert.equal(published, 7);
  const firstSnapshot = requests[0].snapshot_id;
  await ledger.record('codex', { observations: [], diagnostics: [] }); // source cleanup
  const resumed = await deliver(new Ledger(directory), 'codex', options);
  assert.equal(resumed.snapshot_id, firstSnapshot);
  assert.equal(resumed.resumed_at_request, 2);
  assert.deepEqual(requests.slice(firstRun).map(body => [body.action, body.index]), [['chunk', 1], ['commit', undefined]]);
  assert.equal(resumed.accepted, 501);
  assert.equal(published, 501);
  assert.equal((await ledger.transaction(state => state.sources.codex!)).pending, undefined);
 });

 test('lost commit acknowledgment retains snapshot and replay is harmless', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-ack-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root', '2026-01-02T00:00:00Z');
  await ledger.record('codex', { observations: [fixture], diagnostics: [] });
  const accepted = new Set<string>();
  let fail = true;
  const fetchImpl: typeof fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body));
    if (body.action === 'commit') {
      accepted.add(body.snapshot_id);
      if (fail) { fail = false; throw new Error('Lost ACK'); }
    }
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, committed: true, accepted: 1 });
  };
  const options = { endpoint: 'http://localhost/functions/v1/contributions', token: 'tm_test', confirmed: true, fetchImpl, attempts: 1 };
  await assert.rejects(deliver(ledger, 'codex', options));
  await deliver(new Ledger(directory), 'codex', options);
  assert.equal(accepted.size, 1);
 });

 test('no upload without confirmation, paused source, unauthorized key or ownership conflict', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-guard-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root');
  let calls = 0;
  let status = 401;
  const fetchImpl: typeof fetch = async () => { calls++; return new Response('{}', { status }); };
  const options = { endpoint: 'http://localhost/functions/v1/contributions', token: 'tm_test', confirmed: false, fetchImpl };
  await assert.rejects(deliver(ledger, 'codex', options), /explicit/);
  assert.equal(calls, 0);
  await ledger.pause('codex', true);
  await assert.rejects(deliver(ledger, 'codex', { ...options, confirmed: true }), /paused/);
  assert.equal(calls, 0);
  await ledger.pause('codex', false);
  await assert.rejects(deliver(ledger, 'codex', { ...options, confirmed: true }), /unauthorized/);
  status = 409;
  await assert.rejects(deliver(ledger, 'codex', { ...options, confirmed: true }), /ownership/);
  assert.equal(calls, 2);
 });

test('full snapshots carry explicit supersessions and only material identity upgrades', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-tombstones-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root', '2026-01-02T00:00:00Z');
  // Fallback recorded before its response: the alias must reach the server to supersede a possibly uploaded request row.
  await ledger.record('codex', { observations: [{ ...fixture, canonical_id: 'request-fallback', identity_strength: 'request' }], diagnostics: [] });
  await ledger.record('codex', {
    observations: [fixture, { ...fixture, canonical_id: 'response-late' }, { ...fixture, canonical_id: 'response-only' }], diagnostics: [], supersededCanonicalIds: ['old-interval'],
    identityAliases: [{ from: 'request-fallback', to: fixture.canonical_id }, { from: 'request-late', to: 'response-late' }, { from: 'request-only', to: 'response-only' }],
  });
  // A late fallback merged through a durable alias makes that alias material too; it never resurrects as a request row.
  await ledger.record('codex', { observations: [{ ...fixture, canonical_id: 'request-late', identity_strength: 'request' }], diagnostics: [] });
  const chunks: ChunkBody[] = [];
  const fetchImpl: typeof fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body));
    if (body.action === 'chunk') chunks.push(body);
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, committed: true, accepted: 1 });
  };
  await deliver(ledger, 'codex', { endpoint: 'http://localhost/functions/v1/contributions', token: 'tm_test', confirmed: true, fetchImpl });
  assert.deepEqual(chunks[0].superseded_canonical_ids, ['old-interval']);
  assert.deepEqual(chunks[0].observations.map(row => [row.canonical_id, row.identity_strength]), [['response-late', 'response'], ['response-only', 'response'], [fixture.canonical_id, 'response']]);
  assert.deepEqual(chunks[0].identity_aliases, [{ from: 'request-fallback', to: fixture.canonical_id }, { from: 'request-late', to: 'response-late' }]);
  const source = await ledger.transaction(state => state.sources.codex!);
  assert.deepEqual(source.identity_aliases.map(alias => alias.from), ['request-fallback', 'request-late', 'request-only'], 'response-only aliases stay durable locally');
});

test('response-only aliases at scale upload only the material alias subset', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-aliases-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('claude-code', '/selected/root', '2026-01-02T00:00:00Z');
  const claude = { ...fixture, harness: 'claude-code' as const };
  const count = 3000;
  const fallbacks = [7, 1500, 2999];
  await ledger.record('claude-code', { observations: fallbacks.map(index => ({ ...claude, canonical_id: `request:${index}`, identity_strength: 'request' as const })), diagnostics: [] });
  await ledger.record('claude-code', {
    observations: Array.from({ length: count }, (_, index) => ({ ...claude, canonical_id: `message:${index}` })), diagnostics: [],
    identityAliases: Array.from({ length: count }, (_, index) => ({ from: `request:${index}`, to: `message:${index}` })),
  });
  const source = await ledger.transaction(state => state.sources['claude-code']!);
  assert.equal(source.identity_aliases.length, count);
  assert.deepEqual(source.material_alias_ids, fallbacks.map(index => `request:${index}`).sort());
  const chunks: ChunkBody[] = [];
  const fetchImpl: typeof fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body));
    if (body.action === 'chunk') chunks.push(body);
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, committed: true, accepted: count });
  };
  await deliver(ledger, 'claude-code', { endpoint: endpointUrl, token: 'tm_test', confirmed: true, fetchImpl });
  const uploaded = chunks.flatMap(chunk => chunk.identity_aliases);
  assert.equal(uploaded.length, source.material_alias_ids.length);
  assert.deepEqual(uploaded.map(alias => alias.from), source.material_alias_ids);
  assert.equal(chunks.flatMap(chunk => chunk.observations).length, count);
  assert.equal(chunks.length, Math.ceil((count + fallbacks.length) / 500));
});

test('legacy ledger state without alias materiality or rejections loads and keeps uploading its aliases', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-legacy-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/selected/root', '2026-01-02T00:00:00Z');
  await ledger.record('codex', { observations: [fixture], diagnostics: [], identityAliases: [{ from: 'request-legacy', to: fixture.canonical_id }] });
  const database = new DatabaseSync(join(directory, 'ledger.sqlite'));
  const row = database.prepare('SELECT state FROM ledger WHERE id=1').get();
  assert.ok(row && typeof row.state === 'string');
  const legacy = JSON.parse(row.state);
  delete legacy.sources.codex.material_alias_ids;
  delete legacy.sources.codex.rejected_observations;
  database.prepare('UPDATE ledger SET state=? WHERE id=1').run(JSON.stringify(legacy));
  database.close();
  const chunks: ChunkBody[] = [];
  const fetchImpl: typeof fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body));
    if (body.action === 'chunk') chunks.push(body);
    return Response.json({ ok: true, action: body.action, snapshot_id: body.snapshot_id, committed: true, accepted: 1 });
  };
  await deliver(ledger, 'codex', { endpoint: endpointUrl, token: 'tm_test', confirmed: true, fetchImpl });
  assert.deepEqual(chunks[0].identity_aliases, [{ from: 'request-legacy', to: fixture.canonical_id }], 'state that cannot prove materiality stays conservative');
  assert.deepEqual((await ledger.transaction(state => state.sources.codex!)).material_alias_ids, ['request-legacy']);
});

 test('begin plus 59 chunks crossing a rate reset waits once and commits without replaying the prefix', async t => {
  const { ledger } = await largeLedger(t, 59 * 500);
  const clock = { now: Date.parse('2026-01-02T00:00:30Z') };
  const endpoint = stagedEndpoint(clock);
  const sleeps: number[] = [];
  const sleep = async (milliseconds: number): Promise<void> => { sleeps.push(milliseconds); clock.now += milliseconds; };
  // attempts: 1 leaves no transient allowance, so success proves reset waits are budgeted separately.
  const result = await deliver(ledger, 'codex', { endpoint: endpointUrl, token: 'tm_synthetic', confirmed: true, fetchImpl: endpoint.fetchImpl, sleep, attempts: 1 });
  assert.equal(endpoint.chunkCount(), 59);
  assert.deepEqual(endpoint.log, [['begin', undefined, 200], ...Array.from({ length: 59 }, (_, index): [string, number, number] => ['chunk', index, 200]), ['commit', undefined, 429], ['commit', undefined, 200]]);
  assert.equal(sleeps.length, 1);
  assert.ok(sleeps[0] >= 1000 && sleeps[0] <= 60_250);
  assert.equal(Math.floor(clock.now / 60_000), Math.floor(Date.parse('2026-01-02T00:00:30Z') / 60_000) + 1);
  assert.deepEqual({ accepted: result.accepted, waits: result.rate_limit_waits, resumed: result.resumed_at_request }, { accepted: 29_500, waits: 1, resumed: 0 });
  assert.equal(endpoint.published(), 29_500);
  assert.equal((await ledger.transaction(state => state.sources.codex!)).pending, undefined);
 });

 test('persistent rate limiting stops at a hard bound and the next sync resumes at the limited request', async t => {
  const { directory, ledger } = await largeLedger(t, 501);
  const start = Date.parse('2026-01-02T00:00:10Z');
  const clock = { now: start };
  const saturatedUntil = start + 10 * 60_000; // Another installation sharing the key keeps the quota exhausted.
  const endpoint = stagedEndpoint(clock, { limited: body => body.action === 'chunk' && body.index === 1 && clock.now < saturatedUntil });
  const sleeps: number[] = [];
  const sleep = async (milliseconds: number): Promise<void> => { sleeps.push(milliseconds); clock.now += milliseconds; };
  const options = { endpoint: endpointUrl, token: 'tm_synthetic', confirmed: true, fetchImpl: endpoint.fetchImpl, sleep, attempts: 1 };
  await assert.rejects(deliver(ledger, 'codex', options), /rate limit persisted/);
  assert.equal(sleeps.length, 3);
  assert.ok(sleeps.every(milliseconds => milliseconds >= 1000 && milliseconds <= 60_250));
  assert.deepEqual(endpoint.log, [['begin', undefined, 200], ['chunk', 0, 200], ['chunk', 1, 429], ['chunk', 1, 429], ['chunk', 1, 429], ['chunk', 1, 429]]);
  assert.deepEqual((await ledger.transaction(state => state.sources.codex!)).pending?.acknowledged, { requests: 2, chunk_count: 2 });
  assert.equal(endpoint.published(), 0);
  clock.now = saturatedUntil;
  const before = endpoint.log.length;
  const resumed = await deliver(new Ledger(directory), 'codex', options);
  assert.deepEqual(endpoint.log.slice(before), [['chunk', 1, 200], ['commit', undefined, 200]]);
  assert.equal(resumed.resumed_at_request, 2);
  assert.equal(resumed.accepted, 501);
 });

 test('resume after server-side staging expiry replays once from begin', async t => {
  const { directory, ledger } = await largeLedger(t, 501);
  const clock = { now: Date.parse('2026-01-02T00:00:10Z') };
  let interrupt = true;
  const endpoint = stagedEndpoint(clock, { before: body => { if (body.action === 'chunk' && body.index === 1 && interrupt) { interrupt = false; throw new Error('Simulated interruption'); } } });
  const options = { endpoint: endpointUrl, token: 'tm_synthetic', confirmed: true, fetchImpl: endpoint.fetchImpl, attempts: 1 };
  await assert.rejects(deliver(ledger, 'codex', options), /Network delivery failed/);
  endpoint.expire();
  const before = endpoint.log.length;
  const resumed = await deliver(new Ledger(directory), 'codex', options);
  assert.deepEqual(endpoint.log.slice(before), [['chunk', 1, 409], ['begin', undefined, 200], ['chunk', 0, 200], ['chunk', 1, 200], ['commit', undefined, 200]]);
  assert.equal(resumed.accepted, 501);
 });

 test('staging_limit 429 without Retry-After is reported, not retried as a reset wait', async t => {
  const { ledger } = await largeLedger(t, 1);
  let calls = 0;
  const fetchImpl: typeof fetch = async () => { calls++; return Response.json({ error: 'staging_limit' }, { status: 429 }); };
  await assert.rejects(deliver(ledger, 'codex', { endpoint: endpointUrl, token: 'tm_synthetic', confirmed: true, fetchImpl, sleep: async () => {} }), /staging limit/);
  assert.equal(calls, 1);
 });
