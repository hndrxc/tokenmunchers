import { createHash } from 'node:crypto';
import type { Observation } from '../../shared/src/observations.ts';

export interface Identity {
  canonicalId: string;
  strength: Observation['identity_strength'];
  requestId: string | null;
}

export function identity(entry: Record<string, unknown>, message: Record<string, unknown>, transcript: string): Identity | null {
  const response = typeof message.id === 'string' && message.id.trim() ? message.id : null;
  const request = typeof entry.requestId === 'string' && entry.requestId.trim() ? entry.requestId : null;
  if (response) return { canonicalId: `message:${response}`, strength: 'response', requestId: request };
  if (request) return { canonicalId: `request:${request}`, strength: 'request', requestId: request };
  if (typeof entry.uuid !== 'string' || !entry.uuid.trim()) return null;
  // UUID is not fork-stable. Never equate UUID-only records in independent transcripts.
  const digest = createHash('sha256').update(JSON.stringify([transcript, entry.uuid])).digest('hex');
  return { canonicalId: `scoped:${digest}`, strength: 'scoped', requestId: null };
}

/** Merge progressive representations, never sum them or treat output maxima as final. */
export function coalesce(previous: Observation, next: Observation): Observation {
  const diagnostics = new Set([...previous.diagnostics, ...next.diagnostics]);
  const merged = { ...previous };
  for (const field of ['input_tokens', 'cache_read_tokens', 'cache_write_tokens'] as const) {
    const oldValue = previous[field];
    const newValue = next[field];
    if (oldValue === null) merged[field] = newValue;
    else if (newValue !== null && newValue !== oldValue) {
      merged[field] = Math.max(oldValue, newValue);
      diagnostics.add('STREAM_USAGE_CHANGED');
    }
  }
  // The original response's time and attribution win over copied fork metadata.
  if (merged.timestamp === null) merged.timestamp = next.timestamp;
  if (merged.model === 'unknown') merged.model = next.model;
  else if (next.model !== 'unknown' && next.model !== merged.model) diagnostics.add('CONFLICTING_MODEL');
  if (diagnostics.has('CONFLICTING_PROVIDER')) merged.provider = 'unknown';
  else if (merged.provider === 'unknown') merged.provider = next.provider;
  else if (next.provider !== 'unknown' && next.provider !== merged.provider) {
    merged.provider = 'unknown';
    diagnostics.add('CONFLICTING_PROVIDER');
  }
  if (previous.timestamp && next.timestamp && next.timestamp < previous.timestamp) {
    merged.timestamp = next.timestamp;
    merged.session_id = next.session_id;
    merged.parent_session_id = next.parent_session_id;
    merged.is_subagent = next.is_subagent;
  }
  // A source-shaped assistant usage never proves final per-response output.
  merged.output_tokens = null;
  merged.quality = 'partial';
  merged.diagnostics = [...diagnostics];
  return merged;
}
