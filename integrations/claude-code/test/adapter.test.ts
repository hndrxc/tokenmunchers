import assert from 'node:assert/strict';
import { appendFile, copyFile, mkdir, mkdtemp, readFile, rename, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test, type TestContext } from 'node:test';
import { scan, type ClaudeCursor } from '../src/adapter.ts';
import type { Observation } from '../../shared/src/observations.ts';
import { Ledger } from '../../shared/src/ledger.ts';

async function setup(t: TestContext) {
  const root = await mkdtemp(join(tmpdir(), 'claude-adapter-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const project = join(root, 'projects', 'synthetic-project');
  await mkdir(project, { recursive: true });
  return { root, project };
}
async function fixture(name: string, path: string) {
  await mkdir(dirname(path), { recursive: true });
  await copyFile(new URL(`fixtures/${name}`, import.meta.url), path);
}
function assistant(id: string | null, extra: Record<string, unknown> = {}) {
  return { type: 'assistant', sessionId: 'session-parent', uuid: `uuid-${id}`, timestamp: '2026-10-20T00:00:00Z', message: { ...(id ? { id } : {}), model: 'claude-test', usage: { input_tokens: 10, output_tokens: 999, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 }, stop_reason: 'end_turn' }, ...extra };
}

// Scenario 1, 4, 6: even a terminal/highest assistant output remains unproven.
test('streaming frames coalesce, exclusive cache math survives, output/provider/cost stay unknown', async (t) => {
  const { root, project } = await setup(t);
  await fixture('streaming.jsonl', join(project, 'session-parent.jsonl'));
  const result = await scan(root);
  assert.equal(result.observations.length, 1);
  const row = result.observations[0]!;
  assert.equal(row.canonical_id, 'message:future-valid-message-id');
  assert.equal(row.call_count, 1);
  assert.equal(row.input_tokens, 100);
  assert.equal(row.cache_read_tokens, 40);
  assert.equal(row.cache_write_tokens, 60);
  assert.equal(row.input_tokens! + row.cache_read_tokens! + row.cache_write_tokens!, 200);
  assert.equal(row.output_tokens, null);
  assert.equal(row.provider, 'unknown');
  assert.equal(row.cost_usd, null);
  assert.equal(row.cost_basis, 'unknown');
  assert.equal(row.timestamp, '2026-10-20T23:59:59.000Z');
  assert.ok(row.diagnostics.includes('OUTPUT_PLACEHOLDER_UNVERIFIED'));
  assert.equal(row.quality, 'partial');
});

// Scenario 2: copied prefix UUIDs are intentionally unrelated; the API ID wins.
test('fork copied prefix does not inflate and genuinely new work is retained', async (t) => {
  const { root, project } = await setup(t);
  await fixture('streaming.jsonl', join(project, 'session-parent.jsonl'));
  await fixture('fork.jsonl', join(project, 'session-fork.jsonl'));
  const result = await scan(root);
  assert.equal(result.observations.length, 2);
  assert.equal(result.observations.reduce((sum, row) => sum + row.call_count!, 0), 2);
  assert.equal(result.observations.reduce((sum, row) => sum + row.input_tokens!, 0), 107);
  assert.deepEqual((await scan(root)).observations, result.observations);
});

// Scenario 3: dispatch content alone is never manufactured into model usage.
test('subagent work is linked once and parent dispatch adds no usage', async (t) => {
  const { root, project } = await setup(t);
  await fixture('dispatch-compaction.jsonl', join(project, 'session-parent.jsonl'));
  await fixture('subagent.jsonl', join(project, 'session-parent', 'subagents', 'agent-child-a.jsonl'));
  const result = await scan(root);
  assert.equal(result.observations.length, 1);
  assert.equal(result.observations[0]!.is_subagent, true);
  assert.equal(result.observations[0]!.parent_session_id, 'session-parent');
  assert.equal(result.observations[0]!.session_id, 'session-parent/agent-child-a');
  assert.equal(result.observations[0]!.call_count, 1);
  assert.equal(result.observations[0]!.input_tokens, 11);
  assert.ok(result.diagnostics.some((item) => item.code === 'COMPACTION_COVERAGE_UNVERIFIED'));
  assert.ok(result.diagnostics.some((item) => item.code === 'HELPER_USAGE_UNAVAILABLE'));
});

test('actual parent assistant API work is separate from its child rather than discarded', async (t) => {
  const { root, project } = await setup(t);
  await fixture('streaming.jsonl', join(project, 'session-parent.jsonl'));
  await fixture('subagent.jsonl', join(project, 'session-parent', 'subagents', 'agent-child-a.jsonl'));
  const result = await scan(root);
  assert.equal(result.observations.length, 2);
  assert.equal(result.observations.filter((row) => row.is_subagent).length, 1);
  assert.equal(result.observations.reduce((sum, row) => sum + row.input_tokens!, 0), 111);
});

test('request fallback upgrades to response and emits durable cross-page aliases', async (t) => {
  const { root, project } = await setup(t);
  const first = JSON.stringify(assistant(null, { requestId: 'same-request' })) + '\n';
  const second = JSON.stringify(assistant('server-message', { requestId: 'same-request' })) + '\n';
  await writeFile(join(project, 'session-parent.jsonl'), first + second);
  const complete = await scan(root);
  assert.equal(complete.observations.length, 1);
  assert.equal(complete.observations[0]!.canonical_id, 'message:server-message');
  assert.equal(complete.observations[0]!.identity_strength, 'response');
  assert.deepEqual(complete.identityAliases, [{ from: 'request:same-request', to: 'message:server-message' }]);
  const page = await scan(root, { maxBytes: Buffer.byteLength(first) });
  assert.equal(page.observations[0]!.identity_strength, 'request');
  const next = await scan(root, { cursor: page.cursor, maxBytes: 4096 });
  assert.deepEqual(next.identityAliases, complete.identityAliases);
  assert.equal(next.observations.length, 1);
});

test('request aliases coalesce durable pages, replay and cleanup across collector restarts', async (t) => {
  const { root, project } = await setup(t);
  const directory = join(root, 'collector-state');
  const path = join(project, 'session-parent.jsonl');
  const first = JSON.stringify(assistant(null, { requestId: 'durable-request' })) + '\n';
  const second = JSON.stringify(assistant('durable-response', { requestId: 'durable-request' })) + '\n';
  await writeFile(path, first + second);
  const ledger = new Ledger(directory);
  await ledger.activate('claude-code', root, '2026-10-21T00:00:00Z');
  const page = await scan(root, { maxBytes: Buffer.byteLength(first) });
  await ledger.record('claude-code', page);
  const saved = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.deepEqual(saved.cursor, page.cursor);
  assert.deepEqual(Object.keys(saved.records), ['request:durable-request']);
  await new Ledger(directory).record('claude-code', await scan(root, { cursor: saved.cursor }));
  // A fallback-only copy found after the upgrade must resolve through the durable alias.
  await writeFile(path, first);
  await new Ledger(directory).record('claude-code', await scan(root));
  await rm(path);
  await new Ledger(directory).record('claude-code', await scan(root));
  const retained = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.deepEqual(Object.keys(retained.records), ['message:durable-response']);
  assert.deepEqual(retained.identity_aliases, [{ from: 'request:durable-request', to: 'message:durable-response' }]);
  assert.equal(retained.records['message:durable-response']!.input_tokens, 10);
  assert.equal(retained.records['message:durable-response']!.output_tokens, null);
  assert.equal(retained.records['message:durable-response']!.call_count, 1);
  assert.equal(retained.cursor, undefined);
});

test('a requestId naming two response IDs keeps both responses and never blocks durable ledger scans', async (t) => {
  const { root, project } = await setup(t);
  const directory = join(root, 'collector-state');
  const first = JSON.stringify(assistant('first-response', { requestId: 'shared-request' })) + '\n';
  const second = JSON.stringify(assistant('second-response', { requestId: 'shared-request' })) + '\n';
  const fallback = JSON.stringify(assistant(null, { requestId: 'shared-request' })) + '\n';
  await writeFile(join(project, 'session-parent.jsonl'), first + second + fallback);
  const complete = await scan(root);
  assert.deepEqual(complete.identityAliases, [{ from: 'request:shared-request', to: 'message:first-response' }]);
  assert.ok(complete.diagnostics.some((item) => item.code === 'REQUEST_ID_CONFLICT'));
  assert.deepEqual(complete.observations.map((row) => row.canonical_id).sort(), ['message:first-response', 'message:second-response']);
  assert.ok(complete.observations.find((row) => row.canonical_id === 'message:second-response')!.diagnostics.includes('REQUEST_ID_CONFLICT'));
  const ledger = new Ledger(directory);
  await ledger.activate('claude-code', root, '2026-10-21T00:00:00Z');
  // A page boundary between the responses hides the first alias from the second scan.
  const page = await scan(root, { maxBytes: Buffer.byteLength(first) });
  await ledger.record('claude-code', page);
  const rest = await scan(root, { cursor: page.cursor });
  assert.deepEqual(rest.identityAliases, [{ from: 'request:shared-request', to: 'message:second-response' }]);
  await new Ledger(directory).record('claude-code', rest);
  const conflicted = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.ok((conflicted.progress.diagnostics.IDENTITY_ALIAS_CONFLICT ?? 0) >= 1);
  await new Ledger(directory).record('claude-code', complete);
  await new Ledger(directory).record('claude-code', await scan(root));
  const saved = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.deepEqual(Object.keys(saved.records).sort(), ['message:first-response', 'message:second-response']);
  assert.deepEqual(saved.identity_aliases, [{ from: 'request:shared-request', to: 'message:first-response' }]);
  assert.equal(saved.records['message:first-response']!.call_count, 1);
  assert.equal(saved.records['message:second-response']!.call_count, 1);
  assert.equal(saved.progress.scans, 4);
});

test('unrecorded title helpers are diagnosed while retained helper API work coalesces normally', async (t) => {
  const { root, project } = await setup(t);
  const helper = JSON.stringify(assistant('helper-response', { requestId: 'helper-request' }));
  await writeFile(join(project, 'helpers.jsonl'), [
    JSON.stringify({ type: 'custom-title', customTitle: 'synthetic title' }),
    helper, helper,
  ].join('\n') + '\n');
  const result = await scan(root);
  assert.equal(result.observations.length, 1);
  assert.equal(result.observations[0]!.canonical_id, 'message:helper-response');
  assert.equal(result.observations[0]!.call_count, 1);
  assert.ok(result.diagnostics.some(item => item.code === 'HELPER_USAGE_UNAVAILABLE'));
  assert.ok(result.diagnostics.some(item => item.code === 'HELPER_COVERAGE_INCOMPLETE'));
});

test('UUID fallbacks are stable only within each transcript and expose fork uncertainty', async (t) => {
  const { root, project } = await setup(t);
  const line = JSON.stringify(assistant(null, { uuid: 'local-only' })) + '\n';
  await writeFile(join(project, 'parent.jsonl'), line + line);
  await writeFile(join(project, 'fork.jsonl'), line);
  const result = await scan(root);
  assert.equal(result.observations.length, 2);
  assert.ok(result.observations.every((row) => row.identity_strength === 'scoped' && row.diagnostics.includes('SCOPED_IDENTITY_FORK_UNCERTAIN')));
  assert.ok(result.observations.every((row) => !row.canonical_id.includes('synthetic-project')));
});

test('missing identities are not invented from file offsets', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'missing-id.jsonl'), JSON.stringify(assistant(null, { uuid: undefined })) + '\n');
  const result = await scan(root);
  assert.equal(result.observations.length, 0);
  assert.ok(result.diagnostics.some((item) => item.code === 'ASSISTANT_USAGE_SKIPPED'));
});

test('partial trailing JSON is withheld until newline and subsequent rescan is stable', async (t) => {
  const { root, project } = await setup(t);
  const path = join(project, 'partial.jsonl');
  await writeFile(path, JSON.stringify(assistant('partial')));
  const partial = await scan(root);
  assert.equal(partial.observations.length, 0);
  assert.ok(partial.diagnostics.some((item) => item.code === 'PARTIAL_LINE_DEFERRED'));
  assert.equal(partial.progress.deferred, true);
  await appendFile(path, '\n');
  const complete = await scan(root);
  assert.equal(complete.observations.length, 1);
  assert.deepEqual((await scan(root)).observations, complete.observations);
});

test('malformed UTF-8/JSON rows and oversized rows cannot manufacture usage', async (t) => {
  const { root, project } = await setup(t);
  const path = join(project, 'malformed.jsonl');
  await writeFile(path, Buffer.concat([Buffer.from('{bad}\n'), Buffer.from([0xff, 0x0a]), Buffer.from(JSON.stringify({ type: 'user', padding: 'x'.repeat(1024) }) + '\n'), Buffer.from(JSON.stringify(assistant('valid')) + '\n')]));
  const result = await scan(root, { maxLineBytes: 512 });
  assert.equal(result.observations.length, 1);
  assert.equal(result.diagnostics.filter((item) => item.code === 'MALFORMED_JSONL').length, 2);
  assert.ok(result.diagnostics.some((item) => item.code === 'LINE_TOO_LARGE'));
});

test('bounded continuation reaches later files and uses canonical identities rather than cursor identity', async (t) => {
  const { root, project } = await setup(t);
  const a = JSON.stringify(assistant('a')) + '\n';
  const b = JSON.stringify(assistant('b')) + '\n';
  await writeFile(join(project, 'a.jsonl'), a + b);
  await writeFile(join(project, 'b.jsonl'), JSON.stringify(assistant('c')) + '\n');
  let cursor: ClaudeCursor | null = null;
  const seen = new Map<string, Observation>();
  for (let count = 0; count < 6; count++) {
    const result = await scan(root, { maxBytes: Buffer.byteLength(a) + 10, cursor });
    assert.ok(result.progress.bytesRead <= Buffer.byteLength(a) + 10);
    for (const row of result.observations) seen.set(row.canonical_id, row);
    cursor = result.cursor;
    if (!cursor) break;
  }
  assert.equal(cursor, null);
  assert.deepEqual([...seen.keys()].sort(), ['message:a', 'message:b', 'message:c']);
});

test('oversized record continuation never reparses its tail or strands later usage', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'a.jsonl'), JSON.stringify({ type: 'user', padding: 'x'.repeat(2000) }) + '\n' + JSON.stringify(assistant('after-oversize')) + '\n');
  let cursor: ClaudeCursor | null = null;
  const seen: Observation[] = [];
  const codes: string[] = [];
  for (let count = 0; count < 10; count++) {
    const result = await scan(root, { maxBytes: 400, maxLineBytes: 350, cursor });
    seen.push(...result.observations);
    codes.push(...result.diagnostics.map((item) => item.code));
    cursor = result.cursor;
    if (!cursor) break;
  }
  assert.equal(cursor, null);
  assert.equal(seen.length, 1);
  assert.equal(seen[0]!.canonical_id, 'message:after-oversize');
  assert.ok(codes.includes('LINE_TOO_LARGE'));
  assert.ok(!codes.includes('MALFORMED_JSONL'));
});

test('replaced transcript resets continuation safely', async (t) => {
  const { root, project } = await setup(t);
  const path = join(project, 'a.jsonl');
  const first = JSON.stringify(assistant('before')) + '\n';
  await writeFile(path, first + JSON.stringify(assistant('unread')) + '\n');
  const page = await scan(root, { maxBytes: Buffer.byteLength(first) });
  await rename(path, `${path}.old`);
  await writeFile(path, JSON.stringify(assistant('replacement')) + '\n');
  const next = await scan(root, { cursor: page.cursor });
  assert.equal(next.observations[0]!.canonical_id, 'message:replacement');
  assert.ok(next.diagnostics.some((item) => item.code === 'CURSOR_RESET'));
});

test('explicit selected roots, prefix exclusions and symlink refusal restrict discovery', async (t) => {
  const { root, project } = await setup(t);
  const elsewhere = await mkdtemp(join(tmpdir(), 'claude-unselected-'));
  t.after(() => rm(elsewhere, { recursive: true, force: true }));
  await writeFile(join(elsewhere, 'private.jsonl'), JSON.stringify(assistant('unselected')) + '\n');
  await symlink(join(elsewhere, 'private.jsonl'), join(project, 'symlink.jsonl'));
  await symlink(elsewhere, join(root, 'projects', 'linked-project'));
  await writeFile(join(project, 'selected.jsonl'), JSON.stringify(assistant('selected')) + '\n');
  assert.equal((await scan(root)).observations.length, 1);
  assert.equal((await scan(root, { exclude: ['projects/synthetic-project'] })).observations.length, 0);
  assert.equal((await scan(root, { exclude: ['projects/synthetic-project/selected.jsonl'] })).observations.length, 0);
});

test('saved cumulative summaries supersede prior resumes without additive call/day attribution', async (t) => {
  const { root, project } = await setup(t);
  const path = join(project, 'session-parent.jsonl');
  await fixture('cost-state.jsonl', path);
  const first = await scan(root);
  assert.equal(first.observations.length, 1);
  const summary = first.observations[0]!;
  assert.equal(summary.grain, 'session_summary');
  assert.equal(summary.output_tokens, 35);
  assert.equal(summary.input_tokens, 107);
  assert.equal(summary.cost_usd, 0.015);
  assert.equal(summary.cost_basis, 'estimated');
  assert.equal(summary.timestamp, null);
  assert.equal(summary.call_count, null);
  assert.equal(summary.provider, 'unknown');
  assert.ok(summary.diagnostics.includes('CUMULATIVE_SUMMARY_NONADDITIVE'));
  const lines = (await readFile(path, 'utf8')).trimEnd().split('\n');
  await appendFile(path, `${lines[1]}\n`);
  assert.deepEqual((await scan(root)).observations, first.observations);
});

test('result model summaries retain explicit provider and never repair per-request output', async (t) => {
  const { root, project } = await setup(t);
  const saved = { type: 'result', subtype: 'success', session_id: 'session-parent', modelUsage: { 'claude-test': { inputTokens: 10, outputTokens: 44, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.02, provider: 'bedrock' } }, usage: { input_tokens: 10, output_tokens: 44 }, total_cost_usd: 0.02 };
  await writeFile(join(project, 'session-parent.jsonl'), JSON.stringify(assistant('model-response')) + '\n' + JSON.stringify(saved) + '\n');
  const result = await scan(root);
  assert.equal(result.observations.find((row) => row.grain === 'request')!.output_tokens, null);
  const summary = result.observations.find((row) => row.grain === 'session_summary')!;
  assert.equal(summary.provider, 'bedrock');
  assert.equal(summary.output_tokens, 44);
  assert.equal(summary.timestamp, null);
});

test('crashed sessions work without summaries and zeroed crash results are withheld', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'session-parent.jsonl'), JSON.stringify(assistant('before-crash')) + '\n' + JSON.stringify({ type: 'result', subtype: 'error_during_execution', total_cost_usd: 0, modelUsage: {} }) + '\n');
  const result = await scan(root);
  assert.equal(result.observations.length, 1);
  assert.equal(result.observations[0]!.output_tokens, null);
  assert.ok(result.diagnostics.some((item) => item.code === 'CRASH_SUMMARY_UNRELIABLE'));
});

test('invalid and absent token categories remain null rather than zero', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'invalid.jsonl'), JSON.stringify(assistant('invalid', { message: { id: 'invalid', model: 'claude-test', usage: { input_tokens: -1, output_tokens: 0, cache_read_input_tokens: Number.MAX_SAFE_INTEGER + 1 } } })) + '\n');
  const row = (await scan(root)).observations[0]!;
  assert.equal(row.input_tokens, null);
  assert.equal(row.cache_read_tokens, null);
  assert.equal(row.cache_write_tokens, null);
  assert.equal(row.output_tokens, null);
  assert.ok(row.diagnostics.includes('MALFORMED_TOKEN_COUNT'));
});

test('oversized cumulative summary and request counts become unavailable without blocking the durable ledger', async (t) => {
  const { root, project } = await setup(t);
  const directory = join(root, 'collector-state');
  const oversized = { type: 'cost-state', sessionId: 'session-parent', totalCostUSD: 25_000, modelUsage: { 'claude-test': { inputTokens: 2e9, outputTokens: 35, cacheReadInputTokens: 40, cacheCreationInputTokens: 60, costUSD: 25_000 } }, hasUnknownModelCost: false };
  const huge = assistant('huge-request', { message: { id: 'huge-request', model: 'claude-test', usage: { input_tokens: 2e9, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 } } });
  await writeFile(join(project, 'session-parent.jsonl'), [assistant('normal-request'), huge, oversized].map(row => JSON.stringify(row)).join('\n') + '\n');
  const result = await scan(root);
  assert.ok(result.diagnostics.some((item) => item.code === 'SUMMARY_VALUE_OUT_OF_RANGE'));
  const summary = result.observations.find((row) => row.grain === 'session_summary')!;
  assert.equal(summary.input_tokens, null);
  assert.equal(summary.output_tokens, 35);
  assert.equal(summary.cost_usd, null);
  assert.equal(summary.cost_basis, 'unknown');
  assert.ok(summary.diagnostics.includes('SUMMARY_VALUE_OUT_OF_RANGE'));
  const request = result.observations.find((row) => row.canonical_id === 'message:huge-request')!;
  assert.equal(request.input_tokens, null);
  assert.ok(request.diagnostics.includes('MALFORMED_TOKEN_COUNT'));
  const ledger = new Ledger(directory);
  await ledger.activate('claude-code', root, '2026-10-21T00:00:00Z');
  await ledger.record('claude-code', result);
  await new Ledger(directory).record('claude-code', await scan(root));
  const saved = await new Ledger(directory).transaction(state => state.sources['claude-code']!);
  assert.equal(saved.progress.scans, 2);
  assert.equal(saved.records['message:normal-request']!.input_tokens, 10);
  assert.equal(saved.records['message:huge-request']!.input_tokens, null);
  assert.equal(saved.records[summary.canonical_id]!.cost_usd, null);
  assert.equal(saved.records[summary.canonical_id]!.input_tokens, null);
});

// Scenario 5's retained-ledger/server half belongs to the shared collector regression.
test('retention cleanup reports no replacement/delete instruction to its durable ledger caller', async (t) => {
  const { root, project } = await setup(t);
  const path = join(project, 'gone.jsonl');
  await writeFile(path, JSON.stringify(assistant('retained-by-collector')) + '\n');
  const accepted = await scan(root);
  await rm(path);
  const pruned = await scan(root);
  assert.equal(accepted.observations.length, 1);
  assert.deepEqual(pruned.observations, []);
  assert.equal(pruned.cursor, null);
  assert.deepEqual(pruned.discoveredFiles, []);
});

test('metadata observations contain neither transcript content nor source paths or authorization', async (t) => {
  const { root, project } = await setup(t);
  const secret = 'synthetic-secret-never-export';
  await writeFile(join(project, 'privacy.jsonl'), JSON.stringify(assistant('safe-id', { authorization: secret, headers: { Authorization: secret }, provider: secret, message: { id: 'safe-id', model: 'claude-test', content: [{ type: 'text', text: secret }], usage: { input_tokens: 1, output_tokens: 999 } } })) + '\n');
  const result = await scan(root);
  const outbound = JSON.stringify(result.observations);
  assert.ok(!outbound.includes(secret));
  assert.ok(!outbound.includes(root));
  assert.equal(result.observations[0]!.provider, 'unknown');
});

test('explicit nonsecret provider metadata is allowlisted and conflicting providers stay unknown', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'providers.jsonl'), [
    assistant('direct', { provider: 'firstParty' }),
    assistant('managed', { provider: 'vertex' }),
    assistant('conflicting', { provider: 'bedrock' }),
    assistant('conflicting', { provider: 'vertex' }),
    assistant('conflicting', { provider: 'bedrock' }),
  ].map(row => JSON.stringify(row)).join('\n') + '\n');
  const result = await scan(root);
  const rows = new Map(result.observations.map(row => [row.canonical_id, row]));
  assert.equal(rows.get('message:direct')!.provider, 'anthropic');
  assert.equal(rows.get('message:managed')!.provider, 'vertex');
  assert.equal(rows.get('message:conflicting')!.provider, 'unknown');
  assert.ok(rows.get('message:conflicting')!.diagnostics.includes('CONFLICTING_PROVIDER'));
});

test('unavailable request and cumulative summary metadata remain unavailable', async (t) => {
  const { root, project } = await setup(t);
  await writeFile(join(project, 'unknowns.jsonl'), [
    assistant('missing-metadata', { timestamp: undefined, message: { id: 'missing-metadata', usage: {} } }),
    { type: 'cost-state', sessionId: 'session-parent' },
  ].map(row => JSON.stringify(row)).join('\n') + '\n');
  const result = await scan(root);
  const request = result.observations.find(row => row.grain === 'request')!;
  assert.equal(request.timestamp, null);
  assert.equal(request.model, 'unknown');
  assert.equal(request.provider, 'unknown');
  assert.equal(request.input_tokens, null);
  assert.equal(request.output_tokens, null);
  assert.equal(request.cache_read_tokens, null);
  assert.equal(request.cache_write_tokens, null);
  assert.equal(request.cost_usd, null);
  assert.equal(request.call_count, 1);
  const summary = result.observations.find(row => row.grain === 'session_summary')!;
  assert.equal(summary.timestamp, null);
  assert.equal(summary.input_tokens, null);
  assert.equal(summary.output_tokens, null);
  assert.equal(summary.cost_usd, null);
  assert.equal(summary.cost_basis, 'unknown');
  assert.equal(summary.call_count, null);
});

// Redacted real Claude Code 2.1.312 sessions; see fixtures/MANIFEST.md for provenance and limits.
const realSessions = [
  { fixture: 'real-2.1.312-resume.jsonl', session: '7c0ffee0-0000-4000-8000-000000000001', outputs: { 'message:msg_011CgTr3FQP69RJgD4LAGoRV': 22, 'message:msg_011CgTr3rQorXiUUREsPA7d4': 21 }, total: 43 },
  { fixture: 'real-2.1.312-tool-turn.jsonl', session: '7c0ffee0-0000-4000-8000-000000000002', outputs: { 'message:msg_011CgTr5z7DfTdiqDSK2jhGr': 83, 'message:msg_011CgTr68TkxDhWCNnhGC6vN': 20 }, total: 103 },
] as const;

test('real 2.1.312 sessions reconcile saved response output exactly with cost-state without allocating cost', async (t) => {
  const { root, project } = await setup(t);
  for (const real of realSessions) await fixture(real.fixture, join(project, `${real.session}.jsonl`));
  const result = await scan(root);
  assert.ok(!result.diagnostics.some((item) => item.code === 'UNRECOGNIZED_RECORD' || item.code === 'OUTPUT_RECONCILIATION_MISMATCH'));
  for (const real of realSessions) {
    const requests = result.observations.filter((row) => row.grain === 'request' && row.session_id === real.session);
    assert.deepEqual(Object.fromEntries(requests.map((row) => [row.canonical_id, row.output_tokens])), real.outputs);
    for (const row of requests) {
      assert.equal(row.quality, 'observed');
      assert.equal(row.call_count, 1);
      assert.equal(row.cost_usd, null);
      assert.equal(row.cost_basis, 'unknown');
      assert.ok(row.diagnostics.includes('OUTPUT_RECONCILED_WITH_COST_STATE'));
      assert.ok(!row.diagnostics.includes('OUTPUT_PLACEHOLDER_UNVERIFIED'));
    }
    const summary = result.observations.find((row) => row.grain === 'session_summary' && row.session_id === real.session)!;
    assert.equal(summary.output_tokens, real.total);
    assert.equal(requests.reduce((sum, row) => sum + row.output_tokens!, 0), real.total);
  }
  // The two-frame tool turn coalesces into one call rather than adding its repeated frame.
  assert.equal(result.observations.filter((row) => row.grain === 'request').length, 4);
  assert.deepEqual((await scan(root)).observations, result.observations);
});

test('paged sessions stay unverified until a whole-session scan upgrades the durable ledger', async (t) => {
  const { root, project } = await setup(t);
  const real = realSessions[0];
  const path = join(project, `${real.session}.jsonl`);
  await fixture(real.fixture, path);
  const ledger = new Ledger(join(root, 'collector-state'));
  await ledger.activate('claude-code', root, '2026-10-29T00:00:00Z');
  const maxBytes = Math.ceil((await readFile(path)).length / 2);
  let cursor: ClaudeCursor | null = null;
  for (let count = 0; count < 10; count++) {
    const page = await scan(root, { maxBytes, cursor });
    for (const row of page.observations.filter((item) => item.grain === 'request')) {
      assert.equal(row.output_tokens, null);
      assert.ok(row.diagnostics.includes('OUTPUT_RECONCILIATION_DEFERRED'));
    }
    await ledger.record('claude-code', page);
    cursor = page.cursor;
    if (!cursor) break;
  }
  assert.equal(cursor, null);
  await ledger.record('claude-code', await scan(root));
  // A later paged replay cannot downgrade the verified durable observation.
  await ledger.record('claude-code', await scan(root, { maxBytes }));
  const saved = await new Ledger(join(root, 'collector-state')).transaction(state => state.sources['claude-code']!);
  for (const [id, output] of Object.entries(real.outputs)) {
    assert.equal(saved.records[id]!.output_tokens, output);
    assert.equal(saved.records[id]!.quality, 'observed');
    assert.equal(saved.records[id]!.cost_usd, null);
  }
});

test('placeholder frames with differing output and absent helper calls never reconcile', async (t) => {
  const { root, project } = await setup(t);
  const frame = (output: number) => assistant('placeholder', { message: { id: 'placeholder', model: 'claude-test', usage: { input_tokens: 10, output_tokens: output, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 } } });
  const costState = (sessionId: string, outputTokens: number) => ({ type: 'cost-state', sessionId, totalCostUSD: 0.01, modelUsage: { 'claude-test': { inputTokens: 10, outputTokens, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.01 } } });
  // The final frame alone would match; differing frames prove saved output was a placeholder.
  await writeFile(join(project, 'session-parent.jsonl'), [frame(1), frame(30), costState('session-parent', 30)].map((row) => JSON.stringify(row)).join('\n') + '\n');
  // An unrecorded helper call leaves cost-state above the retained responses.
  await writeFile(join(project, 'session-helper.jsonl'), [assistant('retained', { sessionId: 'session-helper' }), costState('session-helper', 1500)].map((row) => JSON.stringify(row)).join('\n') + '\n');
  const result = await scan(root);
  for (const id of ['message:placeholder', 'message:retained']) {
    const row = result.observations.find((item) => item.canonical_id === id)!;
    assert.equal(row.output_tokens, null);
    assert.equal(row.quality, 'partial');
    assert.ok(row.diagnostics.includes('OUTPUT_PLACEHOLDER_UNVERIFIED'));
    assert.ok(row.diagnostics.includes('OUTPUT_RECONCILIATION_MISMATCH'));
  }
  assert.equal(result.diagnostics.filter((item) => item.code === 'OUTPUT_RECONCILIATION_MISMATCH').length, 2);
});

test('a real session without its cost-state keeps response output unverified', async (t) => {
  const { root, project } = await setup(t);
  const real = realSessions[1];
  const lines = (await readFile(new URL(`fixtures/${real.fixture}`, import.meta.url), 'utf8')).trimEnd().split('\n');
  await writeFile(join(project, `${real.session}.jsonl`), lines.filter((line) => JSON.parse(line).type !== 'cost-state').join('\n') + '\n');
  const requests = (await scan(root)).observations.filter((row) => row.grain === 'request');
  assert.equal(requests.length, 2);
  for (const row of requests) {
    assert.equal(row.output_tokens, null);
    assert.equal(row.quality, 'partial');
    assert.ok(row.diagnostics.includes('OUTPUT_PLACEHOLDER_UNVERIFIED'));
    assert.ok(row.diagnostics.includes('OUTPUT_RECONCILIATION_MISMATCH'));
  }
});

test('subagent responses join their parent cost-state and excluded subagents block reconciliation', async (t) => {
  const { root, project } = await setup(t);
  const costState = { type: 'cost-state', sessionId: 'session-parent', totalCostUSD: 0.01, modelUsage: { 'claude-test': { inputTokens: 20, outputTokens: 1998, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.01 } } };
  await writeFile(join(project, 'session-parent.jsonl'), [assistant('parent-call'), costState].map((row) => JSON.stringify(row)).join('\n') + '\n');
  await mkdir(join(project, 'session-parent', 'subagents'), { recursive: true });
  await writeFile(join(project, 'session-parent', 'subagents', 'agent-child.jsonl'), JSON.stringify(assistant('child-call', { isSidechain: true })) + '\n');
  const joined = await scan(root);
  for (const id of ['message:parent-call', 'message:child-call']) {
    const row = joined.observations.find((item) => item.canonical_id === id)!;
    assert.equal(row.output_tokens, 999);
    assert.equal(row.quality, 'observed');
  }
  const partial = await scan(root, { exclude: ['projects/synthetic-project/session-parent/subagents'] });
  const parent = partial.observations.find((item) => item.canonical_id === 'message:parent-call')!;
  assert.equal(parent.output_tokens, null);
  assert.ok(parent.diagnostics.includes('OUTPUT_RECONCILIATION_MISMATCH'));
});
