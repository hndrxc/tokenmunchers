import { chmod, mkdir, open } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { setTimeout as delay } from 'node:timers/promises';
import type { Harness } from '../../shared/src/observations.ts';

export function lockFilePath(stateDir: string, harness: Harness): string {
  return join(stateDir, `${harness}.lock.sqlite`);
}

export interface LockOptions {
  mode: 'skip-if-busy' | 'wait';
  timeoutMs?: number;
  pollIntervalMs?: number;
}

export type LockResult<T> =
  | { acquired: true; result: T }
  | { acquired: false; result?: undefined };

function isSqliteBusy(error: unknown): boolean {
  if (error instanceof Error) {
    if ('errcode' in error && (error.errcode === 5 || error.errcode === 6)) {
      return true;
    }
    if (error.message.includes('database is locked') || error.message.includes('busy')) {
      return true;
    }
  }
  return false;
}

export async function withLock<T>(
  stateDir: string,
  harness: Harness,
  options: LockOptions,
  fn: () => Promise<T>
): Promise<LockResult<T>> {
  await mkdir(stateDir, { recursive: true, mode: 0o700 });
  await chmod(stateDir, 0o700);
  const lockPath = lockFilePath(stateDir, harness);
  const file = await open(lockPath, 'a', 0o600);
  await file.close();
  await chmod(lockPath, 0o600);

  const db = new DatabaseSync(lockPath);
  db.exec('PRAGMA busy_timeout = 0;');

  let acquired = false;
  if (options.mode === 'skip-if-busy') {
    try {
      db.exec('BEGIN IMMEDIATE;');
      acquired = true;
    } catch (error: unknown) {
      if (isSqliteBusy(error)) {
        db.close();
        return { acquired: false };
      }
      db.close();
      throw error;
    }
  } else {
    const timeoutMs = options.timeoutMs ?? 10_000;
    const pollInterval = options.pollIntervalMs ?? 100;
    const deadline = Date.now() + timeoutMs;

    while (true) {
      try {
        db.exec('BEGIN IMMEDIATE;');
        acquired = true;
        break;
      } catch (error: unknown) {
        if (!isSqliteBusy(error)) {
          db.close();
          throw error;
        }
        if (Date.now() >= deadline) {
          break;
        }
        const remaining = deadline - Date.now();
        await delay(Math.min(pollInterval, Math.max(1, remaining)));
      }
    }

    if (!acquired) {
      db.close();
      throw new Error(`Another process is currently running for ${harness}. Wait for it to finish or check process status.`);
    }
  }

  try {
    const result = await fn();
    return { acquired: true, result };
  } finally {
    try {
      if (db.isTransaction) {
        db.exec('ROLLBACK;');
      }
    } finally {
      db.close();
    }
  }
}
