import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, mkdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { main } from '../src/cli.ts';
import { Ledger } from '../../shared/src/ledger.ts';

interface RecordedCall {
  url: string;
  body: Record<string, unknown>;
}

function createMockSupabaseFetch(tracking: { calls: RecordedCall[] }) {
  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input.toString();
    const bodyStr = init?.body ? String(init.body) : '{}';
    let body: Record<string, unknown> = {};
    try {
      body = JSON.parse(bodyStr) as Record<string, unknown>;
    } catch {
      // ignore
    }
    tracking.calls.push({ url, body });
    return new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };
}

test('hook fails with sanitized actionable error when ledger SQLite file is corrupted', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-hook-error-corrupt-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  await mkdir(stateDir, { recursive: true, mode: 0o700 });

  // Write isolated actual corrupted SQLite database bytes to ledger.sqlite
  const ledgerPath = join(stateDir, 'ledger.sqlite');
  await writeFile(ledgerPath, Buffer.from('NOT A VALID SQLITE DATABASE FILE GARBAGE CONTENT'));

  const networkCalls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });
  const outputs: string[] = [];
  const secretToken = 'tm_secret_token_corrupted_test_key_12345';
  let caughtError: unknown = null;

  await assert.rejects(
    async () => {
      await main({
        args: ['hook', '--source', 'claude-code', '--state', stateDir],
        env: { TOKENMUNCHERS_API_KEY: secretToken },
        fetchImpl: fetchImpl as unknown as typeof fetch,
        stdout: (msg) => outputs.push(msg),
      });
    },
    (err: unknown) => {
      caughtError = err;
      assert(err instanceof Error, 'Error should be an instance of Error');
      assert(!err.message.includes(secretToken), 'Error message must not leak credentials');
      assert(err.message.length > 0, 'Error message must be non-empty and actionable');
      return true;
    }
  );

  assert(caughtError !== null, 'Hook must reject when ledger file is corrupted');
  assert.equal(networkCalls.length, 0, 'Corrupted ledger hook must not trigger network uploads');
});

test('unactivated source on existing valid ledger is quiet no-op without network activity', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-hook-unactivated-valid-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');
  const dummyRoot = join(temporary, 'codex-root');
  await mkdir(dummyRoot, { recursive: true });

  // Initialize valid ledger with codex activated
  const ledger = new Ledger(stateDir);
  await ledger.activate('codex', dummyRoot);

  const networkCalls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });
  const outputs: string[] = [];

  // claude-code is unactivated on this valid existing ledger
  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'test-token' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: (msg) => outputs.push(msg),
  });

  assert.equal(networkCalls.length, 0, 'Unactivated source hook must make zero network requests');
  assert.equal(outputs.length, 0, 'Unactivated source hook must be quiet no-op');
});

test('unactivated source on fresh state directory is quiet no-op without network activity', async t => {
  const temporary = await mkdtemp(join(tmpdir(), 'tm-hook-fresh-'));
  t.after(() => rm(temporary, { recursive: true, force: true }));

  const stateDir = join(temporary, 'state');

  const networkCalls: RecordedCall[] = [];
  const fetchImpl = createMockSupabaseFetch({ calls: networkCalls });
  const outputs: string[] = [];

  await main({
    args: ['hook', '--source', 'claude-code', '--state', stateDir],
    env: { TOKENMUNCHERS_API_KEY: 'test-token' },
    fetchImpl: fetchImpl as unknown as typeof fetch,
    stdout: (msg) => outputs.push(msg),
  });

  assert.equal(networkCalls.length, 0, 'Fresh unactivated source hook must make zero network requests');
  assert.equal(outputs.length, 0, 'Fresh unactivated source hook must be quiet no-op');
});
