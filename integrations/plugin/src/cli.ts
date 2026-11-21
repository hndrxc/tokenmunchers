#!/usr/bin/env node
import { parseArgs } from 'node:util';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createHash } from 'node:crypto';
import type { Harness, Observation } from '../../shared/src/observations.ts';
import { Ledger, type SourceState } from '../../shared/src/ledger.ts';
import { deliver } from '../../shared/src/delivery.ts';
import { attributionIssue } from '../../shared/src/aggregate.ts';
import { outbound } from '../../shared/src/metadata.ts';
import { Collector } from '../../collector/src/collector.ts';
import { scan as claudeScan } from '../../claude-code/src/adapter.ts';
import { scan as codexScan } from '../../codex/src/adapter.ts';
import {
  readPluginConfig,
  writePluginConfig,
  validateEndpoint,
  getLastDeliveredRevision,
  updateDeliveredRevision,
  type PluginConfig,
} from './config.ts';
import { withLock } from './lock.ts';
import { DEFAULT_ENDPOINT, formatCompactStatus } from './format.ts';

export type OutboundRevisionSource =
  | SourceState
  | {
      observations: Observation[];
      superseded_ids: string[];
      identity_aliases: Array<{ from: string; to: string }>;
    };

export function computeOutboundRevision(source: OutboundRevisionSource): { revision: string; count: number } {
  let observations: Observation[];
  let aliases: Array<{ from: string; to: string }>;
  let superseded: string[];

  if ('records' in source) {
    const excluded = new Set(source.excluded_ids ?? []);
    observations = Object.values(source.records)
      .filter(obs => !attributionIssue(obs, source.cutoff) && !excluded.has(obs.canonical_id))
      .sort((a, b) => a.canonical_id.localeCompare(b.canonical_id));
    const ids = new Set(observations.map(obs => obs.canonical_id));
    const material = new Set(source.material_alias_ids ?? []);
    aliases = (source.identity_aliases ?? [])
      .filter(alias => material.has(alias.from) && ids.has(alias.to))
      .sort((a, b) => a.from.localeCompare(b.from) || a.to.localeCompare(b.to));
    superseded = [...new Set(source.superseded_ids ?? [])].sort();
  } else {
    observations = [...source.observations].sort((a, b) => a.canonical_id.localeCompare(b.canonical_id));
    aliases = [...source.identity_aliases].sort((a, b) => a.from.localeCompare(b.from) || a.to.localeCompare(b.to));
    superseded = [...new Set(source.superseded_ids ?? [])].sort();
  }

  const hash = createHash('sha256');
  for (const obs of observations) {
    hash.update(`obs:${JSON.stringify(outbound(obs))}\n`);
  }
  for (const id of superseded) {
    hash.update(`superseded:${id}\n`);
  }
  for (const alias of aliases) {
    hash.update(`alias:${alias.from}->${alias.to}\n`);
  }
  return {
    revision: hash.digest('hex'),
    count: observations.length + superseded.length + aliases.length,
  };
}

export interface PluginCliOptions {
  args?: string[];
  ledger?: Ledger;
  collector?: Collector;
  fetchImpl?: typeof fetch;
  env?: NodeJS.ProcessEnv;
  stdout?: (message: string) => void;
  stderr?: (message: string) => void;
  lockTimeoutMs?: number;
}

export function sanitizeErrorMessage(message: string, env?: NodeJS.ProcessEnv): string {
  let result = message;

  const activeEnv = env ?? process.env;
  const envKey = activeEnv?.TOKENMUNCHERS_API_KEY?.trim();
  if (envKey && envKey.length >= 4) {
    result = result.replaceAll(envKey, '[REDACTED]');
  }

  result = result.replace(/\btm_[A-Za-z0-9_-]{8,}\b/g, '[REDACTED]');
  result = result.replace(/\btm_[A-Za-z0-9_-]+\b/g, '[REDACTED]');
  result = result.replace(/(Bearer\s+)[A-Za-z0-9._~+/-]+=*/gi, '$1[REDACTED]');
  result = result.replace(/((?:api[_-]?key|token|secret|password)\s*[:=]\s*)[A-Za-z0-9._~+/-]+/gi, '$1[REDACTED]');
  result = result.replace(/:\/\/([^:@\s]+):([^@\s]+)@/g, '://[REDACTED]:[REDACTED]@');
  result = result.replace(/:\/\/([^:@\s]+)@/g, '://[REDACTED]@');

  return result;
}

export async function main(optionsOrArgs: PluginCliOptions | string[] = process.argv.slice(2)): Promise<void> {
  const options: PluginCliOptions = Array.isArray(optionsOrArgs) ? { args: optionsOrArgs } : optionsOrArgs;
  const env = options.env ?? process.env;
  try {
    return await executeMain(options);
  } catch (error) {
    const raw = error instanceof Error ? error.message : String(error);
    const sanitized = sanitizeErrorMessage(raw, env);
    if (error instanceof Error) {
      error.message = sanitized;
      throw error;
    }
    throw new Error(sanitized);
  }
}

async function executeMain(options: PluginCliOptions): Promise<void> {
  const args = options.args ?? process.argv.slice(2);
  const env = options.env ?? process.env;
  const log = options.stdout ?? ((message: string) => console.log(message));

  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      source: { type: 'string' },
      root: { type: 'string' },
      state: { type: 'string' },
      exclude: { type: 'string', multiple: true },
      endpoint: { type: 'string' },
      'max-bytes': { type: 'string' },
      'max-line-bytes': { type: 'string' },
      'confirm-upload': { type: 'boolean', default: false },
      help: { type: 'boolean', default: false },
    },
  });

  const command = positionals[0] ?? 'help';

  if (values.help || command === 'help') {
    log(`Tokenmunchers native plugin runtime (Node >=26)
Usage: node cli.ts COMMAND --source claude-code|codex [options]

Commands:
  activate   Configure explicit source root and exclusions locally
  preview    Scan source root and report compact metadata-only preview
  status     Display current local ledger metadata and upload consent status
  sync       Scan source root and upload staged snapshot (--confirm-upload required)
  pause      Pause source; excludes observations timestamped during pause
  resume     Resume paused source
  auto-on    Enable recurring uploads on hook execution (--confirm-upload required)
  auto-off   Disable recurring uploads on hook execution
  hook       Native async hook entrypoint; non-blocking lock, scans and uploads if consented

Options:
  --source claude-code|codex   Target AI development harness (required)
  --root PATH                  Explicit source root (activate only)
  --exclude PREFIX             Exclude subpath prefix (activate only, repeatable)
  --state DIR                  Local state dir (default: ~/.local/state/tokenmunchers-collector)
  --endpoint URL               Supabase delivery endpoint (default: Supabase production URL)
  --confirm-upload             Explicit upload confirmation (required for sync and auto-on)
  --max-bytes N                Scan batch size limit (default: 64MiB)
  --max-line-bytes N           Scan line size limit (default: 1MiB)
  --help                       Show this help

Note on sync:
  Plugin sync scans the source root into the ledger first and then delivers the snapshot,
  unlike the standalone collector CLI which only uploads the already retained ledger.`);
    return;
  }

  const allowedCommands = ['activate', 'preview', 'sync', 'pause', 'resume', 'status', 'auto-on', 'auto-off', 'hook'];
  if (positionals.length !== 1 || !allowedCommands.includes(command)) {
    throw new Error('Unknown command; use help');
  }

  if (values.source !== 'claude-code' && values.source !== 'codex') {
    throw new Error('Select --source claude-code|codex explicitly');
  }
  const harness: Harness = values.source;

  if (command !== 'activate' && (values.root !== undefined || values.exclude !== undefined)) {
    throw new Error('Configure root/exclusions only with activate; later scans reuse the explicit selection');
  }

  const bounds: { maxBytes?: number; maxLineBytes?: number } = {};
  for (const [flag, key] of [['max-bytes', 'maxBytes'], ['max-line-bytes', 'maxLineBytes']] as const) {
    if (values[flag] !== undefined) {
      const number = Number(values[flag]);
      if (!Number.isSafeInteger(number) || number < 1) throw new Error(`--${flag} must be a positive safe integer`);
      bounds[key] = number;
    }
  }

  const stateDir = values.state ?? join(homedir(), '.local', 'state', 'tokenmunchers-collector');
  const ledger = options.ledger ?? new Ledger(stateDir);
  const collector = options.collector ?? new Collector(ledger, { 'claude-code': claudeScan, codex: codexScan });
  const endpoint = values.endpoint;
  const lockTimeoutMs = options.lockTimeoutMs ?? 3000;

  if (command === 'hook') {
    const isActivated = await ledger.transaction(state => !!state.sources[harness]);
    if (!isActivated) {
      return;
    }

    const lockResult = await withLock(stateDir, harness, { mode: 'skip-if-busy' }, async () => {
      await collector.scan(harness, bounds);

      const isPaused = await ledger.transaction(state =>
        state.sources[harness]?.pauses.some(p => p.end === null) ?? false
      );
      if (isPaused) {
        return;
      }

      let config = await readPluginConfig(stateDir, harness);
      const token = env.TOKENMUNCHERS_API_KEY;
      if (!config.auto_upload || !token) {
        return;
      }

      const targetEndpoint = endpoint ?? config.endpoint ?? DEFAULT_ENDPOINT;
      try {
        validateEndpoint(targetEndpoint);
      } catch {
        return;
      }

      const outboundState = await ledger.transaction(state => {
        const src = state.sources[harness];
        if (!src) return null;
        return {
          pending: src.pending ? {
            snapshot_id: src.pending.snapshot_id,
            observations: src.pending.observations,
            superseded_ids: src.pending.superseded_ids,
            identity_aliases: src.pending.identity_aliases,
          } : null,
          latest: computeOutboundRevision(src),
        };
      });
      if (!outboundState) return;

      const abort = new AbortController();
      const onInterrupt = (): void => { abort.abort(); };
      process.once('SIGINT', onInterrupt);
      process.once('SIGTERM', onInterrupt);

      try {
        if (outboundState.pending) {
          const pendingRev = computeOutboundRevision(outboundState.pending);
          await deliver(ledger, harness, {
            endpoint: targetEndpoint,
            token,
            confirmed: true,
            signal: abort.signal,
            ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
          });
          config = updateDeliveredRevision(config, targetEndpoint, pendingRev.revision);
          await writePluginConfig(stateDir, harness, config);

          const postState = await ledger.transaction(state => {
            const src = state.sources[harness];
            return src ? computeOutboundRevision(src) : null;
          });
          if (postState && postState.count > 0 && postState.revision !== pendingRev.revision) {
            await deliver(ledger, harness, {
              endpoint: targetEndpoint,
              token,
              confirmed: true,
              signal: abort.signal,
              ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
            });
            config = updateDeliveredRevision(config, targetEndpoint, postState.revision);
            await writePluginConfig(stateDir, harness, config);
          }
        } else {
          if (outboundState.latest.count === 0) {
            return;
          }
          const lastDelivered = getLastDeliveredRevision(config, targetEndpoint);
          if (lastDelivered === outboundState.latest.revision) {
            return;
          }
          await deliver(ledger, harness, {
            endpoint: targetEndpoint,
            token,
            confirmed: true,
            signal: abort.signal,
            ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
          });
          config = updateDeliveredRevision(config, targetEndpoint, outboundState.latest.revision);
          await writePluginConfig(stateDir, harness, config);
        }
      } finally {
        process.removeListener('SIGINT', onInterrupt);
        process.removeListener('SIGTERM', onInterrupt);
      }
    });

    if (!lockResult.acquired) {
      return;
    }
    return;
  }

  if (command === 'activate') {
    if (!values.root) {
      throw new Error('Activation requires an explicitly selected --root');
    }
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      await ledger.activate(harness, values.root!, undefined, values.exclude);
      const preview = await collector.preview(harness);
      const config = await readPluginConfig(stateDir, harness);
      log(JSON.stringify(formatCompactStatus(preview, config), null, 2));
    });
    return;
  }

  if (command === 'status') {
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      const source = await ledger.transaction(state => state.sources[harness]);
      if (!source) throw new Error('Activate source first');
      const preview = await collector.preview(harness);
      const config = await readPluginConfig(stateDir, harness);
      log(JSON.stringify(formatCompactStatus(preview, config), null, 2));
    });
    return;
  }

  if (command === 'preview') {
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      const source = await ledger.transaction(state => state.sources[harness]);
      if (!source) throw new Error('Activate source first');
      const scanResult = await collector.scan(harness, bounds);
      const config = await readPluginConfig(stateDir, harness);
      log(JSON.stringify(formatCompactStatus(scanResult, config), null, 2));
    });
    return;
  }

  if (command === 'pause' || command === 'resume') {
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      const source = await ledger.transaction(state => state.sources[harness]);
      if (!source) throw new Error('Activate source first');
      await ledger.pause(harness, command === 'pause');
      const preview = await collector.preview(harness);
      const config = await readPluginConfig(stateDir, harness);
      log(JSON.stringify(formatCompactStatus(preview, config), null, 2));
    });
    return;
  }

  if (command === 'auto-on') {
    if (!values['confirm-upload']) {
      throw new Error('Recurring uploads require explicit --confirm-upload');
    }
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      const source = await ledger.transaction(state => state.sources[harness]);
      if (!source) throw new Error('Activate the explicitly chosen source root first');
      const currentConfig = await readPluginConfig(stateDir, harness);
      const targetEndpoint = endpoint ?? currentConfig.endpoint ?? DEFAULT_ENDPOINT;
      validateEndpoint(targetEndpoint);
      const updatedConfig: PluginConfig = {
        ...currentConfig,
        auto_upload: true,
        endpoint: targetEndpoint,
        updated_at: new Date().toISOString(),
      };
      await writePluginConfig(stateDir, harness, updatedConfig);
      const preview = await collector.preview(harness);
      log(JSON.stringify(formatCompactStatus(preview, updatedConfig), null, 2));
    });
    return;
  }

  if (command === 'auto-off') {
    await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
      const source = await ledger.transaction(state => state.sources[harness]);
      if (!source) throw new Error('Activate the explicitly chosen source root first');
      const currentConfig = await readPluginConfig(stateDir, harness);
      const updatedConfig: PluginConfig = {
        ...currentConfig,
        auto_upload: false,
        updated_at: new Date().toISOString(),
      };
      await writePluginConfig(stateDir, harness, updatedConfig);
      const preview = await collector.preview(harness);
      log(JSON.stringify(formatCompactStatus(preview, updatedConfig), null, 2));
    });
    return;
  }

  if (command === 'sync') {
    if (!values['confirm-upload']) {
      throw new Error('Upload requires explicit --confirm-upload after local preview');
    }
    const token = env.TOKENMUNCHERS_API_KEY;
    if (!token) {
      throw new Error('Set TOKENMUNCHERS_API_KEY (not persisted)');
    }

    const abort = new AbortController();
    const onInterrupt = (): void => { abort.abort(); };
    process.once('SIGINT', onInterrupt);
    process.once('SIGTERM', onInterrupt);

    try {
      await withLock(stateDir, harness, { mode: 'wait', timeoutMs: lockTimeoutMs }, async () => {
        const source = await ledger.transaction(state => state.sources[harness]);
        if (!source) throw new Error('Activate source first');
        if (source.pauses.some(p => p.end === null)) throw new Error('Source paused; resume before delivery');

        let config = await readPluginConfig(stateDir, harness);
        const targetEndpoint = endpoint ?? config.endpoint ?? DEFAULT_ENDPOINT;
        validateEndpoint(targetEndpoint);

        // Plugin sync scans before delivering, unlike standalone CLI sync
        await collector.scan(harness, bounds);

        const preState = await ledger.transaction(state => {
          const src = state.sources[harness];
          if (!src) return null;
          return {
            pending: src.pending ? {
              snapshot_id: src.pending.snapshot_id,
              observations: src.pending.observations,
              superseded_ids: src.pending.superseded_ids,
              identity_aliases: src.pending.identity_aliases,
            } : null,
            latest: computeOutboundRevision(src),
          };
        });
        if (!preState) throw new Error('Activate source first');

        let deliveryResult: unknown;

        if (preState.pending) {
          const pendingRev = computeOutboundRevision(preState.pending);
          deliveryResult = await deliver(ledger, harness, {
            endpoint: targetEndpoint,
            token,
            confirmed: true,
            signal: abort.signal,
            ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
          });
          config = updateDeliveredRevision(config, targetEndpoint, pendingRev.revision);
          await writePluginConfig(stateDir, harness, config);

          const postState = await ledger.transaction(state => {
            const src = state.sources[harness];
            return src ? computeOutboundRevision(src) : null;
          });

          if (postState && postState.count > 0 && postState.revision !== pendingRev.revision) {
            deliveryResult = await deliver(ledger, harness, {
              endpoint: targetEndpoint,
              token,
              confirmed: true,
              signal: abort.signal,
              ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
            });
            config = updateDeliveredRevision(config, targetEndpoint, postState.revision);
            await writePluginConfig(stateDir, harness, config);
          }
        } else {
          deliveryResult = await deliver(ledger, harness, {
            endpoint: targetEndpoint,
            token,
            confirmed: true,
            signal: abort.signal,
            ...(options.fetchImpl ? { fetchImpl: options.fetchImpl } : {}),
          });
          config = updateDeliveredRevision(config, targetEndpoint, preState.latest.revision);
          await writePluginConfig(stateDir, harness, config);
        }

        const preview = await collector.preview(harness);
        log(JSON.stringify(formatCompactStatus(preview, config, { delivery: deliveryResult as unknown as Record<string, unknown>, endpoint: targetEndpoint }), null, 2));
      });
    } finally {
      process.removeListener('SIGINT', onInterrupt);
      process.removeListener('SIGTERM', onInterrupt);
    }
    return;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  // Ignore hook input only in the executable; library callers retain their own stdin.
  if (process.stdin.readable) {
    process.stdin.on('error', () => {});
    process.stdin.resume();
    if (typeof process.stdin.unref === 'function') process.stdin.unref();
  }
  main().catch(error => {
    const raw = error instanceof Error ? error.message : 'Plugin CLI failed';
    console.error(sanitizeErrorMessage(raw));
    process.exitCode = 1;
  });
}
