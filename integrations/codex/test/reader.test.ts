import assert from 'node:assert/strict';
import { appendFile, chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { zstdCompressSync } from 'node:zlib';
import { scan } from '../src/adapter.ts';
import { meta, modern, usage } from './fixtures/builders.ts';

async function source(t: { after: (fn: () => Promise<void>) => void }) {
  const root = await mkdtemp(join(tmpdir(), 'codex-adapter-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, 'sessions', '2026', '10', '15'), { recursive: true });
  await mkdir(join(root, 'archived_sessions'));
  return root;
}
const encode = (rows: unknown[]) => rows.map(value => JSON.stringify(value)).join('\n') + '\n';

test('chosen-root discovery reads nested active and flat archives, plain and streaming zstd, once per canonical response', async t => {
  const root = await source(t);
  const fixture = await readFile(new URL('./fixtures/modern-source-derived.jsonl', import.meta.url));
  await writeFile(join(root, 'sessions/2026/10/15/rollout-active.jsonl'), fixture);
  await writeFile(join(root, 'archived_sessions/rollout-copy.jsonl.zst'), zstdCompressSync(fixture));
  await writeFile(join(root, 'archived_sessions/rollout-new.jsonl.zst'), zstdCompressSync(Buffer.from(encode([meta(), modern('archived-new', usage(7))]))));
  await writeFile(join(root, 'auth.json'), 'SECRET');
  await writeFile(join(root, 'sessions/2026/10/15/ignored.jsonl'), encode([meta(), modern('ignored', usage(999))]));
  const result = await scan(root);
  assert.deepEqual(result.observations.map(item => item.canonical_id).sort(), ['archived-new', 'response-a', 'response-b', 'response-c']);
  assert.equal(result.observations.reduce((sum, item) => sum + item.input_tokens!, 0), 237);
  assert.equal(result.progress.files, 3);
  assert.ok(!JSON.stringify(result).includes('SECRET'));
  assert.deepEqual((await scan(root)).observations, result.observations);
});

test('root-relative exclusions and symlink refusal never scan a different selected source', async t => {
  const root = await source(t);
  const elsewhere = await source(t);
  await writeFile(join(elsewhere, 'archived_sessions/rollout-private.jsonl'), encode([meta(), modern('private', usage(999))]));
  await symlink(join(elsewhere, 'archived_sessions'), join(root, 'sessions/linked'), 'dir');
  await symlink(join(elsewhere, 'archived_sessions/rollout-private.jsonl'), join(root, 'archived_sessions/rollout-link.jsonl'));
  await writeFile(join(root, 'archived_sessions/rollout-excluded.jsonl'), encode([meta(), modern('excluded', usage(999))]));
  await writeFile(join(root, 'sessions/2026/10/15/rollout-selected.jsonl'), encode([meta(), modern('selected', usage(1))]));
  const result = await scan(root, { exclude: ['archived_sessions'] });
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['selected']);
  assert.ok(result.diagnostics.some(item => item.code === 'EXCLUDED'));
  assert.ok(result.diagnostics.some(item => item.code === 'SYMLINK_SKIPPED'));
  const links = await scan(root);
  assert.ok(!links.observations.some(item => item.canonical_id === 'private'));
  const linkedRoot = join(root, 'linked-root');
  await symlink(elsewhere, linkedRoot, 'dir');
  assert.ok((await scan(linkedRoot)).diagnostics.some(item => item.code === 'INVALID_ROOT'));
});

test('incomplete trailing record is deferred then completed without accepting a partial contribution', async t => {
  const root = await source(t);
  const file = join(root, 'archived_sessions/rollout-tail.jsonl');
  const record = JSON.stringify(modern('late-original-time', usage(5)));
  await writeFile(file, encode([meta()]) + record.slice(0, -5));
  const first = await scan(root);
  assert.equal(first.observations.length, 0);
  assert.ok(first.diagnostics.some(item => item.code === 'DEFERRED_PARTIAL_LINE'));
  await appendFile(file, record.slice(-5) + '\n');
  const second = await scan(root);
  assert.equal(second.observations.length, 1);
  assert.equal(second.observations[0].timestamp, '2026-10-15T10:00:02.000Z');
  assert.deepEqual((await scan(root)).observations, second.observations);
});

test('valid but unterminated final JSON is never accepted until newline commits it', async t => {
  const root = await source(t);
  const file = join(root, 'archived_sessions/rollout-tail.jsonl');
  await writeFile(file, encode([meta()]) + JSON.stringify(modern('needs-newline', usage(5))));
  assert.equal((await scan(root)).observations.length, 0);
  await appendFile(file, '\n');
  assert.equal((await scan(root)).observations.length, 1);
});

test('malformed and oversized complete records are diagnosed without leaking raw content or blocking later rows', async t => {
  const root = await source(t);
  const file = join(root, 'archived_sessions/rollout-bounded.jsonl');
  await writeFile(file, encode([meta()]) + '{"SECRET-PROMPT":!}\n' + JSON.stringify({ content: 'SECRET-TOOL'.repeat(500) }) + '\n' + encode([modern('after-bad-lines', usage(2))]));
  const result = await scan(root, { maxLineBytes: 2048 });
  assert.equal(result.observations.length, 1);
  assert.ok(result.diagnostics.some(item => item.code === 'MALFORMED_JSON'));
  assert.ok(result.diagnostics.some(item => item.code === 'LINE_TOO_LARGE'));
  assert.ok(!JSON.stringify(result).includes('SECRET-PROMPT'));
  assert.ok(!JSON.stringify(result).includes('SECRET-TOOL'));
});

test('scan budgets expose continuation and deferred paths, with larger budget preserving canonical results', async t => {
  const root = await source(t);
  const a = encode([meta(), modern('first', usage(2))]);
  const b = encode([meta(), modern('second', usage(3))]);
  await writeFile(join(root, 'archived_sessions/rollout-a.jsonl'), a);
  await writeFile(join(root, 'archived_sessions/rollout-b.jsonl'), b);
  const bounded = await scan(root, { maxBytes: Buffer.byteLength(a) + 10 });
  assert.deepEqual(bounded.observations.map(item => item.canonical_id), ['first']);
  assert.ok(bounded.diagnostics.some(item => item.code === 'SCAN_TRUNCATED'));
  assert.equal(bounded.continuation.nextFile, 'archived_sessions/rollout-b.jsonl');
  assert.ok(bounded.deferredFiles.includes('archived_sessions/rollout-b.jsonl'));
  const continuation = { nextFile: bounded.continuation.nextFile! };
  const rest = await scan(root, { maxBytes: 10000, continuation });
  assert.deepEqual(rest.observations.map(item => item.canonical_id), ['second']);
  assert.deepEqual((await scan(root, { maxBytes: 10000 })).observations.map(item => item.canonical_id), ['first', 'second']);
});

test('oversized first rollout cannot pin continuation; later rollouts are scanned and it stays deferred', async t => {
  const root = await source(t);
  const later = encode([meta(), modern('after-oversize', usage(3))]);
  await writeFile(join(root, 'archived_sessions/rollout-a.jsonl'), encode([meta(), ...Array.from({ length: 40 }, (_, index) => modern(`big-${index}`, usage(1)))]));
  await writeFile(join(root, 'archived_sessions/rollout-b.jsonl'), later);
  const maxBytes = Buffer.byteLength(later) + 10;
  const first = await scan(root, { maxBytes });
  assert.ok(first.diagnostics.some(item => item.code === 'SCAN_BOUND_TOO_SMALL' && item.file === 'archived_sessions/rollout-a.jsonl'));
  assert.ok(first.deferredFiles.includes('archived_sessions/rollout-a.jsonl'));
  assert.equal(first.continuation.nextFile, 'archived_sessions/rollout-b.jsonl');
  const rest = await scan(root, { maxBytes, continuation: { nextFile: first.continuation.nextFile! } });
  assert.deepEqual(rest.observations.map(item => item.canonical_id), ['after-oversize']);
  assert.equal(rest.continuation.nextFile, null);
  const again = await scan(root, { maxBytes });
  assert.ok(again.deferredFiles.includes('archived_sessions/rollout-a.jsonl'));
  assert.equal(again.continuation.nextFile, 'archived_sessions/rollout-b.jsonl');
});

test('permanent read failure never pins continuation; earlier and later rollouts are rescanned every pass', async t => {
  const root = await source(t);
  const earlier = encode([meta(), modern('earlier', usage(1))]);
  await writeFile(join(root, 'archived_sessions/rollout-a.jsonl'), earlier);
  const broken = join(root, 'archived_sessions/rollout-b.jsonl');
  // Unterminated too, so the file stays permanently incomplete even where permissions are bypassed (root).
  await writeFile(broken, '{"type"');
  await chmod(broken, 0o000);
  await writeFile(join(root, 'archived_sessions/rollout-c.jsonl'), encode([meta(), modern('later', usage(1))]));
  const code = process.getuid?.() === 0 ? 'DEFERRED_PARTIAL_LINE' : 'READ_FAILED';
  const full = await scan(root);
  assert.deepEqual(full.observations.map(item => item.canonical_id), ['earlier', 'later']);
  assert.ok(full.diagnostics.some(item => item.code === code && item.file === 'archived_sessions/rollout-b.jsonl'));
  assert.deepEqual(full.deferredFiles, ['archived_sessions/rollout-b.jsonl']);
  assert.equal(full.continuation.nextFile, null);
  // Bounded passes resumed like the collector does: the failing file is retried but never becomes a sticky resume point.
  const seen = new Map<string, number>();
  let continuation: { nextFile: string } | undefined;
  for (let pass = 0; pass < 6; pass++) {
    const result = await scan(root, { maxBytes: Buffer.byteLength(earlier), continuation });
    for (const item of result.observations) seen.set(item.canonical_id, (seen.get(item.canonical_id) ?? 0) + 1);
    continuation = result.continuation.nextFile ? { nextFile: result.continuation.nextFile } : undefined;
  }
  assert.ok((seen.get('earlier') ?? 0) >= 2, 'earlier rollout is rescanned after the failing file');
  assert.ok((seen.get('later') ?? 0) >= 2, 'later rollout keeps being reached');
});

test('compressed expansion consumes the decompressed byte budget and corrupt archives are visible', async t => {
  const root = await source(t);
  const rows = encode([meta(), ...Array.from({ length: 100 }, (_, index) => modern(`response-${index}`, usage(1)))]);
  await writeFile(join(root, 'archived_sessions/rollout-a.jsonl.zst'), zstdCompressSync(Buffer.from(rows)));
  const result = await scan(root, { maxBytes: 2000 });
  assert.ok(result.observations.length < 100);
  assert.equal(result.progress.bytesRead, 2000);
  assert.ok(result.diagnostics.some(item => item.code === 'SCAN_TRUNCATED'));
  await writeFile(join(root, 'archived_sessions/rollout-b.jsonl.zst'), 'not-zstd');
  const full = await scan(root);
  assert.ok(full.diagnostics.some(item => item.code === 'DECOMPRESSION_FAILED'));
});

test('unavailable roots, stale continuations and invalid limits are explicit', async t => {
  const root = await source(t);
  assert.ok((await scan(join(root, 'missing'))).diagnostics.some(item => item.code === 'INVALID_ROOT'));
  assert.ok((await scan(root, { continuation: { nextFile: 'gone.jsonl' } })).diagnostics.some(item => item.code === 'STALE_CONTINUATION'));
  await assert.rejects(() => scan(root, { maxBytes: 0 }), RangeError);
  await assert.rejects(() => scan(root, { maxLineBytes: -1 }), RangeError);
});

test('chunk boundaries preserve UTF-8 and complete records; invalid UTF-8 is not silently replaced', async t => {
  const root = await source(t);
  const file = join(root, 'archived_sessions/rollout-chunks.jsonl');
  const large = JSON.stringify({ content: 'é'.repeat(40000) }) + '\n';
  const invalid = Buffer.concat([Buffer.from('{\"ignored\":\"'), Buffer.from([0xff]), Buffer.from('\"}\n')]);
  await writeFile(file, Buffer.concat([Buffer.from(encode([meta()]) + large), invalid, Buffer.from(encode([modern('after-chunks', usage(6))]))]));
  const result = await scan(root, { maxLineBytes: 100000 });
  assert.deepEqual(result.observations.map(item => item.canonical_id), ['after-chunks']);
  assert.equal(result.diagnostics.filter(item => item.code === 'MALFORMED_JSON').length, 1);
  assert.ok(result.cursors.every(cursor => cursor.complete));
});

test('concatenated zstd frames stream normally and incomplete frame reports deferred coverage', async t => {
  const root = await source(t);
  const file = join(root, 'archived_sessions/rollout-frames.jsonl.zst');
  const first = zstdCompressSync(Buffer.from(encode([meta(), modern('frame-a', usage(3))])));
  const second = zstdCompressSync(Buffer.from(encode([modern('frame-b', usage(4))])));
  await writeFile(file, Buffer.concat([first, second]));
  assert.deepEqual((await scan(root)).observations.map(item => item.canonical_id), ['frame-a', 'frame-b']);
  await writeFile(file, Buffer.concat([first, second.subarray(0, second.length - 3)]));
  const partial = await scan(root);
  assert.ok(partial.diagnostics.some(item => item.code === 'DECOMPRESSION_FAILED'));
  assert.ok(partial.progress.deferred > 0);
});

test('redacted real 0.154.0 parent and subagent rollouts scan once with explicit lineage', async t => {
  const root = await source(t);
  const day = join(root, 'sessions/2026/10/27');
  await mkdir(day, { recursive: true });
  for (const [fixture, name] of [
    ['codex-0.154.0-real-metadata.jsonl', 'rollout-2026-10-27T19-40-22-01a17ff3-700b-76b2-be96-40479393c5f8.jsonl'],
    ['codex-0.154.0-real-subagent-metadata.jsonl', 'rollout-2026-10-27T19-43-50-01a17ff6-9bdc-7da2-9eea-21e205e2fb4e.jsonl'],
  ]) await writeFile(join(day, name), await readFile(new URL(`./fixtures/${fixture}`, import.meta.url)));
  const result = await scan(root);
  const totals = result.observations.map(item => [item.is_subagent, item.parent_session_id,
    item.input_tokens! + item.output_tokens! + item.cache_read_tokens! + item.cache_write_tokens!] as const);
  assert.equal(totals.length, 4);
  assert.equal(totals.reduce((sum, [, , count]) => sum + count, 0), 43952 + 16449);
  assert.equal(result.observations.reduce((sum, item) => sum + item.call_count!, 0), 4);
  assert.deepEqual(totals.filter(([child]) => child).map(([, parent, count]) => [parent, count]),
    [['01a17ff3-700b-76b2-be96-40479393c5f8', 16449]]);
  assert.deepEqual(result.supersededCanonicalIds, []);
  assert.deepEqual((await scan(root)).observations, result.observations);
});
