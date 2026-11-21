import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, mkdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { main, sanitizeErrorMessage, computeOutboundRevision } from '../src/cli.ts';
import { readPluginConfig, writePluginConfig, validateEndpoint } from '../src/config.ts';
import { Ledger } from '../../shared/src/ledger.ts';
import type { Observation } from '../../shared/src/observations.ts';

interface RecordedCall {
  url: string;
  body: Record<string, unknown>;
}

function createMockSupabaseFetch(tracking: { calls: RecordedCall[] }) {
  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input.toString();
    const bodyStr = init?.body ? String(init.body) : '{}';
    const body = JSON.parse(bodyStr) as Record<string, unknown>;
    tracking.calls.push({ url, body });

    const action = body.action;
    if (action === 'begin') {
      return new Response(JSON.stringify({ ok: true, action: 'begin', snapshot_id: body.snapshot_id }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (action === 'chunk') {
      return new Response(JSON.stringify({ ok: true, action: 'chunk', snapshot_id: body.snapshot_id, index: body.index }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (action === 'commit') {
      const chunks = tracking.calls.filter(c => c.body.action === 'chunk' && c.body.snapshot_id === body.snapshot_id);
      const obsCount = chunks.reduce((acc, c) => acc + ((c.body.observations as unknown[])?.length ?? 0), 0);
      return new Response(JSON.stringify({ ok: true, action: 'commit', committed: true, accepted: obsCount, snapshot_id: body.snapshot_id }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    return new Response(JSON.stringify({ error: 'unknown_action' }), { status: 400 });
  };
}

function makeObservation(overrides: Partial<Observation> = {}): Observation {
  return {
    harness: 'claude-code',
    canonical_id: 'test-obs-1',
    identity_strength: 'response',
    grain: 'request',
    timestamp: '2026-10-27T10:00:00.000Z',
    interval_start: null,
    interval_end: null,
    provider: 'anthropic',
    model: 'claude-3-5-sonnet',
    session_id: 'session-1',
    parent_session_id: null,
    is_subagent: false,
    input_tokens: 100,
    output_tokens: 50,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    call_count: 1,
    cost_usd: 0.005,
    cost_basis: 'reported',
    quality: 'observed',
    diagnostics: [],
    ...overrides,
  };
}

test('same-count model correction publishes and updates outbound revision', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-model-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  await ledger.transaction(state => {
    state.sources['claude-code']!.records['test-obs-1'] = makeObservation({ model: 'claude-3-5-sonnet' });
  });

  const calls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls });

  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'http://127.0.0.1:8080/functions/v1/contributions', '--confirm-upload'],
  });

  // First hook execution: publishes initial model
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  assert.equal(calls.filter(c => c.body.action === 'commit').length, 1);
  const chunk1 = calls.find(c => c.body.action === 'chunk');
  assert.ok(chunk1);
  const obsList1 = chunk1.body.observations as Array<{ model: string }>;
  assert.equal(obsList1[0].model, 'claude-3-5-sonnet');

  calls.length = 0;

  // Immediate second hook execution: unchanged snapshot suppressed
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0, 'Unchanged snapshot must make zero network requests');

  // Correct model without changing record count or canonical ID
  await ledger.transaction(state => {
    state.sources['claude-code']!.records['test-obs-1'] = makeObservation({ model: 'claude-3-7-sonnet' });
  });

  // Third hook execution: same-count model correction MUST publish
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  assert.equal(calls.filter(c => c.body.action === 'commit').length, 1, 'Model correction must trigger publication');
  const chunk2 = calls.find(c => c.body.action === 'chunk');
  assert.ok(chunk2);
  const obsList2 = chunk2.body.observations as Array<{ model: string }>;
  assert.equal(obsList2[0].model, 'claude-3-7-sonnet');

  calls.length = 0;

  // Fourth hook execution: now suppressed
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0);
});

test('local-only diagnostic changes do not republish', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-diag-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  await ledger.transaction(state => {
    state.sources['claude-code']!.records['test-obs-1'] = makeObservation({ diagnostics: [] });
  });

  const calls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls });

  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'http://127.0.0.1:8080/functions/v1/contributions', '--confirm-upload'],
  });

  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.filter(c => c.body.action === 'commit').length, 1);

  calls.length = 0;

  // Add local-only diagnostic codes
  await ledger.transaction(state => {
    state.sources['claude-code']!.records['test-obs-1'].diagnostics = ['CANONICAL_CONFLICT', 'RATE_LIMITED'];
  });

  // Hook run: outbound hash should be identical because outbound() strips diagnostics
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0, 'Local-only diagnostic changes must not trigger republishing');
});

test('older pending snapshot followed by newer scanned data is not lost and sync catches up', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-pending-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  const obs1 = makeObservation({ canonical_id: 'obs-1', timestamp: '2026-10-27T08:00:00.000Z' });
  const obs2 = makeObservation({ canonical_id: 'obs-2', timestamp: '2026-10-27T08:30:00.000Z' });
  const pendingSnapshotId = '11111111-1111-4111-8111-111111111111';

  // Simulate an older pending snapshot staged with only obs1
  await ledger.transaction(state => {
    const src = state.sources['claude-code']!;
    src.records['obs-1'] = obs1;
    src.pending = {
      snapshot_id: pendingSnapshotId,
      observations: [obs1],
      superseded_ids: [],
      identity_aliases: [],
    };
    // Ledger records now also have freshly scanned obs2
    src.records['obs-2'] = obs2;
  });

  const calls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls });

  // Native manual sync should finish outstanding pending snapshot then publish freshly scanned latest state
  await main({
    args: ['sync', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'http://127.0.0.1:8080/functions/v1/contributions', '--confirm-upload'],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  const commits = calls.filter(c => c.body.action === 'commit');
  assert.equal(commits.length, 2, 'Manual sync must finish pending snapshot then publish latest state');

  // Verify first commit finished the older pending snapshot
  assert.equal(commits[0].body.snapshot_id, pendingSnapshotId);
  const firstChunk = calls.find(c => c.body.action === 'chunk' && c.body.snapshot_id === pendingSnapshotId);
  assert.ok(firstChunk);
  const firstObs = firstChunk.body.observations as Array<{ canonical_id: string }>;
  assert.deepEqual(firstObs.map(o => o.canonical_id), ['obs-1']);

  // Verify second commit published the newer full state
  const secondSnapshotId = commits[1].body.snapshot_id as string;
  assert.notEqual(secondSnapshotId, pendingSnapshotId);
  const secondChunk = calls.find(c => c.body.action === 'chunk' && c.body.snapshot_id === secondSnapshotId);
  assert.ok(secondChunk);
  const secondObs = secondChunk.body.observations as Array<{ canonical_id: string }>;
  assert.deepEqual(secondObs.map(o => o.canonical_id), ['obs-1', 'obs-2']);

  // Check persisted config revision matches current ledger revision for target endpoint
  const config = await readPluginConfig(stateDir, 'claude-code');
  const currentOutbound = await ledger.transaction(state => computeOutboundRevision(state.sources['claude-code']!));
  const endpointUrl = 'http://127.0.0.1:8080/functions/v1/contributions';
  assert.equal(config.last_delivered_revisions?.[endpointUrl], currentOutbound.revision);

  // Subsequent hook should make zero requests
  calls.length = 0;
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'http://127.0.0.1:8080/functions/v1/contributions'],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0);
});

test('explicitly switching destination publishes existing data without false suppression', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-dest-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  await ledger.transaction(state => {
    state.sources['claude-code']!.records['test-obs-1'] = makeObservation();
  });

  const calls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls });

  const endpointA = 'http://127.0.0.1:8080/destination-a';
  const endpointB = 'http://127.0.0.1:8080/destination-b';
  const endpointC = 'http://127.0.0.1:8080/destination-c';

  // 1. Configure and publish to endpoint A
  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', endpointA, '--confirm-upload'],
  });
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  assert.equal(calls.filter(c => c.url === endpointA && c.body.action === 'commit').length, 1);
  calls.length = 0;

  // Subsequent hook to endpoint A is suppressed
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0);

  // 2. Switch configured destination to endpoint B
  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', endpointB, '--confirm-upload'],
  });

  // Hook targeting endpoint B MUST publish existing data (not falsely suppressed by endpoint A delivery)
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  assert.equal(calls.filter(c => c.url === endpointB && c.body.action === 'commit').length, 1);
  calls.length = 0;

  // 3. One-off manual sync to a different endpoint C does not suppress configured endpoint B
  const syncOutputs: string[] = [];
  await main({
    args: ['sync', '--source', 'claude-code', '--state', stateDir, '--endpoint', endpointC, '--confirm-upload'],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: (msg) => { syncOutputs.push(msg); },
  });
  assert.equal(calls.filter(c => c.url === endpointC && c.body.action === 'commit').length, 1);
  calls.length = 0;

  // Manual one-off output endpoint equals actual requested endpoint
  assert.equal(syncOutputs.length, 1);
  const syncStatus = JSON.parse(syncOutputs[0]);
  assert.equal(syncStatus.endpoint, endpointC, 'Manual one-off output endpoint must equal actual requested endpoint');

  // Configured recurring endpoint remains unchanged
  const persistedConfig = await readPluginConfig(stateDir, 'claude-code');
  assert.equal(persistedConfig.endpoint, endpointB, 'Configured recurring endpoint must remain unchanged');

  // Hook run with default configured endpoint (which is endpoint B) remains settled
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
  assert.equal(calls.length, 0, 'Configured endpoint B must remain settled at its published revision');
});

test('persisted endpoint validation enforces HTTPS and rejects credential-bearing URLs', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-val-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  // Rejects credentials in URL
  await assert.rejects(
    () => main({
      args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'https://user:pass@example.com/api', '--confirm-upload'],
    }),
    /credentials/
  );

  await assert.rejects(
    () => main({
      args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'https://token@127.0.0.1:8080/api', '--confirm-upload'],
    }),
    /credentials/
  );

  // Rejects non-HTTPS for remote hosts
  await assert.rejects(
    () => main({
      args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--endpoint', 'http://remote.tokenmunchers.com/api', '--confirm-upload'],
    }),
    /HTTPS/
  );

  // Direct validateEndpoint unit checks
  assert.throws(() => validateEndpoint('https://api_key:secret@host.com/endpoint'), /credentials/);
  assert.throws(() => validateEndpoint('http://insecure-domain.org/ingest'), /HTTPS/);
  assert.doesNotThrow(() => validateEndpoint('http://localhost:54321/functions/v1/contributions'));
  assert.doesNotThrow(() => validateEndpoint('http://127.0.0.1:8080/functions/v1/contributions'));
  assert.doesNotThrow(() => validateEndpoint('http://[::1]:8080/functions/v1/contributions'));
  assert.doesNotThrow(() => validateEndpoint('https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions'));

  // Direct writePluginConfig rejects credentialed endpoint
  await assert.rejects(
    () => writePluginConfig(stateDir, 'claude-code', { auto_upload: true, endpoint: 'https://key:pass@example.com' }),
    /credentials/
  );
});

test('error redaction protects environment key, tm_ keys, and suppresses raw invalid config JSON', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-redact-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');

  // 1. Redaction of actual environment key
  const envKey = 'custom-env-secret-999888';
  const sanitizedEnv = sanitizeErrorMessage(`Failed to send data with key ${envKey} to upstream`, {
    TOKENMUNCHERS_API_KEY: envKey,
  });
  assert.equal(sanitizedEnv.includes(envKey), false);
  assert.ok(sanitizedEnv.includes('[REDACTED]'));

  // 2. Redaction of standard tm_ key form
  const tmKey = 'tm_live_abcdef1234567890abcdef1234567890abcdef12';
  const sanitizedTm = sanitizeErrorMessage(`Unauthorized key: ${tmKey}`);
  assert.equal(sanitizedTm.includes(tmKey), false);
  assert.ok(sanitizedTm.includes('[REDACTED]'));

  // 3. Redaction of Bearer tokens and URL credentials
  const sanitizedBearer = sanitizeErrorMessage('Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-ID');
  assert.equal(sanitizedBearer, 'Bearer [REDACTED]');

  const sanitizedUrl = sanitizeErrorMessage('Error connecting to https://admin:supersecret@endpoint.com/test');
  assert.equal(sanitizedUrl.includes('supersecret'), false);
  assert.ok(sanitizedUrl.includes('https://[REDACTED]:[REDACTED]@endpoint.com/test'));

  // 4. Invalid config JSON does not echo raw file contents
  await mkdir(stateDir, { recursive: true });
  const configPath = join(stateDir, 'claude-code-plugin.json');
  await writeFile(configPath, '{\n  "apiKey": "super_secret_token_12345",\n  invalid syntax here\n}\n', 'utf8');

  await assert.rejects(
    () => readPluginConfig(stateDir, 'claude-code'),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.equal(err.message.includes('super_secret_token_12345'), false, 'Must not echo config contents');
      assert.ok(err.message.includes('Invalid plugin configuration file'), 'Must report generic actionable error');
      return true;
    }
  );
});

test('CLI hook handles child stdio ignore without unref TypeError', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-pub-stdio-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'root');
  await mkdir(dummyRoot, { recursive: true });

  const ledger = new Ledger(stateDir);
  await ledger.activate('claude-code', dummyRoot);

  const cliPath = fileURLToPath(new URL('../src/cli.ts', import.meta.url));

  const child = spawn(
    process.execPath,
    [cliPath, 'hook', '--source', 'claude-code', '--state', stateDir],
    {
      stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, TOKENMUNCHERS_API_KEY: 'tm_synthetic_test_key_1234567890' },
    }
  );

  let stderr = '';
  child.stderr.on('data', chunk => { stderr += String(chunk); });

  const { promise, resolve } = Promise.withResolvers<number | null>();
  child.on('exit', code => resolve(code));
  const exitCode = await promise;

  assert.equal(exitCode, 0, `Child process exited with code ${exitCode}, stderr: ${stderr}`);
  assert.equal(stderr.includes('TypeError'), false, 'Must not throw TypeError on stdin unref');
});
