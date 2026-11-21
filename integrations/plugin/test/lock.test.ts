import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, stat, chmod, writeFile, readFile, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn, type ChildProcess } from 'node:child_process';
import { withLock, lockFilePath } from '../src/lock.ts';
import { main } from '../src/cli.ts';

const lockModuleUrl = new URL('../src/lock.ts', import.meta.url).href;

interface ManagedChild {
  readonly process: ChildProcess;
  waitForOutput(target: string, timeoutMs?: number): Promise<string>;
  waitForExit(timeoutMs?: number): Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  release(timeoutMs?: number): Promise<void>;
  kill(signal?: NodeJS.Signals, timeoutMs?: number): Promise<void>;
  sendSignal(signal: NodeJS.Signals): boolean;
  isExited(): boolean;
  getStdout(): string;
  getStderr(): string;
}

const managedByChild = new WeakMap<ChildProcess, ManagedChild>();

function createManagedChild(child: ChildProcess): ManagedChild {
  let stdoutBuf = '';
  let stderrBuf = '';
  let exited = child.exitCode !== null || child.signalCode !== null;
  let closed = false;
  let exitCode: number | null = child.exitCode;
  let exitSignal: NodeJS.Signals | null = child.signalCode;

  type OutputWaiter = {
    target: string;
    resolve: (buf: string) => void;
    reject: (err: Error) => void;
    timer?: NodeJS.Timeout;
  };
  const outputWaiters = new Set<OutputWaiter>();
  type ExitWaiter = {
    resolve: (res: { code: number | null; signal: NodeJS.Signals | null }) => void;
    reject: (err: Error) => void;
    timer?: NodeJS.Timeout;
  };
  const exitWaiters = new Set<ExitWaiter>();

  const checkOutputWaiters = () => {
    for (const waiter of outputWaiters) {
      if (stdoutBuf.includes(waiter.target)) {
        clearTimeout(waiter.timer);
        outputWaiters.delete(waiter);
        waiter.resolve(stdoutBuf);
      }
    }
  };

  const resolveExitWaiters = (res: { code: number | null; signal: NodeJS.Signals | null }) => {
    for (const waiter of exitWaiters) {
      clearTimeout(waiter.timer);
      exitWaiters.delete(waiter);
      waiter.resolve(res);
    }
  };

  const rejectRemainingOutputWaiters = () => {
    for (const waiter of outputWaiters) {
      clearTimeout(waiter.timer);
      outputWaiters.delete(waiter);
      waiter.reject(
        new Error(
          `Child process terminated (code=${exitCode}, signal=${exitSignal}) without emitting "${waiter.target}".\n` +
          `STDOUT: ${stdoutBuf}\nSTDERR: ${stderrBuf}`
        )
      );
    }
  };

  child.stdout?.on('data', (chunk: Buffer | string) => {
    stdoutBuf += chunk.toString();
    checkOutputWaiters();
  });

  child.stderr?.on('data', (chunk: Buffer | string) => {
    stderrBuf += chunk.toString();
  });

  child.on('error', (err: Error) => {
    for (const waiter of outputWaiters) {
      clearTimeout(waiter.timer);
      outputWaiters.delete(waiter);
      waiter.reject(err);
    }
    for (const waiter of exitWaiters) {
      clearTimeout(waiter.timer);
      exitWaiters.delete(waiter);
      waiter.reject(err);
    }
  });

  child.on('exit', (code, signal) => {
    exited = true;
    exitCode = code;
    exitSignal = signal;
    checkOutputWaiters();
    resolveExitWaiters({ code, signal });
  });

  child.on('close', (code, signal) => {
    closed = true;
    if (!exited) {
      exited = true;
      exitCode = code;
      exitSignal = signal;
    }
    checkOutputWaiters();
    resolveExitWaiters({ code: exitCode, signal: exitSignal });
    rejectRemainingOutputWaiters();
  });

  const isExited = () => exited || closed || child.exitCode !== null || child.signalCode !== null;

  const waitForOutput = (target: string, timeoutMs = 10_000): Promise<string> => {
    if (stdoutBuf.includes(target)) {
      return Promise.resolve(stdoutBuf);
    }
    if (closed) {
      return Promise.reject(
        new Error(
          `Child process already terminated (code=${exitCode}, signal=${exitSignal}) without emitting "${target}".\n` +
          `STDOUT: ${stdoutBuf}\nSTDERR: ${stderrBuf}`
        )
      );
    }

    const { promise, resolve, reject } = Promise.withResolvers<string>();
    const waiter: OutputWaiter = { target, resolve, reject };

    const timer = setTimeout(() => {
      outputWaiters.delete(waiter);
      reject(
        new Error(
          `Timed out after ${timeoutMs}ms waiting for "${target}" from child.\n` +
          `STDOUT: ${stdoutBuf}\nSTDERR: ${stderrBuf}`
        )
      );
    }, timeoutMs);
    timer.unref();
    waiter.timer = timer;

    outputWaiters.add(waiter);
    return promise;
  };

  const waitForExit = (timeoutMs = 10_000): Promise<{ code: number | null; signal: NodeJS.Signals | null }> => {
    if (isExited()) {
      return Promise.resolve({ code: exitCode, signal: exitSignal });
    }

    const { promise, resolve, reject } = Promise.withResolvers<{
      code: number | null;
      signal: NodeJS.Signals | null;
    }>();
    const waiter: ExitWaiter = { resolve, reject };

    const timer = setTimeout(() => {
      exitWaiters.delete(waiter);
      reject(
        new Error(
          `Timed out after ${timeoutMs}ms waiting for child to exit.\n` +
          `STDOUT: ${stdoutBuf}\nSTDERR: ${stderrBuf}`
        )
      );
    }, timeoutMs);
    timer.unref();
    waiter.timer = timer;

    exitWaiters.add(waiter);
    return promise;
  };

  const release = async (timeoutMs = 10_000): Promise<void> => {
    if (isExited()) return;
    try {
      child.stdin?.write('RELEASE\n');
    } catch {
      // Stdin may be closed if process is terminating
    }
    await waitForOutput('RELEASED\n', timeoutMs);
    await waitForExit(timeoutMs);
  };

  const kill = async (signal: NodeJS.Signals = 'SIGKILL', timeoutMs = 5_000): Promise<void> => {
    if (isExited()) return;
    try {
      child.kill(signal);
    } catch {
      // Child process already dead
    }
    try {
      await waitForExit(timeoutMs);
    } catch {
      if (!isExited() && signal !== 'SIGKILL') {
        try {
          child.kill('SIGKILL');
        } catch {
          // Process might already be dead
        }
        await waitForExit(2_000).catch(() => {});
      }
    }
  };

  const sendSignal = (signal: NodeJS.Signals): boolean => {
    if (isExited()) return false;
    return child.kill(signal);
  };

  const managed: ManagedChild = {
    process: child,
    waitForOutput,
    waitForExit,
    release,
    kill,
    sendSignal,
    isExited,
    getStdout: () => stdoutBuf,
    getStderr: () => stderrBuf,
  };

  managedByChild.set(child, managed);
  return managed;
}

function getManagedChild(childOrManaged: ManagedChild | ChildProcess): ManagedChild {
  if ('waitForOutput' in childOrManaged) {
    return childOrManaged;
  }
  let managed = managedByChild.get(childOrManaged);
  if (!managed) {
    managed = createManagedChild(childOrManaged);
  }
  return managed;
}

function waitForChildOutput(
  child: ManagedChild | ChildProcess,
  target: string,
  timeoutMs?: number
): Promise<string> {
  return getManagedChild(child).waitForOutput(target, timeoutMs);
}

function releaseChild(child: ManagedChild | ChildProcess, timeoutMs?: number): Promise<void> {
  return getManagedChild(child).release(timeoutMs);
}

function killChild(
  child: ManagedChild | ChildProcess,
  signal: NodeJS.Signals = 'SIGKILL',
  timeoutMs?: number
): Promise<void> {
  return getManagedChild(child).kill(signal, timeoutMs);
}

function spawnLockHolder(options: {
  stateDir: string;
  harness: string;
  interceptSigterm?: boolean;
}): ManagedChild {
  const code = `
    import { withLock } from ${JSON.stringify(lockModuleUrl)};

    if (${JSON.stringify(Boolean(options.interceptSigterm))}) {
      process.on('SIGTERM', () => {
        process.stdout.write("SIGTERM_RECEIVED\\n");
      });
    }

    process.stdout.write("LAUNCHED\\n");
    await withLock(
      ${JSON.stringify(options.stateDir)},
      ${JSON.stringify(options.harness)},
      { mode: 'wait' },
      async () => {
        process.stdout.write("ACQUIRED\\n");
        const { promise, resolve } = Promise.withResolvers();
        process.stdin.once('data', () => {
          process.stdout.write("RELEASING\\n");
          resolve(undefined);
        });
        await promise;
      }
    );
    process.stdout.write("RELEASED\\n");
    process.exit(0);
  `;

  const child = spawn(process.execPath, ['--input-type=module', '-e', code], {
    stdio: ['pipe', 'pipe', 'pipe'],
  });

  return createManagedChild(child);
}

test(
  'overlapping hook skips: concurrent native hooks do not block or throw when lock is held',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-hook-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    // Activate harness first so hook CLI command has initialized state
    await main({
      args: ['activate', '--source', 'claude-code', '--root', tempDir, '--state', stateDir],
    });

    const child = spawnLockHolder({ stateDir, harness: 'claude-code' });
    t.after(() => killChild(child));

    await waitForChildOutput(child, 'ACQUIRED\n');

    // Direct withLock skip check
    const lockResult = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => {
      throw new Error('Critical section must not execute while lock is held');
    });
    assert.equal(lockResult.acquired, false, 'withLock in skip-if-busy mode must return acquired:false');

    // Consumer-visible CLI hook check
    await main({
      args: ['hook', '--source', 'claude-code', '--state', stateDir],
    });

    await releaseChild(child);

    // After lock is released, hook acquires lock normally
    const postResult = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => {
      return 'ok';
    });
    assert.equal(postResult.acquired, true);
    assert.equal(postResult.result, 'ok');
  }
);

test(
  'command waits and bounded timeout: commands wait for lock and time out with actionable error',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-wait-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    // 1. Command waits until release and then succeeds
    const child1 = spawnLockHolder({ stateDir, harness: 'claude-code' });
    t.after(() => killChild(child1));
    await waitForChildOutput(child1, 'ACQUIRED\n');

    let waitedResult: string | undefined;
    const waitingPromise = withLock(
      stateDir,
      'claude-code',
      { mode: 'wait', timeoutMs: 5000, pollIntervalMs: 25 },
      async () => {
        waitedResult = 'acquired-after-wait';
        return waitedResult;
      }
    );

    // Deterministically release the holder while waitingPromise is blocked
    await releaseChild(child1);

    const res = await waitingPromise;
    assert.equal(res.acquired, true);
    assert.equal(res.result, 'acquired-after-wait');
    assert.equal(waitedResult, 'acquired-after-wait');

    // 2. Command bounded timeout when lock holder does not release
    const child2 = spawnLockHolder({ stateDir, harness: 'claude-code' });
    t.after(() => killChild(child2));
    await waitForChildOutput(child2, 'ACQUIRED\n');

    await assert.rejects(
      () =>
        withLock(
          stateDir,
          'claude-code',
          { mode: 'wait', timeoutMs: 80, pollIntervalMs: 15 },
          async () => 'never'
        ),
      (err: unknown) => {
        assert.ok(err instanceof Error, 'Expected rejection to be an Error instance');
        assert.match(err.message, /claude-code/i, 'Error message should identify the blocked harness');
        return true;
      }
    );

    await releaseChild(child2);
  }
);

test(
  'killed holder recovers: lock is immediately released on process death without permanent block',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-kill-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    const child = spawnLockHolder({ stateDir, harness: 'claude-code' });
    t.after(() => killChild(child));
    await waitForChildOutput(child, 'ACQUIRED\n');

    // Verify lock is currently held
    const busyResult = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => 'nope');
    assert.equal(busyResult.acquired, false);

    // Kill holder process abruptly with SIGKILL
    await killChild(child, 'SIGKILL');

    // Immediately after process termination, SQLite/OS file lock must be cleared
    const recoveryResult = await withLock(
      stateDir,
      'claude-code',
      { mode: 'wait', timeoutMs: 2000 },
      async () => {
        return 'recovered-successfully';
      }
    );

    assert.equal(recoveryResult.acquired, true);
    assert.equal(recoveryResult.result, 'recovered-successfully');
  }
);

test(
  'active critical sections never overlap across concurrent child processes',
  { timeout: 20_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-concurrency-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');
    const sharedLog = join(tempDir, 'concurrency.json');
    await writeFile(sharedLog, JSON.stringify({ inCritical: 0, maxConcurrent: 0, totalRuns: 0 }), 'utf8');

    // Run 3 child workers contending for the lock
    const workerCount = 3;
    const workers: ManagedChild[] = [];

    const workerCode = `
    import { withLock } from ${JSON.stringify(lockModuleUrl)};
    import { readFile, writeFile } from 'node:fs/promises';
    import { setTimeout as delay } from 'node:timers/promises';

    const logFile = ${JSON.stringify(sharedLog)};

    for (let round = 0; round < 3; round++) {
      await withLock(${JSON.stringify(stateDir)}, 'claude-code', { mode: 'wait', timeoutMs: 10000, pollIntervalMs: 20 }, async () => {
        const raw = await readFile(logFile, 'utf8');
        const state = JSON.parse(raw);
        if (state.inCritical !== 0) {
          throw new Error("Concurrency violation: inCritical is " + state.inCritical);
        }
        state.inCritical++;
        state.maxConcurrent = Math.max(state.maxConcurrent, state.inCritical);
        state.totalRuns++;
        await writeFile(logFile, JSON.stringify(state), 'utf8');

        // Multi-process integration: genuine platform delay keeps lock held during sibling contention
        await delay(30);

        const checkRaw = await readFile(logFile, 'utf8');
        const checkState = JSON.parse(checkRaw);
        if (checkState.inCritical !== 1) {
          throw new Error("Concurrency violation inside critical section: inCritical is " + checkState.inCritical);
        }
        checkState.inCritical--;
        await writeFile(logFile, JSON.stringify(checkState), 'utf8');
      });
    }
    process.stdout.write("COMPLETED\\n");
  `;

    for (let i = 0; i < workerCount; i++) {
      const rawChild = spawn(process.execPath, ['--input-type=module', '-e', workerCode], {
        stdio: ['pipe', 'pipe', 'pipe'],
      });
      const child = createManagedChild(rawChild);
      t.after(() => killChild(child));
      workers.push(child);
    }

    // Await all workers to complete
    await Promise.all(workers.map(w => waitForChildOutput(w, 'COMPLETED\n')));
    await Promise.all(workers.map(w => w.waitForExit()));

    const finalRaw = await readFile(sharedLog, 'utf8');
    const finalState = JSON.parse(finalRaw);
    assert.equal(finalState.inCritical, 0, 'No processes remaining in critical section');
    assert.equal(finalState.maxConcurrent, 1, 'Max concurrent processes in critical section must be strictly 1');
    assert.equal(finalState.totalRuns, workerCount * 3, 'All rounds across all workers must have completed');
  }
);

test(
  'harnesses independent: claude-code and codex locks do not block each other',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-harnesses-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    // Spawn claude-code lock holder
    const claudeChild = spawnLockHolder({ stateDir, harness: 'claude-code' });
    t.after(() => killChild(claudeChild));
    await waitForChildOutput(claudeChild, 'ACQUIRED\n');

    // Spawn codex lock holder on the same stateDir
    const codexChild = spawnLockHolder({ stateDir, harness: 'codex' });
    t.after(() => killChild(codexChild));
    await waitForChildOutput(codexChild, 'ACQUIRED\n');

    // Both are holding their respective locks simultaneously!
    const claudeBusy = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => 'nope');
    const codexBusy = await withLock(stateDir, 'codex', { mode: 'skip-if-busy' }, async () => 'nope');

    assert.equal(claudeBusy.acquired, false, 'claude-code lock must be held');
    assert.equal(codexBusy.acquired, false, 'codex lock must be held');

    await releaseChild(claudeChild);
    await releaseChild(codexChild);
  }
);

test(
  'SIGTERM cannot release lock before protected operation ends',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-sigterm-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    const child = spawnLockHolder({ stateDir, harness: 'claude-code', interceptSigterm: true });
    t.after(() => killChild(child));
    await waitForChildOutput(child, 'ACQUIRED\n');

    // Send SIGTERM to the holder
    child.sendSignal('SIGTERM');
    await waitForChildOutput(child, 'SIGTERM_RECEIVED\n');

    // Protected operation is still ongoing in child; lock MUST still be held
    const tryAcquire = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => 'nope');
    assert.equal(tryAcquire.acquired, false, 'SIGTERM must not cause premature lock release before operation settles');

    // Now release child and verify it cleanly settles
    await releaseChild(child);

    const postResult = await withLock(stateDir, 'claude-code', { mode: 'skip-if-busy' }, async () => 'acquired-after-settle');
    assert.equal(postResult.acquired, true);
    assert.equal(postResult.result, 'acquired-after-settle');
  }
);

test(
  'file and directory permissions: stateDir 0700, lock database 0600, errors not swallowed',
  { timeout: 15_000 },
  async t => {
    const tempDir = await mkdtemp(join(tmpdir(), 'tm-lock-perms-'));
    t.after(() => rm(tempDir, { recursive: true, force: true }));

    const stateDir = join(tempDir, 'state');

    await withLock(stateDir, 'claude-code', { mode: 'wait' }, async () => {
      // inside lock
    });

    const stateDirStat = await stat(stateDir);
    assert.equal(
      stateDirStat.mode & 0o777,
      0o700,
      `stateDir must have 0700 permissions, got ${(stateDirStat.mode & 0o777).toString(8)}`
    );

    const dbPath = lockFilePath(stateDir, 'claude-code');
    const dbStat = await stat(dbPath);
    assert.equal(
      dbStat.mode & 0o777,
      0o600,
      `lock file must have 0600 permissions, got ${(dbStat.mode & 0o777).toString(8)}`
    );

    // Verify permission errors are not swallowed
    const readOnlyDir = join(tempDir, 'readonly-parent');
    const blockedStateDir = join(readOnlyDir, 'blocked-state');
    await mkdir(readOnlyDir, { mode: 0o500 });
    t.after(() => chmod(readOnlyDir, 0o700).catch(() => {}));
    await chmod(readOnlyDir, 0o500);

    await assert.rejects(
      () => withLock(blockedStateDir, 'claude-code', { mode: 'wait', timeoutMs: 100 }, async () => {}),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        const code = 'code' in err && typeof err.code === 'string' ? err.code : undefined;
        return code === 'EACCES' || code === 'EPERM';
      },
      'Permission errors on inaccessible directory must throw and not be swallowed'
    );

    // Restore permissions so cleanup succeeds
    await chmod(readOnlyDir, 0o700);
  }
);
