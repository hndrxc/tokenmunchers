import type { Observation } from '../../shared/src/observations.ts';
import type { Transcript } from './discovery.ts';
import { identity } from './identity.ts';

export function object(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null;
}
/** Mirrors shared metadata() bounds; out-of-range values become unavailable instead of failing the ledger record. */
export const TOKEN_LIMIT = 1e9;
export const COST_LIMIT = 10_000;
export function token(value: unknown): number | null {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 && value <= TOKEN_LIMIT ? value : null;
}
export function timestamp(value: unknown): string | null {
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? new Date(value).toISOString() : null;
}

// SDK ModelUsage.provider is explicit nonsecret infrastructure metadata. No endpoint,
// authorization header, settings/environment, model-name or provider guessing is used.
const providers: Record<string, string> = {
  firstParty: 'anthropic', anthropic: 'anthropic', bedrock: 'bedrock', vertex: 'vertex',
  foundry: 'foundry', anthropicAws: 'anthropic-aws', mantle: 'mantle', gateway: 'gateway',
};
export function provider(value: unknown): string {
  return typeof value === 'string' && Object.hasOwn(providers, value) ? providers[value]! : 'unknown';
}

/** `output` is the frame's saved output count: evidence for cost-state reconciliation only, never emitted unverified. */
export function normalize(entry: Record<string, unknown>, file: Transcript): { observation: Observation; requestId: string | null; output: number | null } | null {
  if (entry.type !== 'assistant' || entry.isApiErrorMessage === true) return null;
  const message = object(entry.message);
  const usage = object(message?.usage);
  if (!message || !usage) return null;
  const key = identity(entry, message, file.relativePath);
  if (!key) return null;
  const diagnostics = ['OUTPUT_PLACEHOLDER_UNVERIFIED', 'COST_UNAVAILABLE'];
  if (key.strength === 'scoped') diagnostics.push('SCOPED_IDENTITY_FORK_UNCERTAIN');
  const values = {
    input_tokens: token(usage.input_tokens),
    cache_read_tokens: token(usage.cache_read_input_tokens),
    cache_write_tokens: token(usage.cache_creation_input_tokens),
  };
  for (const [name, value] of Object.entries(values)) {
    if (value === null) diagnostics.push(usage[name === 'cache_read_tokens' ? 'cache_read_input_tokens' : name === 'cache_write_tokens' ? 'cache_creation_input_tokens' : name] === undefined ? 'TOKEN_CATEGORY_UNAVAILABLE' : 'MALFORMED_TOKEN_COUNT');
  }
  const infrastructure = provider(entry.provider);
  if (infrastructure === 'unknown') diagnostics.push('PROVIDER_UNVERIFIED');
  const observedAt = timestamp(entry.timestamp);
  if (observedAt === null) diagnostics.push('TIMESTAMP_UNAVAILABLE');
  // Parent Task/Agent content is not parsed into observations. Only its actual
  // assistant API response usage, if recorded, is counted; dispatch adds no call.
  return {
    requestId: key.requestId,
    output: token(usage.output_tokens),
    observation: {
      harness: 'claude-code', canonical_id: key.canonicalId, identity_strength: key.strength,
      grain: 'request', timestamp: observedAt, interval_start: null, interval_end: null,
      provider: infrastructure, model: typeof message.model === 'string' && message.model ? message.model : 'unknown',
      session_id: file.isSubagent ? file.sessionId : typeof entry.sessionId === 'string' ? entry.sessionId : file.sessionId,
      parent_session_id: file.parentSessionId,
      is_subagent: file.isSubagent || entry.isSidechain === true,
      ...values, output_tokens: null, call_count: 1, cost_usd: null, cost_basis: 'unknown',
      quality: 'partial', diagnostics: [...new Set(diagnostics)],
    },
  };
}
