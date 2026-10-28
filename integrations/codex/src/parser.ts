import { createHash } from 'node:crypto';
import type { AdapterResult, Diagnostic, Observation } from '../../shared/src/observations.ts';
import { equalUsage, integer, normalize, object, subtract, text, UsageError, wireUsage, type WireUsage } from './token-normalizer.ts';

interface Context {
  thread: string | null; parent: string | null; fork: string | null; model: string; provider: string;
  subagent: boolean | null; ownStart: number | null;
}
interface Snapshot { usage: WireUsage; time: string | null; model: string; index: number }
interface Candidate { observation: Observation; end: number }
export interface Segment { format: 'modern' | 'legacy'; line: number }
export interface CumulativeEvidence {
  canonicalId: string; session: string; start: WireUsage | null; end: WireUsage | null;
  startTime: string | null; endTime: string | null;
}
export interface ParsedRollout extends AdapterResult {
  segments: Segment[]; supersededCanonicalIds: string[];
  modernEvidence: CumulativeEvidence[]; legacyEvidence: CumulativeEvidence[];
}
const timestamp = (value: unknown): string | null => {
  const raw = text(value);
  return raw && /^\d{4}-\d{2}-\d{2}T/.test(raw) && Number.isFinite(Date.parse(raw)) ? new Date(raw).toISOString() : null;
};
const hash = (value: unknown): string => createHash('sha256').update(JSON.stringify(value)).digest('hex');

/** Consumes only top-level source records; embedded compaction checkpoints are not responses. */
export class RolloutParser {
  private context: Context = { thread: null, parent: null, fork: null, model: 'unknown', provider: 'unknown', subagent: null, ownStart: null };
  private previous: Snapshot | null = null;
  private snapshots: Snapshot[] = [];
  private pending: Candidate[] = [];
  private observations: Observation[] = [];
  private diagnostics: Diagnostic[] = [];
  private segments: Segment[] = [];
  private supersededCanonicalIds: string[] = [];
  private responseIds = new Set<string>();
  private modernEvidence: CumulativeEvidence[] = [];
  private legacyEvidence: CumulativeEvidence[] = [];
  private index = 0;
  private format: Segment['format'] | null = null;
  private source: string;
  constructor(source = 'selected-rollout') { this.source = source; }
  private report(code: string, line: number, message: string): void {
    this.diagnostics.push({ code, message, file: this.source, line });
  }
  private base(time: string | null): Omit<Observation, 'canonical_id' | 'identity_strength' | 'grain' | 'input_tokens' | 'output_tokens' | 'cache_read_tokens' | 'cache_write_tokens' | 'call_count' | 'quality'> {
    return { harness: 'codex', timestamp: time, interval_start: null, interval_end: null,
      provider: this.context.provider, model: this.context.model, session_id: this.context.thread,
      parent_session_id: this.context.parent ?? this.context.fork, is_subagent: this.context.subagent,
      cost_usd: null, cost_basis: 'unknown', diagnostics: ['UNPRICED', ...(this.context.provider === 'unknown' ? ['UNKNOWN_PROVIDER'] : [])] };
  }
  /** Segment transitions are also diagnostics so collector preview can show per-file format capability. */
  private segment(format: Segment['format'], line: number): void {
    if (format === this.format) return;
    this.segments.push({ format, line });
    this.format = format;
    if (format === 'modern') this.report('SEGMENT_MODERN', line, 'Modern per-response usage segment begins.');
    else this.report('SEGMENT_LEGACY', line, 'Legacy cumulative token-count segment begins; intervals are reconstructed.');
  }
  consume(value: unknown, line: number): void {
    const row = object(value);
    const payload = object(row?.payload);
    if (!row || !payload) return;
    const time = timestamp(row.timestamp);
    // Forked/spawned rollouts replay the parent's history (including its session_meta) before the child boundary.
    const inherited = this.context.ownStart !== null && integer(row.ordinal) && row.ordinal < this.context.ownStart;
    if (row.type === 'session_meta') {
      const id = text(payload.id);
      if (this.context.thread && id !== this.context.thread && (inherited || (id !== null && (id === this.context.fork || id === this.context.parent)))) {
        this.report('INHERITED_CONTEXT', line, 'Replayed lineage session metadata does not replace this rollout identity.');
        return;
      }
      if (this.context.thread && id !== this.context.thread) {
        this.report('SESSION_CONTEXT_CHANGED', line, 'Session identity changed inside one rollout; uncertain counters reset.');
        this.previous = null; this.snapshots = [];
      }
      this.context.thread = id;
      this.context.parent = text(payload.parent_thread_id);
      this.context.fork = text(payload.forked_from_id);
      const provider = text(payload.model_provider) ?? 'unknown';
      if (provider !== this.context.provider) { this.previous = null; this.snapshots = []; }
      this.context.provider = provider;
      this.context.ownStart = integer(payload.subagent_history_start_ordinal) ? payload.subagent_history_start_ordinal : null;
      const source = object(payload.source);
      // Real 0.154.0 writes `subagent`; pinned upstream serde spelling is `sub_agent`.
      const subagent = source?.subagent ?? source?.sub_agent;
      const spawn = object(object(subagent)?.thread_spawn);
      this.context.parent ??= text(spawn?.parent_thread_id);
      this.context.subagent = this.context.parent || subagent !== undefined ? true : typeof payload.source === 'string' ? false : null;
      return;
    }
    if (row.type === 'turn_context') {
      if (inherited) return;
      const next = text(payload.model) ?? 'unknown';
      if (this.context.model !== next) {
        if (this.previous) this.report('MODEL_CHANGED', line, 'Model changed; cross-model legacy delta withheld.');
        this.previous = null; this.snapshots = [];
      }
      this.context.model = next;
      return;
    }
    if (row.type === 'compacted') {
      this.previous = null; this.snapshots = [];
      this.report('COMPACTION_BOUNDARY', line, 'Compaction checkpoint ignored as an accounting record; legacy baseline reset.');
      return;
    }
    if (row.type !== 'token_usage_record' && !(row.type === 'event_msg' && payload.type === 'token_count')) return;
    const format = row.type === 'token_usage_record' ? 'modern' : 'legacy';
    if (this.context.ownStart !== null) {
      if (!integer(row.ordinal)) {
        this.report('MISSING_OWNERSHIP_ORDINAL', line, 'Record cannot be placed relative to explicit child ownership boundary.');
        return;
      }
      if (row.ordinal < this.context.ownStart) {
        this.report('INHERITED_PREFIX', line, 'Inherited child context excluded from accounting.'); return;
      }
    }
    if (format === 'modern') this.segment(format, line);
    try {
      if (format === 'modern') this.modern(payload, time, line);
      else this.legacy(payload, time, line);
    } catch (error) {
      this.report(error instanceof UsageError ? error.code : 'MALFORMED_USAGE', line, 'Usage metadata violates the supported wire invariants; record withheld.');
      this.previous = null; this.snapshots = [];
    }
  }
  private modern(payload: Record<string, unknown>, time: string | null, line: number): void {
    const response = text(payload.response_id);
    const thread = text(payload.thread_id);
    if (!response || !thread) { this.report('MISSING_RESPONSE_ID', line, 'Modern record lacks canonical response or thread identity.'); return; }
    if (this.context.thread && thread !== this.context.thread) {
      this.report('INHERITED_RESPONSE', line, 'Response belongs to another thread and is not child-local work.'); return;
    }
    const usage = wireUsage(payload.usage);
    const tokens = normalize(usage);
    if (this.responseIds.has(response)) {
      const base = this.base(time);
      // Keep both copies for global conflict detection, but never rewind continuity on replay.
      this.observations.push({ ...base, canonical_id: response, identity_strength: 'response', grain: 'request',
        session_id: thread, ...tokens, call_count: 1, quality: 'observed' });
      return;
    }
    this.responseIds.add(response);
    // Only cumulative metadata is used as continuity evidence, never as response usage.
    let cumulative: WireUsage | null = null;
    let before: WireUsage | null = null;
    try {
      cumulative = wireUsage(payload.thread_token_usage);
      normalize(cumulative);
      before = subtract(cumulative, usage);
      if (!before) throw new UsageError('MALFORMED_TOTAL');
      normalize(before);
    } catch {
      cumulative = null; before = null;
      this.report('MISSING_STITCH_EVIDENCE', line, 'Thread cumulative metadata is invalid or unavailable; legacy transition withheld.');
    }
    this.modernEvidence.push({ canonicalId: response, session: thread, start: before, end: cumulative, startTime: time, endTime: time });
    const matching = before ? this.snapshots.findLast(snapshot => equalUsage(snapshot.usage, before)) : undefined;
    if (this.pending.length) {
      const safe = matching ? this.pending.filter(candidate => candidate.end <= matching.index) : [];
      this.supersededCanonicalIds.push(...this.pending.filter(candidate => !safe.includes(candidate)).map(candidate => candidate.observation.canonical_id));
      this.observations.push(...safe.map(candidate => candidate.observation));
      if (safe.length !== this.pending.length) this.report('UNCERTAIN_TRANSITION', line, 'Legacy intervals without an exact cumulative pre-response boundary withheld to avoid overlap.');
      else this.report('STITCH_CONFIRMED', line, 'Legacy end baseline exactly matches thread cumulative usage minus this response.');
      this.pending = [];
    }
    const base = this.base(time);
    this.observations.push({ ...base, canonical_id: response, identity_strength: 'response', grain: 'request',
      session_id: thread, ...tokens, call_count: 1, quality: 'observed', diagnostics: [...base.diagnostics, ...(!time ? ['MISSING_TIMESTAMP'] : [])] });
    this.previous = cumulative ? { usage: cumulative, time, model: this.context.model, index: ++this.index } : null;
    // Never stitch to a stale pre-reset snapshot: this response starts a new evidence segment.
    this.snapshots = this.previous ? [this.previous] : [];
  }
  private legacy(payload: Record<string, unknown>, time: string | null, line: number): void {
    const info = object(payload.info);
    if (!info) { this.report('NO_USAGE_SNAPSHOT', line, 'Token-count event has no usage metadata.'); return; }
    const total = wireUsage(info.total_token_usage);
    const last = wireUsage(info.last_token_usage);
    if (integer(info.model_context_window) && total.total_tokens === info.model_context_window && total.total_tokens > 0 &&
      total.input_tokens === 0 && total.output_tokens === 0 && total.cached_input_tokens === 0 && total.cache_write_input_tokens === 0) {
      this.report('SYNTHETIC_OVERFLOW', line, 'Context-window synthetic counter fill discarded; baseline reset.');
      this.previous = null; this.snapshots = []; return;
    }
    normalize(total); normalize(last);
    // Modern rollouts mirror each response's thread cumulative as token_count; only unexplained snapshots open a legacy segment.
    if (!(this.format === 'modern' && this.previous && equalUsage(total, this.previous.usage))) this.segment('legacy', line);
    if (!this.context.thread) { this.report('MISSING_SESSION_ID', line, 'Legacy interval has no stable transcript session identity.'); return; }
    if ((this.context.fork || this.context.parent) && this.context.ownStart === null) {
      this.report('UNOWNED_FORK_INTERVAL', line, 'Child or fork legacy range lacks explicit inherited-prefix boundary; no child ownership assumed.'); return;
    }
    const current: Snapshot = { usage: total, time, model: this.context.model, index: ++this.index };
    const previous = this.previous;
    this.previous = current;
    this.snapshots.push(current);
    if (!previous) { this.report('LEGACY_BASELINE_ONLY', line, 'Initial cumulative snapshot establishes a baseline, not attributable usage.'); return; }
    if (equalUsage(total, previous.usage)) { this.report('REPEATED_SNAPSHOT', line, 'Unchanged cumulative snapshot contributes no additional usage.'); return; }
    const delta = subtract(total, previous.usage);
    if (!delta) {
      this.snapshots = [current];
      this.report('COUNTER_RESET', line, 'Non-monotonic counters reset the legacy baseline; no negative usage emitted.'); return;
    }
    // A recognized append must agree in every wire category with upstream last_token_usage.
    if (!equalUsage(delta, last) || previous.model !== current.model || (previous.time && time && time < previous.time)) {
      this.report('UNRECOGNIZED_INTERVAL', line, 'Counter delta is not a recognized monotonic append in a single model/time segment.'); return;
    }
    const tokens = normalize(delta);
    if (delta.total_tokens === 0) return;
    const base = this.base(time);
    const canonicalId = `legacy:${hash([this.context.thread, previous.time, time, previous.usage, total])}`;
    this.legacyEvidence.push({ canonicalId, session: this.context.thread, start: previous.usage, end: total,
      startTime: previous.time, endTime: time });
    this.pending.push({ end: current.index, observation: { ...base,
      canonical_id: canonicalId,
      identity_strength: 'scoped', grain: 'reconstructed_interval', interval_start: previous.time, interval_end: time,
      ...tokens, call_count: null, quality: 'reconstructed',
      diagnostics: [...base.diagnostics, 'LEGACY_RECONSTRUCTED', 'UNKNOWN_CALL_COUNT', 'SCOPED_SINGLE_OWNER', ...(!time ? ['MISSING_TIMESTAMP'] : [])] } });
  }
  finish(): ParsedRollout {
    this.observations.push(...this.pending.map(candidate => candidate.observation));
    this.pending = [];
    return { observations: this.observations, diagnostics: this.diagnostics, segments: this.segments,
      supersededCanonicalIds: this.supersededCanonicalIds, modernEvidence: this.modernEvidence, legacyEvidence: this.legacyEvidence };
  }
}

/** Strong response IDs are harness-global; conflicting copies are withheld, never summed. */
export function deduplicate(observations: Observation[], diagnostics: Diagnostic[], conflictingCanonicalIds: string[] = []): Observation[] {
  const accepted = new Map<string, Observation>();
  const conflicts = new Set<string>();
  for (const observation of observations) {
    const id = observation.canonical_id;
    if (conflicts.has(id)) continue;
    const previous = accepted.get(id);
    if (!previous) { accepted.set(id, observation); continue; }
    const accounting = (item: Observation) => [item.grain, item.input_tokens, item.output_tokens, item.cache_read_tokens, item.cache_write_tokens, item.call_count];
    if (JSON.stringify(accounting(previous)) !== JSON.stringify(accounting(observation))) {
      conflicts.add(id); accepted.delete(id);
      conflictingCanonicalIds.push(id);
      diagnostics.push({ code: 'CONFLICTING_RESPONSE', message: 'Canonical identity has conflicting usage; withheld from this scan.' });
    } else {
      if (previous.provider === 'unknown' && observation.provider !== 'unknown') previous.provider = observation.provider;
      if (previous.model === 'unknown' && observation.model !== 'unknown') previous.model = observation.model;
      diagnostics.push({ code: 'DUPLICATE_RESPONSE', message: 'Repeated canonical contribution ignored.' });
    }
  }
  return [...accepted.values()];
}
