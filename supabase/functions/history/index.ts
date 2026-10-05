// POST /functions/v1/history
//
// Authorization: Bearer tm_<key>
// Body:
//   { "action": "status", "device_id": uuid }
//   { "action": "upload", "device_id": uuid, "reset": bool, "rows": [ ...up to 1000 ] }
//
// Imports daily usage totals from before a machine started reporting live.
// Rows are checked against a strict allowlist, like ingest: unknown fields
// reject the whole request.

import { bearerKey, json, rpc, sha256Hex, UUID_RE } from "../_shared/common.ts";

const MAX_ROWS = 1000;
const MAX_BODY = 256 * 1024;
const DAY_RE = /^\d{4}-\d{2}-\d{2}$/;

type Check = (v: unknown) => boolean;

const str = (max: number): Check => (v) => typeof v === "string" && v.length > 0 && v.length <= max;
const count: Check = (v) => Number.isSafeInteger(v) && (v as number) >= 0 && (v as number) <= 1e12;
const cost: Check = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 1e7;
const bool: Check = (v) => typeof v === "boolean";
const day: Check = (v) => {
  if (typeof v !== "string" || !DAY_RE.test(v)) return false;
  const t = Date.parse(`${v}T00:00:00Z`);
  return Number.isFinite(t) && t >= Date.parse("2020-01-01T00:00:00Z") && t <= Date.now() + 86_400_000;
};

const ROW_FIELDS: Record<string, Check> = {
  day,
  provider: str(64),
  model: str(128),
  is_subagent: bool,
  call_count: count,
  input_tokens: count,
  output_tokens: count,
  cache_read_tokens: count,
  cache_write_tokens: count,
  cost_usd: cost,
};

function validateRow(row: unknown): string | null {
  if (!row || typeof row !== "object" || Array.isArray(row)) return "row must be an object";
  const r = row as Record<string, unknown>;
  for (const [field, value] of Object.entries(r)) {
    const check = ROW_FIELDS[field];
    if (!check) return `unknown field: ${field}`;
    if (!check(value)) return `invalid ${field}`;
  }
  for (const field of Object.keys(ROW_FIELDS)) {
    if (!(field in r)) return `missing ${field}`;
  }
  return null;
}

Deno.serve(async (req) => {
  if (req.method !== "POST") return json(405, { error: "method_not_allowed" });

  const key = bearerKey(req);
  if (!key) return json(401, { error: "invalid_key" });

  const raw = await req.text();
  if (raw.length > MAX_BODY) return json(413, { error: "too_large" });

  let body: Record<string, unknown>;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error();
    body = parsed as Record<string, unknown>;
  } catch {
    return json(400, { error: "invalid_json" });
  }

  const deviceId = body.device_id;
  if (typeof deviceId !== "string" || !UUID_RE.test(deviceId)) return json(400, { error: "invalid_device_id" });

  let args: Record<string, unknown>;
  if (body.action === "status") {
    if (Object.keys(body).some((k) => k !== "action" && k !== "device_id")) return json(400, { error: "invalid_body" });
    args = { p_action: "status", p_device_id: deviceId };
  } else if (body.action === "upload") {
    if (Object.keys(body).some((k) => !["action", "device_id", "reset", "rows"].includes(k))) {
      return json(400, { error: "invalid_body" });
    }
    if (typeof body.reset !== "boolean") return json(400, { error: "invalid_reset" });
    const rows = body.rows;
    if (!Array.isArray(rows) || rows.length > MAX_ROWS || (rows.length === 0 && !body.reset)) {
      return json(400, { error: "invalid_batch_size" });
    }
    for (let i = 0; i < rows.length; i++) {
      const problem = validateRow(rows[i]);
      if (problem) return json(400, { error: "invalid_row", index: i, detail: problem });
    }
    args = { p_action: "upload", p_device_id: deviceId, p_reset: body.reset, p_rows: rows };
  } else {
    return json(400, { error: "unknown_action" });
  }

  const res = await rpc("ingest_history", { p_key_hash: await sha256Hex(key), ...args });
  if (!res.ok) {
    console.error("history rpc failed", res.status, await res.text());
    return json(502, { error: "storage_failed" });
  }

  const result = (await res.json()) as { error?: string };
  if (result.error === "invalid_key") return json(401, result);
  if (result.error === "rate_limited") return json(429, result);
  if (result.error) return json(400, result);
  return json(200, result);
});
