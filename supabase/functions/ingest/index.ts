// POST /functions/v1/ingest
//
// Authorization: Bearer tm_<key>
// Body: one event object, or { "events": [ ...up to 100 ] }
//
// Every field is checked against a strict allowlist; unknown fields reject the
// whole request so nothing beyond usage metadata can ever be stored.

import { bearerKey, json, rpc, sha256Hex, UUID_RE } from "../_shared/common.ts";

const MAX_BATCH = 100;
const MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;
const MAX_SKEW_MS = 5 * 60 * 1000;

type Check = (v: unknown) => boolean;

const str = (max: number): Check => (v) => typeof v === "string" && v.length > 0 && v.length <= max;
const tokens: Check = (v) => Number.isSafeInteger(v) && (v as number) >= 0 && (v as number) <= 1e9;
const cost: Check = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 10_000;
const bool: Check = (v) => typeof v === "boolean";
const uuid: Check = (v) => typeof v === "string" && UUID_RE.test(v);
const timestamp: Check = (v) => {
  if (typeof v !== "string" || v.length > 40) return false;
  const t = Date.parse(v);
  const now = Date.now();
  return Number.isFinite(t) && t > now - MAX_AGE_MS && t < now + MAX_SKEW_MS;
};

const presenceFields = {
  kind: () => true,
  session_id: str(128),
  provider: str(64),
  model: str(128),
  started_at: timestamp,
  client_version: str(32),
};

const SCHEMAS: Record<string, { fields: Record<string, Check>; required: string[] }> = {
  usage: {
    fields: {
      kind: () => true,
      event_id: uuid,
      session_id: str(128),
      ts: timestamp,
      provider: str(64),
      model: str(128),
      input_tokens: tokens,
      output_tokens: tokens,
      cache_read_tokens: tokens,
      cache_write_tokens: tokens,
      cost_usd: cost,
      is_subagent: bool,
      client_version: str(32),
    },
    required: [
      "event_id", "session_id", "ts", "provider", "model",
      "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
      "cost_usd", "is_subagent",
    ],
  },
  session_start: { fields: presenceFields, required: ["session_id"] },
  heartbeat: { fields: presenceFields, required: ["session_id"] },
  session_end: {
    fields: { kind: () => true, session_id: str(128), client_version: str(32) },
    required: ["session_id"],
  },
};

function validate(event: unknown): string | null {
  if (!event || typeof event !== "object" || Array.isArray(event)) return "event must be an object";
  const e = event as Record<string, unknown>;
  const schema = typeof e.kind === "string" ? SCHEMAS[e.kind] : undefined;
  if (!schema) return "unknown kind";
  for (const [field, value] of Object.entries(e)) {
    const check = schema.fields[field];
    if (!check) return `unknown field: ${field}`;
    if (!check(value)) return `invalid ${field}`;
  }
  for (const field of schema.required) {
    if (!(field in e)) return `missing ${field}`;
  }
  return null;
}

Deno.serve(async (req) => {
  if (req.method !== "POST") return json(405, { error: "method_not_allowed" });

  const key = bearerKey(req);
  if (!key) return json(401, { error: "invalid_key" });

  const raw = await req.text();
  if (raw.length > 64 * 1024) return json(413, { error: "too_large" });

  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    return json(400, { error: "invalid_json" });
  }

  let events: unknown[];
  if (body && typeof body === "object" && !Array.isArray(body) && "events" in body) {
    const b = body as Record<string, unknown>;
    if (Object.keys(b).length !== 1 || !Array.isArray(b.events)) {
      return json(400, { error: "invalid_body" });
    }
    events = b.events;
  } else {
    events = [body];
  }
  if (events.length === 0 || events.length > MAX_BATCH) return json(400, { error: "invalid_batch_size" });

  for (let i = 0; i < events.length; i++) {
    const problem = validate(events[i]);
    if (problem) return json(400, { error: "invalid_event", index: i, detail: problem });
  }

  const res = await rpc("ingest", { p_key_hash: await sha256Hex(key), p_events: events });
  if (!res.ok) {
    console.error("ingest rpc failed", res.status, await res.text());
    return json(502, { error: "storage_failed" });
  }

  const result = (await res.json()) as { error?: string; accepted?: number; inserted?: number };
  if (result.error === "invalid_key") return json(401, result);
  if (result.error === "rate_limited") return json(429, result);
  return json(200, result);
});
