import { relative, resolve, sep } from 'node:path';
import type { AdapterResult, Diagnostic, ScanOptions } from '../../shared/src/observations.ts';
import { deduplicate, RolloutParser, type CumulativeEvidence, type ParsedRollout, type Segment } from './parser.ts';
import { equalUsage, type WireUsage } from './token-normalizer.ts';
import { discover, readRows, type Cursor, type ReadProgress } from './reader.ts';

export interface CodexScanResult extends AdapterResult {
  progress: ReadProgress;
  cursors: Cursor[];
  segments: Array<{ file: string; segments: Segment[] }>;
  continuation: { nextFile: string | null };
  deferredFiles: string[];
  supersededCanonicalIds: string[];
  conflictingCanonicalIds: string[];
}

/** An exact cumulative chain proves disjointness; numeric ordering alone cannot survive resets. */
function connectedBoundary(from: WireUsage | null, to: WireUsage | null, fromTime: string | null,
  toTime: string | null, edges: CumulativeEvidence[]): boolean {
  if (!from || !to || !fromTime || !toTime || fromTime > toTime) return false;
  const pending = [{ usage: from, time: fromTime }];
  const visited = new Set<string>();
  for (let index = 0; index < pending.length; index++) {
    const current = pending[index];
    if (equalUsage(current.usage, to)) return true;
    const key = JSON.stringify([current.usage, current.time]);
    if (visited.has(key)) continue;
    visited.add(key);
    for (const edge of edges) {
      if (edge.start && edge.end && edge.startTime && edge.endTime && edge.startTime >= current.time &&
        edge.endTime <= toTime && equalUsage(current.usage, edge.start)) pending.push({ usage: edge.end, time: edge.endTime });
    }
  }
  return false;
}
export async function scan(root: string, options: ScanOptions = {}): Promise<CodexScanResult> {
  for (const value of [options.maxBytes, options.maxLineBytes]) {
    if (value !== undefined && (!Number.isSafeInteger(value) || value <= 0)) throw new RangeError('Scan limits must be positive safe integers.');
  }
  const diagnostics: Diagnostic[] = [];
  const files = await discover(root, options.exclude ?? [], diagnostics);
  const progress: ReadProgress = { files: 0, bytesRead: 0, deferred: 0 };
  const cursors: Cursor[] = [];
  const segments: Array<{ file: string; segments: Segment[] }> = [];
  const observations = [] as AdapterResult['observations'];
  const supersededCanonicalIds: string[] = [];
  const parsed: Array<{ file: string; result: ParsedRollout }> = [];
  const conflictingCanonicalIds: string[] = [];
  const local = (file: string) => relative(resolve(root), file).split(sep).join('/');
  const continuation = options.continuation;
  const start = continuation?.nextFile ? files.findIndex(file => local(file) === continuation.nextFile) : 0;
  if (start < 0) {
    diagnostics.push({ code: 'STALE_CONTINUATION', message: 'Continuation file is no longer retained; restart a full scan.' });
    return { observations: [], diagnostics, progress, cursors, segments, deferredFiles: [], supersededCanonicalIds: [], conflictingCanonicalIds: [], continuation: { nextFile: null } };
  }
  let nextFile: string | null = null;
  const deferredFiles: string[] = [];
  for (let index = start; index < files.length; index++) {
    const file = files[index];
    const path = local(file);
    if (Math.max(progress.bytesRead, progress.sourceBytesRead ?? 0) >= (options.maxBytes ?? 256 * 1024 * 1024)) {
      nextFile ??= path;
      deferredFiles.push(...files.slice(index).map(local));
      progress.deferred += files.length - index;
      diagnostics.push({ code: 'SCAN_TRUNCATED', message: 'Remaining rollouts deferred by scan byte budget.', file: path });
      break;
    }
    const cursor: Cursor = { file: path, line: 0, bytesRead: 0, complete: false, truncated: false };
    cursors.push(cursor);
    const parser = new RolloutParser(path);
    for await (const row of readRows(file, path, options, progress, diagnostics, cursor)) parser.consume(row.value, row.line);
    const result = parser.finish();
    parsed.push({ file: path, result });
    observations.push(...result.observations);
    diagnostics.push(...result.diagnostics);
    supersededCanonicalIds.push(...result.supersededCanonicalIds);
    segments.push({ file: path, segments: result.segments });
    if (!cursor.complete) deferredFiles.push(path);
    // Only a budget stop resumes here; damaged, partial or unreadable files are retried by the next full pass.
    // A file that exhausted a fresh budget can never fit it, so resume after it instead of pinning.
    if (cursor.truncated) {
      if (index === start) {
        nextFile ??= index + 1 < files.length ? local(files[index + 1]) : null;
        diagnostics.push({ code: 'SCAN_BOUND_TOO_SMALL', message: 'Rollout exceeds the whole scan byte budget; increase maxBytes to read it completely.', file: path });
      } else nextFile ??= path;
    }
  }
  const unique = deduplicate(observations, diagnostics, conflictingCanonicalIds);
  const conflicts = new Set(conflictingCanonicalIds);
  const modern = parsed.flatMap(item => item.result.modernEvidence).filter(item => !conflicts.has(item.canonicalId));
  const safe = new Set<string>();
  const uncertain = new Set<string>();
  for (const { file, result } of parsed) {
    const localModern = new Set(result.modernEvidence.map(item => item.canonicalId));
    const retained = new Set(result.observations.map(item => item.canonical_id));
    for (const interval of result.legacyEvidence) {
      if (!retained.has(interval.canonicalId)) continue;
      const sessionModern = modern.filter(item => item.session === interval.session);
      const disjoint = sessionModern.filter(item => !localModern.has(item.canonicalId)).every(item =>
        connectedBoundary(interval.end, item.start, interval.endTime, item.startTime, sessionModern) ||
        connectedBoundary(item.end, interval.start, item.endTime, interval.startTime, sessionModern));
      if (disjoint) safe.add(interval.canonicalId);
      else {
        uncertain.add(interval.canonicalId);
        diagnostics.push({ code: 'UNCERTAIN_CROSS_FILE_TRANSITION', file,
          message: 'Legacy archive interval lacks an exact disjoint cumulative boundary against another retained modern rollout.' });
      }
    }
  }
  supersededCanonicalIds.push(...[...uncertain].filter(id => !safe.has(id)));
  const superseded = new Set(supersededCanonicalIds);
  const accepted = unique.filter(observation => !superseded.has(observation.canonical_id));
  return { observations: accepted, diagnostics, progress, cursors, segments,
    continuation: { nextFile }, deferredFiles, supersededCanonicalIds: [...superseded], conflictingCanonicalIds };
}
