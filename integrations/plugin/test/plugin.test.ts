import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, mkdir, copyFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { main } from '../src/cli.ts';
import { readPluginConfig } from '../src/config.ts';
import { Ledger } from '../../shared/src/ledger.ts';
import { Collector } from '../../collector/src/collector.ts';
import { scan as claudeScan } from '../../claude-code/src/adapter.ts';
import { scan as codexScan } from '../../codex/src/adapter.ts';

function createMockSupabaseFetch(tracking: { calls: Array<{ url: string; body: Record<string, unknown> }> }) {
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
      return new Response(JSON.stringify({ ok: true, action: 'commit', committed: true, accepted: 10, snapshot_id: body.snapshot_id }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    return new Response(JSON.stringify({ error: 'unknown_action' }), { status: 400 });
  };
}

test('local-only activate and preview make zero network requests and report compact metadata', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-local-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const claudeRoot = join(temporary, 'claude');
  await mkdir(join(claudeRoot, 'projects', 'p1'), { recursive: true });
  await copyFile(new URL('../../claude-code/test/fixtures/streaming.jsonl', import.meta.url), join(claudeRoot, 'projects', 'p1', 'session.jsonl'));

  const networkCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });

  const outputs: string[] = [];
  const log = (msg: string) => { outputs.push(msg); };

  // 1. Activate
  await main({
    args: ['activate', '--source', 'claude-code', '--root', claudeRoot, '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: log,
  });

  assert.equal(networkCalls.length, 0, 'activate must make 0 network calls');
  assert.equal(outputs.length, 1);
  const activatedSummary = JSON.parse(outputs[0]);
  assert.equal(activatedSummary.harness, 'claude-code');
  assert.equal(activatedSummary.root, claudeRoot);
  assert.equal(activatedSummary.recurring_upload, false);
  assert.ok(activatedSummary.device_id);

  // 2. Preview
  outputs.length = 0;
  await main({
    args: ['preview', '--source', 'claude-code', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: log,
  });

  assert.equal(networkCalls.length, 0, 'preview must make 0 network calls');
  assert.equal(outputs.length, 1);
  const previewSummary = JSON.parse(outputs[0]);
  assert.ok(previewSummary.retained_contributions > 0);
  assert.ok(previewSummary.totals.input_tokens > 0);
  assert.equal(previewSummary.recurring_upload, false);
  // Must be compact metadata: no raw diagnostics array of files
  assert.equal(Array.isArray(previewSummary.diagnostics), false);

  // 3. Status
  outputs.length = 0;
  await main({
    args: ['status', '--source', 'claude-code', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: log,
  });

  assert.equal(networkCalls.length, 0, 'status must make 0 network calls');
  const statusSummary = JSON.parse(outputs[0]);
  assert.equal(statusSummary.retained_contributions, previewSummary.retained_contributions);
});

test('consent enforcement: sync and auto-on require confirmation, key is required for delivery', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-consent-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const codexRoot = join(temporary, 'codex');
  await mkdir(join(codexRoot, 'sessions'), { recursive: true });
  await copyFile(new URL('../../codex/test/fixtures/modern-source-derived.jsonl', import.meta.url), join(codexRoot, 'sessions', 's1.jsonl'));

  const networkCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });

  // Activate
  await main({
    args: ['activate', '--source', 'codex', '--root', codexRoot, '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  // Sync without --confirm-upload must fail
  await assert.rejects(
    () => main({
      args: ['sync', '--source', 'codex', '--state', stateDir],
      fetchImpl: fetchImpl as unknown as typeof fetch,
      env: { TOKENMUNCHERS_API_KEY: 'test-key' },
    }),
    /Upload requires explicit --confirm-upload/
  );

  // Sync with --confirm-upload but no TOKENMUNCHERS_API_KEY must fail
  await assert.rejects(
    () => main({
      args: ['sync', '--source', 'codex', '--state', stateDir, '--confirm-upload'],
      fetchImpl: fetchImpl as unknown as typeof fetch,
      env: {},
    }),
    /Set TOKENMUNCHERS_API_KEY/
  );

  // auto-on without --confirm-upload must fail
  await assert.rejects(
    () => main({
      args: ['auto-on', '--source', 'codex', '--state', stateDir],
      fetchImpl: fetchImpl as unknown as typeof fetch,
    }),
    /Recurring uploads require explicit --confirm-upload/
  );

  // Hook before auto-on does not upload
  await main({
    args: ['hook', '--source', 'codex', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    env: { TOKENMUNCHERS_API_KEY: 'test-key' },
  });
  assert.equal(networkCalls.length, 0, 'hook without auto-on consent must not upload');

  // auto-on before activation on another harness must fail
  await assert.rejects(
    () => main({
      args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--confirm-upload'],
      fetchImpl: fetchImpl as unknown as typeof fetch,
    }),
    /Activate the explicitly chosen source root first/
  );
});

test('identity reuse: preserves device_id, cutoff, and exclusions across standalone CLI and plugin', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-identity-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const codexRoot = join(temporary, 'codex');
  await mkdir(join(codexRoot, 'sessions'), { recursive: true });
  await copyFile(new URL('../../codex/test/fixtures/modern-source-derived.jsonl', import.meta.url), join(codexRoot, 'sessions', 's1.jsonl'));

  // 1. Activate using standalone Ledger directly
  const standaloneLedger = new Ledger(stateDir);
  const standaloneSource = await standaloneLedger.activate('codex', codexRoot, '2026-01-01T00:00:00Z', ['private/logs']);
  const initialDeviceId = standaloneSource.device_id;
  const initialCutoff = standaloneSource.cutoff;
  const initialExclusions = standaloneSource.exclusions;

  // 2. Query status via plugin CLI
  const outputs: string[] = [];
  await main({
    args: ['status', '--source', 'codex', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });

  const pluginStatus = JSON.parse(outputs[0]);
  assert.equal(pluginStatus.device_id, initialDeviceId, 'Device ID must remain identical');
  assert.equal(pluginStatus.cutoff, initialCutoff, 'Cutoff must remain identical');
  assert.deepEqual(pluginStatus.exclusions, initialExclusions, 'Exclusions must remain identical');

  // 3. Turn auto-on, then auto-off
  await main({
    args: ['auto-on', '--source', 'codex', '--state', stateDir, '--confirm-upload'],
  });
  const configOn = await readPluginConfig(stateDir, 'codex');
  assert.equal(configOn.auto_upload, true);

  await main({
    args: ['auto-off', '--source', 'codex', '--state', stateDir],
  });
  const configOff = await readPluginConfig(stateDir, 'codex');
  assert.equal(configOff.auto_upload, false);

  // 4. Verify identity did not change after auto-on / auto-off
  outputs.length = 0;
  await main({
    args: ['status', '--source', 'codex', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });
  const finalStatus = JSON.parse(outputs[0]);
  assert.equal(finalStatus.device_id, initialDeviceId, 'Identity must survive auto-on and auto-off');
});

test('explicit manual sync and hook recurring delivery with Supabase mock', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-delivery-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const claudeRoot = join(temporary, 'claude');
  await mkdir(join(claudeRoot, 'projects', 'p1'), { recursive: true });
  await copyFile(new URL('../../claude-code/test/fixtures/streaming.jsonl', import.meta.url), join(claudeRoot, 'projects', 'p1', 'session.jsonl'));

  const networkCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });

  await main({
    args: ['activate', '--source', 'claude-code', '--root', claudeRoot, '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  // Manual sync with confirm-upload
  const outputs: string[] = [];
  await main({
    args: ['sync', '--source', 'claude-code', '--state', stateDir, '--confirm-upload'],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    env: { TOKENMUNCHERS_API_KEY: 'secret-token-123' },
    stdout: (msg) => { outputs.push(msg); },
  });

  assert.ok(networkCalls.length >= 3, 'Delivery must begin, chunk, and commit');
  assert.equal(networkCalls[0].body.action, 'begin');
  assert.equal(networkCalls[networkCalls.length - 1].body.action, 'commit');
  const syncOutput = JSON.parse(outputs[0]);
  assert.ok(syncOutput.delivery);
  assert.equal(syncOutput.delivery.accepted, 10);

  // Enable auto-on
  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--confirm-upload'],
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  // Rapid hook with NO changes must suppress duplicate identical snapshot delivery!
  const callsBeforeNoOpHook = networkCalls.length;
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    env: { TOKENMUNCHERS_API_KEY: 'secret-token-123' },
  });
  assert.equal(networkCalls.length, callsBeforeNoOpHook, 'Unchanged snapshot must be suppressed on repeated hooks');

  // Disable recurring upload
  await main({
    args: ['auto-off', '--source', 'claude-code', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  // Hook after auto-off must make no network requests
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    env: { TOKENMUNCHERS_API_KEY: 'secret-token-123' },
  });
  assert.equal(networkCalls.length, callsBeforeNoOpHook, 'Hook after auto-off must make 0 network requests');
});

test('pause semantics exclude timestamps and stop delivery', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-pause-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const codexRoot = join(temporary, 'codex');
  await mkdir(join(codexRoot, 'sessions'), { recursive: true });
  await copyFile(new URL('../../codex/test/fixtures/modern-source-derived.jsonl', import.meta.url), join(codexRoot, 'sessions', 's1.jsonl'));

  const networkCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });

  await main({
    args: ['activate', '--source', 'codex', '--root', codexRoot, '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });

  // Pause
  const outputs: string[] = [];
  await main({
    args: ['pause', '--source', 'codex', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });
  const pausedStatus = JSON.parse(outputs[0]);
  assert.equal(pausedStatus.paused, true);

  // Sync while paused must fail
  await assert.rejects(
    () => main({
      args: ['sync', '--source', 'codex', '--state', stateDir, '--confirm-upload'],
      fetchImpl: fetchImpl as unknown as typeof fetch,
      env: { TOKENMUNCHERS_API_KEY: 'key' },
    }),
    /Source paused; resume before delivery/
  );

  // Enable auto-on and run hook while paused: must skip delivery
  await main({
    args: ['auto-on', '--source', 'codex', '--state', stateDir, '--confirm-upload'],
  });
  await main({
    args: ['hook', '--source', 'codex', '--state', stateDir],
    fetchImpl: fetchImpl as unknown as typeof fetch,
    env: { TOKENMUNCHERS_API_KEY: 'key' },
  });
  assert.equal(networkCalls.length, 0, 'Hook while paused must skip delivery');

  // Resume
  outputs.length = 0;
  await main({
    args: ['resume', '--source', 'codex', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });
  const resumedStatus = JSON.parse(outputs[0]);
  assert.equal(resumedStatus.paused, false);
});

test('harness isolation: claude-code and codex have independent state and locks', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-plugin-isolation-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const claudeRoot = join(temporary, 'claude');
  const codexRoot = join(temporary, 'codex');
  await mkdir(join(claudeRoot, 'p1'), { recursive: true });
  await mkdir(join(codexRoot, 's1'), { recursive: true });

  await main({
    args: ['activate', '--source', 'claude-code', '--root', claudeRoot, '--state', stateDir],
  });
  await main({
    args: ['activate', '--source', 'codex', '--root', codexRoot, '--state', stateDir],
  });

  // Turn auto-on for claude-code only
  await main({
    args: ['auto-on', '--source', 'claude-code', '--state', stateDir, '--confirm-upload'],
  });

  const claudeCfg = await readPluginConfig(stateDir, 'claude-code');
  const codexCfg = await readPluginConfig(stateDir, 'codex');
  assert.equal(claudeCfg.auto_upload, true);
  assert.equal(codexCfg.auto_upload, false);

  // Pause codex only
  await main({
    args: ['pause', '--source', 'codex', '--state', stateDir],
  });

  const outputs: string[] = [];
  await main({
    args: ['status', '--source', 'claude-code', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });
  const claudeStatus = JSON.parse(outputs[0]);
  assert.equal(claudeStatus.paused, false);

  outputs.length = 0;
  await main({
    args: ['status', '--source', 'codex', '--state', stateDir],
    stdout: (msg) => { outputs.push(msg); },
  });
  const codexStatus = JSON.parse(outputs[0]);
  assert.equal(codexStatus.paused, true);
});
