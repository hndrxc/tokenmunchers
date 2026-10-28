import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { deduplicate, RolloutParser } from '../src/parser.ts';
import { normalize, UsageError } from '../src/token-normalizer.ts';
import { meta, modern, row, snapshot, thread, usage } from './fixtures/builders.ts';
import type { Diagnostic } from '../../shared/src/observations.ts';

function parse(rows: unknown[]) {
  const parser = new RolloutParser('synthetic.jsonl');
  rows.forEach((value, index) => parser.consume(value, index + 1));
  return parser.finish();
}
const model = row('turn_context', { model: 'fixture-model' });

test('modern source-derived A=120 B=80 C=30 uses per-response usage, not cumulative totals', async () => {
  const fixture = await readFile(new URL('./fixtures/modern-source-derived.jsonl', import.meta.url), 'utf8');
  const parsed = parse(fixture.trim().split('\n').map(line => JSON.parse(line)));
  const diagnostics: Diagnostic[] = [];
  const result = deduplicate([...parsed.observations, ...parsed.observations], diagnostics);
  assert.deepEqual(result.map(item => item.input_tokens), [120, 80, 30]);
  assert.deepEqual(result.map(item => item.canonical_id), ['response-a', 'response-b', 'response-c']);
  assert.equal(result.reduce((sum, item) => sum + item.call_count!, 0), 3);
  assert.ok(result.every(item => item.grain === 'request' && item.cost_usd === null && item.provider === 'unknown'));
  assert.equal(diagnostics.filter(item => item.code === 'DUPLICATE_RESPONSE').length, 3);
  assert.equal(result[0].timestamp, '2026-10-15T10:00:01.000Z');
});

test('cache-write is disjoint, reasoning is a subset, absent serde-default write is zero', () => {
  assert.deepEqual(normalize(usage(100, 10, 40, 60, 5)), {
    input_tokens: 0, output_tokens: 10, cache_read_tokens: 40, cache_write_tokens: 60,
  });
  const old = { ...usage(100, 10, 40) } as Record<string, unknown>;
  delete old.cache_write_input_tokens;
  assert.equal(normalize(old).input_tokens, 60);
  assert.equal(Object.values(normalize(usage(100, 10, 40, 60))).reduce((sum, count) => sum + count, 0), 110);
});

test('malformed cache subsets, totals, reasoning, null and unsafe counts are rejected, never clamped', () => {
  for (const [source, code] of [
    [usage(5, 1, 4, 2), 'MALFORMED_CACHE_SUBSET'],
    [{ ...usage(10), total_tokens: 9 }, 'MALFORMED_TOTAL'],
    [usage(10, 1, 0, 0, 2), 'MALFORMED_REASONING_SUBSET'],
    [{ ...usage(10), output_tokens: null }, 'MALFORMED_USAGE'],
    [{ ...usage(10), input_tokens: -1 }, 'MALFORMED_USAGE'],
    [{ ...usage(10), input_tokens: Number.MAX_SAFE_INTEGER + 1 }, 'MALFORMED_USAGE'],
    [{ ...usage(10), cache_write_input_tokens: null }, 'MALFORMED_USAGE'],
  ] as const) assert.throws(() => normalize(source), (error: unknown) => error instanceof UsageError && error.code === code);
  const result = parse([meta(), modern('bad-cache', usage(1, 0, 2))]);
  assert.equal(result.observations.length, 0);
  assert.ok(result.diagnostics.some(item => item.code === 'MALFORMED_CACHE_SUBSET'));
});

test('legacy fixture discards 200000 context fill, retains recognized 110-token interval and unknown calls', async () => {
  const fixture = await readFile(new URL('./fixtures/legacy-synthetic.jsonl', import.meta.url), 'utf8');
  const result = parse(fixture.trim().split('\n').map(line => JSON.parse(line)));
  assert.equal(result.observations.length, 1);
  assert.deepEqual(result.observations.map(item => [item.input_tokens, item.cache_read_tokens, item.cache_write_tokens, item.output_tokens]), [[0, 40, 60, 10]]);
  assert.equal(result.observations[0].call_count, null);
  assert.equal(result.observations[0].quality, 'reconstructed');
  assert.equal(result.observations[0].provider, 'explicit-fixture-provider');
  assert.ok(result.diagnostics.some(item => item.code === 'SYNTHETIC_OVERFLOW'));
});

test('legacy repeats and counter drops reset safely; later new work remains countable', () => {
  const result = parse([meta(), model, snapshot(usage()), snapshot(usage(10), usage(10), 2),
    snapshot(usage(10), usage(10), 3), snapshot(usage(2), usage(2), 4), snapshot(usage(7), usage(5), 5)]);
  assert.deepEqual(result.observations.map(item => item.input_tokens), [10, 5]);
  assert.ok(result.diagnostics.some(item => item.code === 'REPEATED_SNAPSHOT'));
  assert.ok(result.diagnostics.some(item => item.code === 'COUNTER_RESET'));
});

test('unknown initial baseline, mismatched last delta, model changes and compaction never invent intervals', () => {
  const result = parse([meta(), model, snapshot(usage(100)), snapshot(usage(120), usage(5), 2),
    row('turn_context', { model: 'other-model' }, 3), snapshot(usage(140), usage(20), 4),
    row('compacted', { latest_token_usage_record: modern('embedded', usage(999)).payload }, 5),
    snapshot(usage(150), usage(10), 6), snapshot(usage(160), usage(10), 7)]);
  assert.deepEqual(result.observations.map(item => [item.input_tokens, item.model]), [[10, 'other-model']]);
  assert.ok(result.diagnostics.some(item => item.code === 'UNRECOGNIZED_INTERVAL'));
  assert.ok(result.diagnostics.some(item => item.code === 'MODEL_CHANGED'));
  assert.ok(result.diagnostics.some(item => item.code === 'COMPACTION_BOUNDARY'));
  assert.ok(!result.observations.some(item => item.canonical_id === 'embedded'));
});

test('mixed legacy/modern exact cumulative boundary retains proven prefix only', () => {
  const result = parse([meta(), model, snapshot(usage()), snapshot(usage(10), usage(10), 1),
    modern('new-modern', usage(5), usage(15), 2), snapshot(usage(15), usage(5), 3),
    snapshot(usage(19), usage(4), 4), modern('later-modern', usage(3), usage(22), 5)]);
  assert.deepEqual(result.observations.map(item => [item.grain, item.input_tokens]), [
    ['reconstructed_interval', 10], ['request', 5], ['reconstructed_interval', 4], ['request', 3],
  ]);
  assert.equal(result.diagnostics.filter(item => item.code === 'STITCH_CONFIRMED').length, 2);
  assert.equal(result.supersededCanonicalIds.length, 0);
  assert.deepEqual(result.segments.map(segment => segment.format), ['legacy', 'modern', 'legacy', 'modern']);
});

test('legacy snapshot already containing modern response is withheld and explicitly superseded', () => {
  const prefix = [meta(), model, snapshot(usage()), snapshot(usage(10), usage(10), 1)];
  const earlier = parse(prefix);
  assert.equal(earlier.observations.length, 1);
  const result = parse([...prefix, modern('overlapping-modern', usage(10), usage(10), 2)]);
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['overlapping-modern']);
  assert.deepEqual(result.supersededCanonicalIds, [earlier.observations[0].canonical_id]);
  assert.ok(result.diagnostics.some(item => item.code === 'UNCERTAIN_TRANSITION'));
});

test('transition with absent or invalid cumulative evidence skips legacy but retains exact modern usage', () => {
  const record = modern('no-cumulative-proof', usage(4), usage(14));
  const result = parse([meta(), snapshot(usage()), snapshot(usage(10), usage(10), 1),
    { ...record, payload: { ...record.payload, thread_token_usage: null } }]);
  assert.deepEqual(result.observations.map(item => item.input_tokens), [4]);
  assert.ok(result.diagnostics.some(item => item.code === 'MISSING_STITCH_EVIDENCE'));
  assert.equal(result.supersededCanonicalIds.length, 1);
});

test('fork prefix ownership skips legacy without a boundary while retaining genuinely new modern child work', () => {
  const child = '33333333-3333-4333-8333-333333333333';
  const inherited = modern('parent-response', usage(10));
  const own = modern('child-response', usage(5));
  const result = parse([meta({ id: child, forked_from_id: thread, parent_thread_id: thread }), model,
    snapshot(usage()), snapshot(usage(10), usage(10), 1), inherited,
    { ...own, payload: { ...own.payload, thread_id: child } }]);
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['child-response']);
  assert.equal(result.observations[0].is_subagent, true);
  assert.equal(result.observations[0].parent_session_id, thread);
  assert.ok(result.diagnostics.some(item => item.code === 'UNOWNED_FORK_INTERVAL'));
  assert.ok(result.diagnostics.some(item => item.code === 'INHERITED_RESPONSE'));
});

test('explicit child-local ordinal boundary prunes inherited legacy and retains new child intervals', () => {
  const result = parse([meta({ forked_from_id: 'parent', subagent_history_start_ordinal: 10 }), model,
    { ...snapshot(usage(90)), ordinal: 9 }, { ...snapshot(usage()), ordinal: 10 },
    { ...snapshot(usage(7), usage(7), 2), ordinal: 11 }, snapshot(usage(9), usage(2), 3)]);
  assert.deepEqual(result.observations.map(item => item.input_tokens), [7]);
  assert.ok(result.diagnostics.some(item => item.code === 'INHERITED_PREFIX'));
  assert.ok(result.diagnostics.some(item => item.code === 'MISSING_OWNERSHIP_ORDINAL'));
});

test('subagent lineage uses explicit source metadata; missing provider stays unknown without secret inspection', () => {
  const result = parse([meta({ source: { sub_agent: { thread_spawn: { parent_thread_id: 'parent', depth: 1 } } },
    authorization: 'DO-NOT-READ', base_instructions: { text: 'PRIVATE-PROMPT' } }), model,
    modern('subagent-response', usage(3, 2))]);
  assert.equal(result.observations[0].provider, 'unknown');
  assert.equal(result.observations[0].parent_session_id, 'parent');
  assert.equal(result.observations[0].is_subagent, true);
  assert.ok(!JSON.stringify(result).includes('DO-NOT-READ'));
  assert.ok(!JSON.stringify(result).includes('PRIVATE-PROMPT'));
});

test('strong identities deduplicate across sessions; conflicting accounting is visibly withheld', () => {
  const a = parse([meta(), modern('same-response', usage(5))]).observations[0];
  const diagnostics: Diagnostic[] = [];
  assert.equal(deduplicate([a, { ...a, session_id: 'fork-copy' }], diagnostics).length, 1);
  assert.equal(deduplicate([a, { ...a, input_tokens: 6 }], diagnostics).length, 0);
  assert.ok(diagnostics.some(item => item.code === 'CONFLICTING_RESPONSE'));
});

test('unknown timestamp is null, zero usage remains an observed response, and no-usage responses emit nothing', () => {
  const zero = modern('zero-response', usage());
  const missing = modern('missing-usage', usage());
  const result = parse([meta(), { ...zero, timestamp: 'not-a-time' },
    { ...missing, payload: { ...missing.payload, usage: undefined } }]);
  assert.equal(result.observations.length, 1);
  assert.equal(result.observations[0].timestamp, null);
  assert.equal(result.observations[0].call_count, 1);
  assert.ok(result.observations[0].diagnostics.includes('MISSING_TIMESTAMP'));
  assert.ok(result.diagnostics.some(item => item.code === 'MALFORMED_USAGE'));
});

test('redacted real Codex 0.154.0 resumed metadata reconciles three responses, not cumulative stdout totals', async () => {
  const fixture = await readFile(new URL('./fixtures/codex-0.154.0-real-metadata.jsonl', import.meta.url), 'utf8');
  const result = parse(fixture.trim().split('\n').map(line => JSON.parse(line)));
  assert.equal(result.observations.length, 3);
  assert.deepEqual(result.observations.map(item => item.input_tokens), [2541, 162, 226]);
  assert.deepEqual(result.observations.map(item => item.output_tokens), [11, 41, 11]);
  assert.deepEqual(result.observations.map(item => item.cache_read_tokens), [12032, 14464, 14464]);
  assert.equal(result.observations.reduce((sum, item) => sum + item.input_tokens! + item.output_tokens! + item.cache_read_tokens! + item.cache_write_tokens!, 0), 43952);
  assert.equal(result.observations.reduce((sum, item) => sum + item.call_count!, 0), 3);
  const observed = result.observations[0];
  assert.deepEqual([observed.input_tokens, observed.output_tokens, observed.cache_read_tokens, observed.cache_write_tokens], [2541, 11, 12032, 0]);
  assert.equal(observed.input_tokens! + observed.cache_read_tokens! + observed.cache_write_tokens!, 14573);
  assert.equal(observed.input_tokens! + observed.output_tokens! + observed.cache_read_tokens! + observed.cache_write_tokens!, 14584);
  assert.equal(observed.call_count, 1);
  assert.equal(observed.model, 'gpt-6-astra');
  assert.equal(observed.provider, 'openai');
  assert.equal(observed.cost_usd, null);
  assert.equal(observed.timestamp, '2026-10-28T00:40:28.224Z');
  assert.ok(result.diagnostics.some(item => item.code === 'REPEATED_SNAPSHOT'));
  assert.deepEqual(result.segments.map(segment => segment.format), ['modern']);
});

test('a repeated modern record cannot rewind continuity or erase subsequent recognized legacy work', () => {
  const record = modern('replayed-modern', usage(10), usage(10), 1);
  const result = parse([meta(), model, record, snapshot(usage(15), usage(5), 2), record]);
  const diagnostics: Diagnostic[] = [];
  const observations = deduplicate(result.observations, diagnostics);
  assert.deepEqual(observations.map(item => [item.grain, item.input_tokens]), [['request', 10], ['reconstructed_interval', 5]]);
  assert.equal(result.supersededCanonicalIds.length, 0);
});

test('stitching never reuses pre-reset cumulative evidence after model, provider, session, malformed or compaction boundaries', () => {
  const malformed = modern('malformed-boundary', usage(1, 0, 2));
  const boundaries = [
    row('turn_context', { model: 'other-model' }),
    meta({ model_provider: 'changed-provider' }),
    meta({ id: 'other-thread' }),
    row('compacted', {}),
    malformed,
  ];
  for (const boundary of boundaries) {
    const next = modern('after-reset', usage(5), usage(15), 3);
    if (boundary.type === 'session_meta' && boundary.payload.id === 'other-thread') next.payload.thread_id = 'other-thread';
    const prefix = [meta(), model, snapshot(usage()), snapshot(usage(10), usage(10), 1)];
    const prior = parse(prefix).observations[0];
    const result = parse([...prefix, boundary, next]);
    assert.deepEqual(result.observations.map(item => item.canonical_id), ['after-reset']);
    assert.deepEqual(result.supersededCanonicalIds, [prior.canonical_id]);
    assert.ok(result.diagnostics.some(item => item.code === 'UNCERTAIN_TRANSITION'));
  }
});

test('parent-only and source-spawn lineage cannot attribute inherited legacy counters without an ownership boundary', () => {
  for (const lineage of [
    { parent_thread_id: 'parent-thread' },
    { source: { sub_agent: { thread_spawn: { parent_thread_id: 'parent-thread', depth: 1 } } } },
    { source: { subagent: { thread_spawn: { parent_thread_id: 'parent-thread', depth: 1 } } } },
  ]) {
    const result = parse([meta(lineage), model, snapshot(usage()), snapshot(usage(10), usage(10), 1),
      modern('own-modern-response', usage(3), usage(13))]);
    assert.deepEqual(result.observations.map(item => item.canonical_id), ['own-modern-response']);
    assert.equal(result.observations[0].parent_session_id, 'parent-thread');
    assert.equal(result.observations[0].is_subagent, true);
    assert.ok(result.diagnostics.some(item => item.code === 'UNOWNED_FORK_INTERVAL'));
  }
});

test('ordinary forks are not fabricated as subagents, and modern inherited responses remain excluded', () => {
  const result = parse([meta({ forked_from_id: 'parent-thread' }), model,
    modern('fork-local-response', usage(4)),
    { ...modern('inherited-response', usage(100)), payload: { ...modern('inherited-response', usage(100)).payload, thread_id: 'parent-thread' } }]);
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['fork-local-response']);
  assert.equal(result.observations[0].parent_session_id, 'parent-thread');
  assert.equal(result.observations[0].is_subagent, false);
});

test('replayed parent session_meta in a fork never replaces the fork identity or drops its own responses', () => {
  const fork = '44444444-4444-4444-8444-444444444444';
  const own = modern('fork-own-response', usage(6));
  const result = parse([meta({ id: fork, forked_from_id: thread }), model, meta(), snapshot(usage(50)),
    { ...own, payload: { ...own.payload, thread_id: fork } }]);
  assert.deepEqual(result.observations.map(item => [item.canonical_id, item.session_id, item.parent_session_id]),
    [['fork-own-response', fork, thread]]);
  assert.ok(result.diagnostics.some(item => item.code === 'INHERITED_CONTEXT'));
  assert.ok(!result.diagnostics.some(item => item.code === 'SESSION_CONTEXT_CHANGED'));
});

test('redacted real Codex 0.154.0 subagent rollout counts only its own post-boundary response', async () => {
  const fixture = await readFile(new URL('./fixtures/codex-0.154.0-real-subagent-metadata.jsonl', import.meta.url), 'utf8');
  const result = parse(fixture.trim().split('\n').map(line => JSON.parse(line)));
  const parent = '01a17ff3-700b-76b2-be96-40479393c5f8';
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['resp_02a033d6cc146a18016ae14548b804819cb56234a34532f491']);
  const [child] = result.observations;
  assert.deepEqual([child.input_tokens, child.cache_read_tokens, child.cache_write_tokens, child.output_tokens, child.call_count], [16440, 0, 0, 9, 1]);
  assert.equal(child.session_id, '01a17ff6-9bdc-7da2-9eea-21e205e2fb4e');
  assert.equal(child.parent_session_id, parent);
  assert.equal(child.is_subagent, true);
  assert.equal(child.provider, 'openai');
  assert.equal(child.model, 'gpt-6-astra');
  assert.ok(result.diagnostics.some(item => item.code === 'INHERITED_CONTEXT'));
  assert.ok(result.diagnostics.some(item => item.code === 'REPEATED_SNAPSHOT'));
});
