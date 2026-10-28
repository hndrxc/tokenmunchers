export const MAX_BODY_BYTES = 256 * 1024;
export const MAX_CHUNKS = 1000;
export const MAX_OBSERVATIONS = 500;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
type Check = (value: unknown) => boolean;
const text = (max: number): Check => v => typeof v === "string" && v.length > 0 && v.length <= max && !/[\u0000-\u001f]/.test(v);
const oneOf = (...values: string[]): Check => v => typeof v === "string" && values.includes(v);
const nullable = (check: Check): Check => v => v === null || check(v);
const integer: Check = v => typeof v === "number" && Number.isSafeInteger(v) && v >= 0 && v <= 1e9;
export function timestamp(value: unknown): boolean {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(value)) return false;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) && parsed >= 0 && parsed <= Date.now() + 300_000 && new Date(parsed).toISOString().slice(0, 19) === value.slice(0, 19);
}
const observation: Record<string, Check> = {
  harness: oneOf("claude-code", "codex"), canonical_id: text(512),
  identity_strength: oneOf("response", "request", "scoped"), grain: oneOf("request", "reconstructed_interval"),
  timestamp, interval_start: nullable(timestamp), interval_end: nullable(timestamp),
  provider: text(64), model: text(128), session_id: nullable(text(128)), parent_session_id: nullable(text(128)),
  is_subagent: nullable(v => typeof v === "boolean"),
  input_tokens: nullable(integer), output_tokens: nullable(integer), cache_read_tokens: nullable(integer), cache_write_tokens: nullable(integer), call_count: nullable(integer),
  cost_usd: nullable(v => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 10_000),
  cost_basis: oneOf("reported", "estimated", "unknown"), quality: oneOf("observed", "reconstructed", "partial"),
};
function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
function fields(value: Record<string, unknown>, schema: Record<string, Check>): string | null {
  for (const key of Object.keys(value)) if (!Object.hasOwn(schema, key)) return `unknown field: ${key}`;
  for (const [key, check] of Object.entries(schema)) if (!Object.hasOwn(value, key) || !check(value[key])) return `invalid ${key}`;
  return null;
}
export function validateRequest(value: unknown): string | null {
  if (!object(value)) return "body must be an object";
  const common = { action: oneOf("begin", "chunk", "commit"), device_id: (v: unknown) => typeof v === "string" && UUID.test(v), harness: observation.harness, snapshot_id: (v: unknown) => typeof v === "string" && UUID.test(v) };
  const schema: Record<string, Check> = value.action === "begin" ? { ...common, cutoff: timestamp, chunk_count: (v: unknown) => typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= MAX_CHUNKS }
    : value.action === "chunk" ? { ...common, index: (v: unknown) => typeof v === "number" && Number.isInteger(v) && v >= 0 && v < MAX_CHUNKS, observations: Array.isArray, ...(Object.hasOwn(value, "superseded_canonical_ids") ? { superseded_canonical_ids: Array.isArray } : {}), ...(Object.hasOwn(value, "identity_aliases") ? { identity_aliases: Array.isArray } : {}) }
    : common;
  const problem = fields(value, schema);
  if (problem) return problem;
  if (value.action !== "chunk") return null;
  const rows = value.observations as unknown[];
  const superseded = (value.superseded_canonical_ids ?? []) as unknown[];
  const aliases = (value.identity_aliases ?? []) as unknown[];
  if (rows.length + superseded.length + aliases.length < 1 || rows.length + superseded.length + aliases.length > MAX_OBSERVATIONS) return "invalid observation count";
  if (superseded.some(id => !text(512)(id)) || new Set(superseded).size !== superseded.length) return "invalid superseded identities";
  const ids = new Set<string>(superseded as string[]);
  for (const alias of aliases) {
    if (!object(alias) || fields(alias, { from: text(512), to: text(512) }) || alias.from === alias.to || ids.has(alias.from as string)) return "invalid identity alias";
    ids.add(alias.from as string);
  }
  for (const row of rows) {
    if (!object(row)) return "observation must be an object";
    const invalid = fields(row, observation);
    if (invalid) return invalid;
    if (row.harness !== value.harness) return "harness mismatch";
    if ((row.cost_usd === null) !== (row.cost_basis === "unknown")) return "cost basis mismatch";
    if (row.grain === "reconstructed_interval" && (row.call_count !== null || row.quality !== "reconstructed")) return "invalid reconstructed quality/calls";
    const start = row.interval_start as string | null;
    const end = row.interval_end as string | null;
    if ((start === null) !== (end === null)) return "incomplete interval";
    if (row.grain === "reconstructed_interval" && (!start || !end)) return "incomplete reconstructed interval";
    if (start && end && (Date.parse(start) > Date.parse(end) || start.slice(0, 10) !== end.slice(0, 10) || start.slice(0, 10) !== (row.timestamp as string).slice(0, 10))) return "unattributable interval";
    if (ids.has(row.canonical_id as string)) return "duplicate canonical identity";
    ids.add(row.canonical_id as string);
  }
  return null;
}

/** Bound memory before decoding or parsing, including requests without Content-Length. */
export async function readBody(request: Request): Promise<string | null> {
  if (!request.body) return "";
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_BODY_BYTES) { await reader.cancel(); return null; }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
  return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
}
