#!/usr/bin/env node
import { parseArgs } from 'node:util';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { Ledger } from '../../shared/src/ledger.ts';
import { deliver } from '../../shared/src/delivery.ts';
import type { Harness } from '../../shared/src/observations.ts';
import { scan as claude } from '../../claude-code/src/adapter.ts';
import { scan as codex } from '../../codex/src/adapter.ts';
import { Collector } from './collector.ts';

export async function main(args = process.argv.slice(2)): Promise<void> {
  const { values, positionals } = parseArgs({ args, allowPositionals: true, options: {
    source: { type: 'string' }, root: { type: 'string' }, state: { type: 'string' },
    exclude: { type: 'string', multiple: true }, endpoint: { type: 'string' },
    'max-bytes': { type: 'string' }, 'max-line-bytes': { type: 'string' },
    'interval-ms': { type: 'string' }, 'confirm-upload': { type: 'boolean', default: false },
    help: { type: 'boolean', default: false },
  } });
  const command = positionals[0] ?? 'help';
  if (values.help || command === 'help') {
    console.log(`Tokenmunchers metadata collector (Node >=26)
Commands: activate, preview, collect, sync, watch, pause, resume, status
Required: --source claude-code|codex
Activate: --root EXPLICIT_ROOT [--exclude ROOT_RELATIVE_PREFIX] (repeatable)
Local state: --state DIR (default ~/.local/state/tokenmunchers-collector)
Bounds: --max-bytes N (default 64MiB), --max-line-bytes N (default 1MiB)
Delivery: --endpoint https://HOST/functions/v1/contributions --confirm-upload
Watch: --interval-ms N (default 60000, minimum 1000)
Credential: TOKENMUNCHERS_API_KEY environment variable, never persisted.
preview/collect scan into the local ledger only; sync uploads the retained ledger
as one staged snapshot; watch scans every interval and uploads only with
--confirm-upload. Every upload prints the preview first. Rate-limit resets are
waited out (bounded); an interrupted sync resumes at the failed request.
Exclusions and source root are fixed at activation. Pause/resume affect only the
selected source and permanently exclude original timestamps during the pause.
No default source root, no home discovery, no uploads without confirmation.`);
    return;
  }
  if (positionals.length !== 1 || !['activate', 'preview', 'collect', 'sync', 'watch', 'pause', 'resume', 'status'].includes(command)) throw new Error('Unknown command; use help');
  if (values.source !== 'claude-code' && values.source !== 'codex') throw new Error('Select --source claude-code|codex explicitly');
  const harness: Harness = values.source;
  const ledger = new Ledger(values.state ?? join(homedir(), '.local', 'state', 'tokenmunchers-collector'));
  const collector = new Collector(ledger, { 'claude-code': claude, codex });
  if (command === 'activate') {
    if (!values.root) throw new Error('Activation requires an explicitly selected --root');
    const source = await ledger.activate(harness, values.root, undefined, values.exclude);
    console.log(JSON.stringify({ activated: harness, root: source.root, cutoff: source.cutoff, exclusions: source.exclusions, uploaded: false }, null, 2));
    return;
  }
  if (values.root || values.exclude) throw new Error('Configure root/exclusions only with activate; later scans reuse the explicit selection');
  if (command === 'pause' || command === 'resume') {
    await ledger.pause(harness, command === 'pause');
    console.log(JSON.stringify(await collector.preview(harness), null, 2));
    return;
  }
  if (command === 'status') { console.log(JSON.stringify(await collector.preview(harness), null, 2)); return; }
  const bounds: { maxBytes?: number; maxLineBytes?: number } = {};
  for (const [flag, key] of [['max-bytes', 'maxBytes'], ['max-line-bytes', 'maxLineBytes']] as const) {
    if (values[flag] !== undefined) {
      const number = Number(values[flag]);
      if (!Number.isSafeInteger(number) || number < 1) throw new Error(`--${flag} must be a positive safe integer`);
      bounds[key] = number;
    }
  }
  if (command === 'preview' || command === 'collect') { console.log(JSON.stringify(await collector.scan(harness, bounds), null, 2)); return; }
  const endpoint = values.endpoint;
  if (values['confirm-upload'] && !endpoint) throw new Error('Confirmed delivery requires --endpoint');
  const interval = Number(values['interval-ms'] ?? 60_000);
  if (!Number.isSafeInteger(interval) || interval < 1000) throw new Error('--interval-ms must be >=1000');
  // Interrupting sync/watch aborts in-flight requests and reset waits; delivery persists acknowledged progress first.
  const abort = new AbortController();
  const stop = (): void => abort.abort();
  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);
  const delivery = { endpoint: endpoint ?? '', token: process.env.TOKENMUNCHERS_API_KEY ?? '', confirmed: values['confirm-upload'], signal: abort.signal };
  try {
    if (command === 'sync') {
      console.log(JSON.stringify(await collector.preview(harness), null, 2));
      console.log(JSON.stringify(await deliver(ledger, harness, delivery), null, 2));
      return;
    }
    while (!abort.signal.aborted) {
      try {
        const preview = await collector.scan(harness, bounds);
        console.log(JSON.stringify(preview, null, 2));
        if (values['confirm-upload'] && !preview.paused) console.log(JSON.stringify(await deliver(ledger, harness, delivery), null, 2));
      } catch (error) { console.error(error instanceof Error ? error.message : 'Collector failed'); }
      try { await delay(interval, undefined, { signal: abort.signal }); } catch { if (!abort.signal.aborted) throw new Error('Collector timer failed'); }
    }
  } finally {
    process.removeListener('SIGINT', stop);
    process.removeListener('SIGTERM', stop);
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => { console.error(error instanceof Error ? error.message : 'Collector failed'); process.exitCode = 1; });
}
