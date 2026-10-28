// Metadata-only collector snapshots. OMP ingest/history are intentionally separate.
import { bearerKey, json, rpc, sha256Hex } from "../_shared/common.ts";
import { publicationResponse } from "./response.ts";
import { readBody, validateRequest } from "./validation.ts";

Deno.serve(async request => {
  if (request.method !== "POST") return json(405, { error: "method_not_allowed" });
  const key = bearerKey(request);
  if (!key) return json(401, { error: "invalid_key" });
  let body: unknown;
  try {
    const raw = await readBody(request);
    if (raw === null) return json(413, { error: "too_large" });
    body = JSON.parse(raw);
  } catch { return json(400, { error: "invalid_json" }); }
  const problem = validateRequest(body);
  if (problem) return json(400, { error: "invalid_body", detail: problem });
  try {
    const response = await rpc("publish_contributions", { p_key_hash: await sha256Hex(key), p_request: body });
    if (!response.ok) return json(502, { error: "storage_failed" });
    return publicationResponse(await response.json() as { error?: string; retry_after_seconds?: unknown });
  } catch { return json(502, { error: "storage_failed" }); }
});
