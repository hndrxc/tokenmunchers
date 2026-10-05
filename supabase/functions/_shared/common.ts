// Helpers shared by the plugin-facing Edge Functions (ingest, history).

export const KEY_RE = /^tm_[A-Za-z0-9_-]{43}$/;
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

export async function sha256Hex(input: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(input));
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

function secretKey(): string {
  const keys = Deno.env.get("SUPABASE_SECRET_KEYS");
  if (keys) {
    try {
      const parsed = JSON.parse(keys) as Record<string, string>;
      if (parsed.default) return parsed.default;
    } catch {
      // fall through to the legacy key
    }
  }
  const legacy = Deno.env.get("SUPABASE_SERVICE_ROLE_KEY");
  if (!legacy) throw new Error("no secret key available");
  return legacy;
}

const SUPABASE_URL = Deno.env.get("SUPABASE_URL")!;
const SECRET_KEY = secretKey();

/** Calls a service-role-only RPC. */
export function rpc(name: string, args: unknown): Promise<Response> {
  return fetch(`${SUPABASE_URL}/rest/v1/rpc/${name}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", apikey: SECRET_KEY },
    body: JSON.stringify(args),
  });
}

/** The plugin key from the Authorization header, or null if malformed. */
export function bearerKey(req: Request): string | null {
  const key = req.headers.get("authorization")?.replace(/^Bearer\s+/i, "") ?? "";
  return KEY_RE.test(key) ? key : null;
}
