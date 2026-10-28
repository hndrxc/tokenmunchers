export type Harness = 'claude-code' | 'codex';
export type Grain = 'request' | 'reconstructed_interval' | 'session_summary';
export interface Observation {
  harness: Harness;
  canonical_id: string;
  identity_strength: 'response' | 'request' | 'scoped';
  grain: Grain;
  timestamp: string | null;
  interval_start: string | null;
  interval_end: string | null;
  provider: string;
  model: string;
  session_id: string | null;
  parent_session_id: string | null;
  is_subagent: boolean | null;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  call_count: number | null;
  cost_usd: number | null;
  cost_basis: 'reported' | 'estimated' | 'unknown';
  quality: 'observed' | 'reconstructed' | 'partial' | 'summary';
  diagnostics: string[];
}
export interface Diagnostic { code: string; message: string; file?: string; line?: number }
export interface ScanOptions {
  maxBytes?: number;
  maxLineBytes?: number;
  exclude?: string[];
  cursor?: { file: string; offset: number; line: number; device: number; inode: number; discardLine?: boolean } | null;
  continuation?: { nextFile?: string };
}
export interface AdapterResult {
  observations: Observation[];
  diagnostics: Diagnostic[];
  cursor?: ScanOptions['cursor'] | null;
  continuation?: { nextFile: string | null };
  supersededCanonicalIds?: string[];
  identityAliases?: { from: string; to: string }[];
  conflictingCanonicalIds?: string[];
  deferredFiles?: string[];
  [key: string]: unknown;
}
export type Adapter = (root: string, options?: ScanOptions) => Promise<AdapterResult>;
