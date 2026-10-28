import type { Diagnostic, Observation } from '../../shared/src/observations.ts';
import type { Transcript } from './discovery.ts';
import { object, token } from './normalizer.ts';

/** Disjoint categories in a fixed order: input, cache read, cache write, output. */
type Usage = [number | null, number | null, number | null, number | null];

/** Cost-state modelUsage covers the main pipeline including its subagents, so evidence spans both transcripts. */
function session(file: Transcript): string {
  return JSON.stringify([file.project, file.isSubagent ? file.parentSessionId ?? file.sessionId : file.sessionId]);
}

/**
 * Within-scan evidence that saved per-response usage is final. In real Claude Code 2.1.312 sessions the
 * per-message usage summed over distinct message IDs equals the latest cost-state modelUsage of that model
 * exactly. Only that exact equality, over a session whose transcripts were all read completely in this scan,
 * verifies output. Cost is never allocated to requests and summaries are never added to them.
 */
export class OutputEvidence {
  readonly #frames = new Map<string, Set<string>>();
  readonly #members = new Map<string, Set<string>>();
  readonly #costStates = new Map<string, Record<string, unknown>>();
  readonly #complete = new Set<string>();

  /** One saved assistant frame; `canonicalId` may be a request fallback later resolved to its response. */
  frame(file: Transcript, canonicalId: string, observation: Observation, output: number | null): void {
    const usage: Usage = [observation.input_tokens, observation.cache_read_tokens, observation.cache_write_tokens, output];
    let frames = this.#frames.get(canonicalId);
    if (!frames) this.#frames.set(canonicalId, frames = new Set());
    frames.add(JSON.stringify(usage));
    const key = session(file);
    let members = this.#members.get(key);
    if (!members) this.#members.set(key, members = new Set());
    members.add(canonicalId);
  }

  /** Later rows replace earlier ones: cost-state is cumulative and the last complete row is the latest. */
  costState(file: Transcript, entry: Record<string, unknown>): void {
    if (entry.type !== 'cost-state' || file.isSubagent) return;
    if (entry.sessionId !== undefined && entry.sessionId !== file.sessionId) return;
    this.#costStates.set(session(file), object(entry.modelUsage) ?? {});
  }

  /** The transcript was read from byte 0 to EOF without deferral, truncation, or read failure. */
  complete(file: Transcript): void {
    this.#complete.add(file.relativePath);
  }

  apply(files: Transcript[], observations: Map<string, Observation>, resolve: (canonicalId: string) => string, diagnostics: Diagnostic[]): void {
    const transcripts = new Map<string, Transcript[]>();
    for (const file of files) {
      const key = session(file);
      const list = transcripts.get(key);
      if (list) list.push(file);
      else transcripts.set(key, [file]);
    }
    const usages = new Map<string, Set<string>>();
    for (const [id, frames] of this.#frames) {
      const target = resolve(id);
      let set = usages.get(target);
      if (!set) usages.set(target, set = new Set());
      for (const frame of frames) set.add(frame);
    }
    // Placeholder frames whose values differ, or frames naming different models, never verify a response.
    const exact = (row: Observation): Usage | null => {
      const set = usages.get(row.canonical_id);
      if (row.identity_strength !== 'response' || row.diagnostics.includes('CONFLICTING_MODEL') || !set || set.size !== 1) return null;
      const usage = JSON.parse([...set][0]!) as Usage;
      return usage.every((value) => value !== null) ? usage : null;
    };
    const verified = new Map<string, number>();
    const unverified = new Map<string, string>();
    for (const [key, members] of this.#members) {
      const group = transcripts.get(key) ?? [];
      const main = group.find((file) => !file.isSubagent) ?? group[0];
      const rows = [...new Set([...members].map(resolve))].map((id) => observations.get(id)).filter((row): row is Observation => row?.grain === 'request');
      if (!main || !rows.length) continue;
      if (!group.every((file) => this.#complete.has(file.relativePath))) {
        for (const row of rows) if (!unverified.has(row.canonical_id)) unverified.set(row.canonical_id, 'OUTPUT_RECONCILIATION_DEFERRED');
        diagnostics.push({ code: 'OUTPUT_RECONCILIATION_DEFERRED', message: 'Session transcripts were not all read completely in this scan; output stays unverified until one scan sees the whole session.', file: main.path });
        continue;
      }
      const costState = this.#costStates.get(key);
      const models = new Map<string, Observation[]>();
      for (const row of rows) {
        const list = models.get(row.model);
        if (list) list.push(row);
        else models.set(row.model, [row]);
      }
      for (const [model, selected] of models) {
        const expected = costState && Object.hasOwn(costState, model) ? object(costState[model]) : null;
        const totals = expected ? [token(expected.inputTokens), token(expected.cacheReadInputTokens), token(expected.cacheCreationInputTokens), token(expected.outputTokens)] : null;
        const values = selected.map(exact);
        const sums = [0, 0, 0, 0];
        for (const usage of values) if (usage) usage.forEach((value, index) => { sums[index] += value!; });
        if (totals && values.every((usage) => usage !== null) && totals.every((total, index) => total !== null && total === sums[index])) {
          selected.forEach((row, index) => verified.set(row.canonical_id, values[index]![3]!));
        } else {
          for (const row of selected) unverified.set(row.canonical_id, 'OUTPUT_RECONCILIATION_MISMATCH');
          diagnostics.push({ code: 'OUTPUT_RECONCILIATION_MISMATCH', message: costState ? 'Distinct response usage does not exactly equal the latest cost-state model totals; output stays unverified.' : 'No saved cost-state for this session; output stays unverified.', file: main.path });
        }
      }
    }
    for (const [id, output] of verified) {
      const row = observations.get(id)!;
      observations.set(id, { ...row, output_tokens: output, quality: 'observed', diagnostics: [...row.diagnostics.filter((code) => code !== 'OUTPUT_PLACEHOLDER_UNVERIFIED'), 'OUTPUT_RECONCILED_WITH_COST_STATE'] });
    }
    for (const [id, code] of unverified) {
      if (verified.has(id)) continue;
      const row = observations.get(id)!;
      observations.set(id, { ...row, diagnostics: [...new Set([...row.diagnostics, code])] });
    }
  }
}
