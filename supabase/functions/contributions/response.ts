/** Maps a publish_contributions RPC result onto the public HTTP contract. */
export function publicationResponse(result: { error?: string; retry_after_seconds?: unknown }): Response {
  const error = result.error;
  const status = !error ? 200
    : error === "invalid_key" ? 401
    : error === "rate_limited" || error === "staging_limit" ? 429
    : error.endsWith("conflict") || error === "stale_snapshot" ? 409
    : 400;
  const headers = new Headers({ "Content-Type": "application/json" });
  // Only the shared per-key minute window is time-based; staging_limit clears by committing or expiry.
  const wait = result.retry_after_seconds;
  if (error === "rate_limited" && typeof wait === "number" && Number.isInteger(wait) && wait >= 1 && wait <= 60) headers.set("Retry-After", String(wait));
  return new Response(JSON.stringify(result), { status, headers });
}
