import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { Ledger } from '../src/ledger.ts';
import { aggregate, attributionIssue } from '../src/aggregate.ts';
import { metadata, reconcile, outbound } from '../src/metadata.ts';
import type { Observation } from '../src/observations.ts';
const fixture: Observation = JSON.parse(await readFile(new URL('./fixtures/observation.json', import.meta.url), 'utf8'));

 test('canonical replay, child work and cleanup retain one contribution across restart', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-ledger-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  const selected = await ledger.activate('codex', '/explicit/synthetic-root', '2026-01-02T00:00:00Z', ['sessions/private']);
  await ledger.record('codex', { observations: [fixture, fixture, { ...fixture, canonical_id: 'synthetic-child', is_subagent: true, parent_session_id: fixture.session_id }], diagnostics: [] });
  await new Ledger(directory).record('codex', { observations: [], diagnostics: [{ code: 'DEFERRED_LINE', message: 'Partial line deferred' }], continuation: { nextFile: 'sessions/later.jsonl' } });
  const source = await ledger.transaction(state => state.sources.codex!);
  assert.equal(Object.keys(source.records).length, 2);
  assert.equal(source.progress.scans, 2);
  assert.equal(source.continuation?.nextFile, 'sessions/later.jsonl');
  assert.equal(source.device_id, selected.device_id);
  assert.equal((await ledger.activate('codex', '/explicit/synthetic-root', '2026-03-01T00:00:00Z', ['sessions/private'])).cutoff, '2026-01-02T00:00:00.000Z');
  await assert.rejects(ledger.activate('codex', '/different/root'), /immutable/);
  await assert.rejects(ledger.activate('codex', '/explicit/synthetic-root', undefined, []), /Exclusions are immutable/);
  assert.equal((await stat(join(directory, 'ledger.sqlite'))).mode & 0o777, 0o600);
  assert.equal((await stat(directory)).mode & 0o777, 0o700);
 });

test('metadata, progress and cursor roll back atomically on a rejected scan', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-rollback-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/explicit/root');
  await assert.rejects(ledger.record('codex', { observations: [fixture, { ...fixture, canonical_id: 'foreign', harness: 'claude-code' }], diagnostics: [], continuation: { nextFile: 'later' } }), /another harness/);
  const source = await new Ledger(directory).transaction(state => state.sources.codex!);
  assert.deepEqual(source.records, {});
  assert.equal(source.progress.scans, 0);
  assert.equal(source.continuation, undefined);
 });

test('one invalid observation is rejected alone with a durable field diagnostic while the scan commits', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-invalid-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/explicit/root');
  const model = `synthetic-overlong-model-${'x'.repeat(200)}`;
  const invalid = { ...fixture, canonical_id: 'synthetic-invalid', model };
  const first = await ledger.record('codex', { observations: [invalid, fixture], diagnostics: [], continuation: { nextFile: 'later' } });
  assert.equal(first.rejected, 1);
  let source = await new Ledger(directory).transaction(state => state.sources.codex!);
  assert.deepEqual(Object.keys(source.records), [fixture.canonical_id]);
  assert.deepEqual(Object.values(source.rejected_observations), ['model']);
  assert.equal(source.progress.diagnostics.INVALID_OBSERVATION, 1);
  assert.equal(source.continuation?.nextFile, 'later');
  assert.ok(!JSON.stringify(source).includes(model) && !JSON.stringify(source).includes('synthetic-invalid'), 'no raw rejected values persist');
  const second = await ledger.record('codex', { observations: [invalid, { ...fixture, canonical_id: 'synthetic-next' }, { ...fixture, canonical_id: 'synthetic-before-epoch', timestamp: '1969-12-31T23:59:59.000Z' }], diagnostics: [] });
  assert.equal(second.rejected, 2);
  source = await ledger.transaction(state => state.sources.codex!);
  assert.deepEqual(Object.keys(source.records).sort(), [fixture.canonical_id, 'synthetic-next'].sort());
  assert.deepEqual(Object.values(source.rejected_observations).sort(), ['model', 'timestamp'], 'replayed rejections stay idempotent');
  assert.equal(source.progress.scans, 2);
});

test('a request naming a second response skips that alias durably and keeps both responses', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-alias-conflict-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('claude-code', '/chosen/root');
  const claude = { ...fixture, harness: 'claude-code' as const };
  await ledger.record('claude-code', { observations: [{ ...claude, canonical_id: 'request:R', identity_strength: 'request' }], diagnostics: [] });
  await ledger.record('claude-code', { observations: [{ ...claude, canonical_id: 'message:A' }], diagnostics: [], identityAliases: [{ from: 'request:R', to: 'message:A' }] });
  const conflicting = await ledger.record('claude-code', { observations: [{ ...claude, canonical_id: 'message:B', input_tokens: 999 }], diagnostics: [{ code: 'REQUEST_ID_CONFLICT', message: 'synthetic' }], identityAliases: [{ from: 'request:R', to: 'message:B' }] });
  assert.equal(conflicting.duplicates, 0, 'distinct responses never merge');
  const source = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.equal(source.progress.scans, 3, 'the conflicting scan commits');
  assert.deepEqual(Object.keys(source.records).sort(), ['message:A', 'message:B']);
  assert.equal(source.records['message:B']!.input_tokens, 999);
  assert.deepEqual(source.identity_aliases, [{ from: 'request:R', to: 'message:A' }]);
  assert.deepEqual(source.material_alias_ids, ['request:R']);
  assert.ok(source.records['message:A']!.diagnostics.includes('IDENTITY_ALIAS_CONFLICT'));
  assert.ok(source.records['message:B']!.diagnostics.includes('IDENTITY_ALIAS_CONFLICT'));
  assert.equal(source.progress.diagnostics.IDENTITY_ALIAS_CONFLICT, 1);
  await ledger.record('claude-code', { observations: [{ ...claude, canonical_id: 'request:R', identity_strength: 'request' }], diagnostics: [] });
  const replayed = await ledger.transaction(state => state.sources['claude-code']!);
  assert.deepEqual(Object.keys(replayed.records).sort(), ['message:A', 'message:B'], 'a late fallback follows the durable mapping only');
  assert.equal(replayed.records['message:B']!.input_tokens, 999);
});

 test('original UTC timestamps partition history/live without overlap and preserve unknowns', () => {
  const cutoff = '2026-01-02T00:00:00.000Z';
  const live = { ...fixture, canonical_id: 'at-cutoff', timestamp: cutoff };
  const summary = { ...fixture, canonical_id: 'summary', grain: 'session_summary' as const, quality: 'summary' as const, cost_usd: 1, cost_basis: 'estimated' as const };
  const result = aggregate([fixture, fixture, live, summary], cutoff);
  assert.equal(result.days.length, 2);
  assert.equal(result.days[0].input_tokens, 80);
  assert.equal(result.days[0].output_tokens, null);
  assert.equal(result.days[0].call_count, null);
  assert.equal(result.days[0].cost_usd, null);
  assert.equal(result.days[0].known_subtotals.output_tokens, 0);
  assert.equal(result.withheld.NONADDITIVE_SUMMARY, 1);
  assert.equal([fixture, live].filter(value => value.timestamp! < cutoff).length, 1);
  assert.equal([fixture, live].filter(value => value.timestamp! >= cutoff).length, 1);
  assert.equal(attributionIssue({ ...fixture, grain: 'reconstructed_interval', interval_start: fixture.timestamp, interval_end: cutoff }, cutoff), 'AMBIGUOUS_INTERVAL_DAY');
  assert.equal(attributionIssue({ ...fixture, grain: 'reconstructed_interval', interval_start: '2026-01-01T12:00:00Z', interval_end: '2026-01-01T14:00:00Z' }, '2026-01-01T13:00:00Z'), 'INTERVAL_STRADDLES_CUTOFF');
 });

 test('persistent source-isolated pause excludes late-flushed calls after restart and resume', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-pause-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/chosen/codex', '2026-01-01T00:00:00Z');
  await ledger.activate('claude-code', '/chosen/claude', '2026-01-01T00:00:00Z');
  await ledger.pause('codex', true, '2026-01-01T12:00:00Z');
  await new Ledger(directory).pause('codex', false, '2026-01-02T00:00:00Z');
  await ledger.record('codex', { observations: [fixture], diagnostics: [] });
  await ledger.record('claude-code', { observations: [{ ...fixture, harness: 'claude-code' }], diagnostics: [] });
  const state = await ledger.transaction(state => state);
  assert.equal(Object.keys(state.sources.codex!.records).length, 0);
  assert.deepEqual(state.sources.codex!.excluded_ids, [fixture.canonical_id]);
  assert.equal(Object.keys(state.sources['claude-code']!.records).length, 1);
  assert.notEqual(state.sources.codex!.device_id, state.sources['claude-code']!.device_id);
 });

 test('reconciliation is deterministic, metadata allowlisted and null never zeroed', () => {
  const richer = { ...fixture, output_tokens: 30, call_count: 1, quality: 'observed' as const };
  assert.deepEqual(reconcile(fixture, richer), reconcile(richer, fixture));
  assert.equal(reconcile(fixture, richer).output_tokens, 30);
  const poisoned = Object.assign({}, fixture, { prompt: 'SECRET_RAW_PROMPT', file: '/SECRET_PATH' });
  assert.equal(JSON.stringify(metadata(poisoned)).includes('SECRET'), false);
  assert.equal('diagnostics' in outbound(poisoned), false);
  assert.equal(outbound(fixture).cost_usd, null);
  for (const value of [1e9 + 1, Number.MAX_SAFE_INTEGER + 1, -1, 0.5]) assert.throws(() => metadata({ ...fixture, input_tokens: value }), /Invalid input_tokens/);
 });

 test('request identity upgrade and evidence-based legacy supersession survive replay', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-reconcile-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/chosen/root');
  const request = { ...fixture, canonical_id: 'request-key', identity_strength: 'request' as const };
  const interval = { ...fixture, canonical_id: 'interval-key', identity_strength: 'scoped' as const, grain: 'reconstructed_interval' as const, quality: 'reconstructed' as const };
  await ledger.record('codex', { observations: [request, interval], diagnostics: [] });
  await ledger.record('codex', { observations: [{ ...fixture, canonical_id: 'response-key' }], diagnostics: [], identityAliases: [{ from: 'request-key', to: 'response-key' }], supersededCanonicalIds: ['interval-key'] });
  await new Ledger(directory).record('codex', { observations: [request, interval], diagnostics: [] });
  const source = await ledger.transaction(state => state.sources.codex!);
  assert.deepEqual(Object.keys(source.records), ['response-key']);
  assert.deepEqual(source.superseded_ids, ['interval-key']);
  assert.deepEqual(source.identity_aliases, [{ from: 'request-key', to: 'response-key' }]);
  assert.deepEqual(source.material_alias_ids, ['request-key']);
  await assert.rejects(ledger.record('codex', { observations: [], diagnostics: [], supersededCanonicalIds: ['response-key'] }), /Only reconstructed/);
 });

test('process termination inside transaction leaves no partial metadata or cursor', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'tokenmunchers-crash-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const ledger = new Ledger(directory);
  await ledger.activate('codex', '/explicit/root');
  const moduleUrl = new URL('../src/ledger.ts', import.meta.url).href;
  const program = `import {Ledger} from ${JSON.stringify(moduleUrl)}; const ledger = new Ledger(${JSON.stringify(directory)}); await ledger.transaction(state => { state.sources.codex.progress.scans = 99; state.sources.codex.continuation = {nextFile:'lost'}; process.exit(23); });`;
  const child = spawnSync(process.execPath, ['--input-type=module', '-e', program], { encoding: 'utf8' });
  assert.equal(child.status, 23, child.stderr);
  const source = await new Ledger(directory).transaction(state => state.sources.codex!);
  assert.equal(source.progress.scans, 0);
  assert.equal(source.continuation, undefined);
});
