import type { AdapterResult, Diagnostic, Observation, ScanOptions } from '../../shared/src/observations.ts';
import { discover } from './discovery.ts';
import { coalesce } from './identity.ts';
import { normalize, object } from './normalizer.ts';
import { readJsonl, type ReadState } from './reader.ts';
import { summaries } from './summaries.ts';
import { OutputEvidence } from './verification.ts';

export type ClaudeCursor = NonNullable<ScanOptions['cursor']>;
export type ClaudeScanOptions = ScanOptions;
export interface ClaudeScanResult extends AdapterResult {
  cursor: ClaudeCursor | null;
  discoveredFiles: string[];
  deferredFiles: string[];
  projects: string[];
  identityAliases: { from: string; to: string }[];
  progress: { files: number; bytesRead: number; deferred: boolean };
}

/** Read-only metadata scan. Result cursors/paths/diagnostic details are LOCAL ONLY. */
export async function scan(root: string, options: ClaudeScanOptions = {}): Promise<ClaudeScanResult> {
  const maxBytes = options.maxBytes ?? 256 * 1024 * 1024;
  const maxLineBytes = options.maxLineBytes ?? 4 * 1024 * 1024;
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 1 || !Number.isSafeInteger(maxLineBytes) || maxLineBytes < 1) throw new RangeError('Scan byte and line limits must be positive safe integers.');
  const diagnostics: Diagnostic[] = [];
  let files;
  try { files = await discover(root, options.exclude ?? [], diagnostics); }
  catch (error) {
    return { observations: [], diagnostics: [{ code: 'ROOT_UNAVAILABLE', message: `Selected Claude root is unavailable (${(error as NodeJS.ErrnoException).code ?? 'IO'}).` }], cursor: null, discoveredFiles: [], deferredFiles: [], projects: [], identityAliases: [], progress: { files: 0, bytesRead: 0, deferred: true } };
  }
  const observations = new Map<string, Observation>();
  const requests = new Map<string, string>();
  const identityAliases: { from: string; to: string }[] = [];
  const deferredFiles: string[] = [];
  const evidence = new OutputEvidence();
  let bytesRead = 0;
  // Real 2.1.312 transcripts also persist attachment, request-shape/blob, tool-use and mode records; none carries usage.
  const knownTypes: Record<string, true> = { user: true, assistant: true, system: true, summary: true, progress: true, 'file-history-snapshot': true, 'queue-operation': true, 'last-prompt': true, 'custom-title': true, 'agent-name': true, 'agent-color': true, tag: true, 'pr-link': true, 'bridge-session': true, 'worktree-state': true, 'cost-state': true, result: true, attachment: true, 'atis-latch': true, 'api-request-shape': true, 'api-request-blob': true, 'api-request': true, 'tools-used': true, mode: true };
  let scannedFiles = 0;
  let cursor: ClaudeCursor | null = null;
  let startIndex = options.cursor ? files.findIndex((file) => file.relativePath === options.cursor!.file) : 0;
  if (startIndex < 0) {
    diagnostics.push({ code: 'CURSOR_FILE_MISSING', message: 'Continuation file disappeared or was excluded; canonical full rescan restarted.' });
    startIndex = 0;
  }
  for (let index = startIndex; index < files.length; index++) {
    const file = files[index]!;
    if (bytesRead >= maxBytes) {
      cursor = { file: file.relativePath, offset: 0, line: 0, device: 0, inode: 0 };
      deferredFiles.push(...files.slice(index).map((item) => item.relativePath));
      break;
    }
    const resume = options.cursor?.file === file.relativePath ? options.cursor : null;
    const state: ReadState = { bytesRead: 0, committedOffset: resume?.offset ?? 0, startOffset: resume?.offset ?? 0, line: resume?.line ?? 0, device: resume?.device ?? 0, inode: resume?.inode ?? 0, discardLine: resume?.discardLine ?? false, deferred: false, truncated: false };
    let sawAssistant = false;
    let readable = true;
    try {
      for await (const row of readJsonl(file.path, maxBytes - bytesRead, maxLineBytes, diagnostics, state)) {
        const entry = row.value;
        const result = normalize(entry, file);
        if (result) {
          sawAssistant = true;
          let observation = result.observation;
          if (result.requestId) {
            const fallback = `request:${result.requestId}`;
            const known = requests.get(result.requestId);
            if (observation.identity_strength === 'response') {
              if (known && known !== observation.canonical_id) {
                // The first response owns the request alias; a second response ID stays separate and unaliased.
                diagnostics.push({ code: 'REQUEST_ID_CONFLICT', message: 'Request metadata names multiple response IDs; response identities remain separate.', file: file.path, line: row.line });
                observation = { ...observation, diagnostics: [...observation.diagnostics, 'REQUEST_ID_CONFLICT'] };
              } else {
                requests.set(result.requestId, observation.canonical_id);
                identityAliases.push({ from: fallback, to: observation.canonical_id });
                const earlier = observations.get(fallback);
                if (earlier) {
                  observations.delete(fallback);
                  observation = { ...coalesce(earlier, observation), canonical_id: observation.canonical_id, identity_strength: 'response' };
                }
              }
            } else if (known) observation = { ...observation, canonical_id: known, identity_strength: 'response' };
          }
          evidence.frame(file, observation.canonical_id, result.observation, result.output);
          const previous = observations.get(observation.canonical_id);
          observations.set(observation.canonical_id, previous ? coalesce(previous, observation) : observation);
        } else if (entry.type === 'assistant' && object(object(entry.message)?.usage)) {
          diagnostics.push({ code: 'ASSISTANT_USAGE_SKIPPED', message: 'Assistant usage has no supported stable identity or represents an API error.', file: file.path, line: row.line });
        }
        for (const summary of summaries(entry, file)) {
          if (summary.diagnostics.includes('SUMMARY_VALUE_OUT_OF_RANGE')) diagnostics.push({ code: 'SUMMARY_VALUE_OUT_OF_RANGE', message: 'Cumulative summary token or cost value exceeds the accounting bound; withheld as unavailable.', file: file.path, line: row.line });
          observations.set(summary.canonical_id, summary);
        }
        evidence.costState(file, entry);
        if (entry.type === 'result' && entry.subtype === 'error_during_execution') diagnostics.push({ code: 'CRASH_SUMMARY_UNRELIABLE', message: 'Crash result may carry zeroed usage; summary withheld.', file: file.path, line: row.line });
        if (entry.type === 'system' && (entry.subtype === 'compact_boundary' || entry.subtype === 'microcompact_boundary')) diagnostics.push({ code: 'COMPACTION_COVERAGE_UNVERIFIED', message: 'Compaction boundary scanned; unrecorded helper calls cannot be recovered.', file: file.path, line: row.line });
        if (entry.type === 'summary' || entry.type === 'custom-title') diagnostics.push({ code: 'HELPER_USAGE_UNAVAILABLE', message: 'Conversation summary or title metadata is not a token ledger; helper usage may be absent.', file: file.path, line: row.line });
        if (typeof entry.type === 'string' && !Object.hasOwn(knownTypes, entry.type)) diagnostics.push({ code: 'UNRECOGNIZED_RECORD', message: 'Unrecognized transcript record type; no usage inferred.', file: file.path, line: row.line });
      }
    } catch (error) {
      diagnostics.push({ code: 'TRANSCRIPT_READ_FAILED', message: `Transcript could not be read (${(error as NodeJS.ErrnoException).code ?? 'IO'}).`, file: file.path });
      deferredFiles.push(file.relativePath);
      readable = false;
    }
    scannedFiles++;
    bytesRead += state.bytesRead;
    if (sawAssistant) diagnostics.push({ code: 'HELPER_COVERAGE_INCOMPLETE', message: 'Retained assistant rows do not prove complete helper, billing or final-output coverage.', file: file.path });
    if (state.deferred) deferredFiles.push(file.relativePath);
    else if (readable && state.startOffset === 0) evidence.complete(file);
    if (state.truncated) {
      cursor = { file: file.relativePath, offset: state.committedOffset, line: state.line, device: state.device, inode: state.inode, discardLine: state.discardLine };
      deferredFiles.push(...files.slice(index + 1).map((item) => item.relativePath));
      if (state.committedOffset === state.startOffset) diagnostics.push({ code: 'SCAN_BOUND_TOO_SMALL', message: 'A record cannot fit this scan budget; increase maxBytes to advance without persisting source content.', file: file.path });
      break;
    }
  }
  evidence.apply(files, observations, (id) => id.startsWith('request:') ? requests.get(id.slice('request:'.length)) ?? id : id, diagnostics);
  return {
    observations: [...observations.values()], diagnostics,
    identityAliases: [...new Map(identityAliases.map((alias) => [`${alias.from}\0${alias.to}`, alias])).values()],
    cursor, discoveredFiles: files.map((file) => file.relativePath), deferredFiles: [...new Set(deferredFiles)],
    projects: [...new Set(files.map((file) => file.project))],
    progress: { files: scannedFiles, bytesRead, deferred: deferredFiles.length > 0 },
  };
}
