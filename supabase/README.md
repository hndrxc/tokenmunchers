# tokenmunchers backend (Supabase)

The database schema, row-level security (RLS), scheduled jobs and Edge Functions behind [tokenmunchers](../README.md).

Production project: `tokenmunchers` (`fywgkzqgtitpwijweojk`, us-east-2).

## Architecture

```
OMP plugin ──POST /functions/v1/ingest──▶ ingest  ──rpc ingest()─────────▶ Postgres ──Realtime──▶ dashboard
           ──POST /functions/v1/history─▶ history ──rpc ingest_history()──┘   │
                                                                              └─ pg_cron: rollups, retention, presence
```

- The Edge Functions check every field of a request against an allowlist and hash the API key. They then call `security definer` functions with the secret key. No client role can write usage data.
- Each API key is limited to 600 events per minute, shared between `ingest` and `history` (a history request counts as 10).
- Measured on the live project: an ingest round trip takes ~0.2–0.3 s warm. From a model call finishing to the dashboard showing it takes 0.45–1.0 s.

## Data model

| Object | Contents | Retention |
| --- | --- | --- |
| `allowlist` | Invited emails. Sign-up is rejected unless the email is listed. | Managed by hand |
| `profiles` | Handle, display name, avatar. Created by a trigger on sign-up. | Until account deletion |
| `api_keys` | SHA-256 hash and a display prefix. The plaintext `tm_…` key is shown once, when created. | Until revoked or deleted |
| `usage_events` | One row per model call. Feeds Realtime, the activity feed, and the today/week totals. | Until the start of UTC day today−8 |
| `usage_daily` | Daily totals per user, provider, model and subagent flag, computed from `usage_events` | Forever |
| `usage_history` | Imported pre-install daily totals per user and device (`/munch backfill`) | Forever (each upload replaces that device's previous one) |
| `usage_daily_all` (view) | `usage_daily` + `usage_history` | n/a |
| `live_sessions` | Presence. A user is live if a heartbeat arrived in the last 90 s. | Pruned after 10 min |

Deleting an account cascades to every row the user owns.

### Why the retention windows line up

`ingest` rejects events older than 7 days, so a late event (from a retry queue) always lands on a UTC day between today−7 and today. Every 10 minutes, the `tokenmunchers-rollup` job recomputes the daily totals for exactly those days. Raw rows are kept until the start of today−8, so a recomputed day always still has all its raw rows. A recompute therefore never replaces a correct daily total with a partial one.

History and live data never overlap: the plugin uploads history only for calls made before that machine's first live event.

### Read access

Members (signed-in users whose profile exists) can read every usage table, view and leaderboard function, plus other members' profiles. Each user can read only their own API keys. Clients have no write access to usage data, and can't call `ingest` or `ingest_history`.

### Functions

| Function | Callable by | Description |
| --- | --- | --- |
| `leaderboard_since(timestamptz)` | Members | Per-user totals since a time (up to 7 days back), from raw events |
| `leaderboard_all_time()` | Members | History, plus daily totals for past days, plus raw events for today |
| `create_api_key(label)` | Members | Creates a key and returns its plaintext once |
| `delete_my_account()` | Members | Deletes the caller's auth user (cascades) |
| `ingest(key_hash, events)` | `service_role` | Stores usage events, updates presence, applies the rate limit |
| `ingest_history(key_hash, action, device_id, reset, rows)` | `service_role` | History status and uploads |

### Scheduled jobs (pg_cron)

| Job | Schedule | Action |
| --- | --- | --- |
| `tokenmunchers-rollup` | Every 10 min | Recomputes `usage_daily` for today−7 to today |
| `tokenmunchers-retention` | Daily 03:15 UTC | Deletes raw events before today−8 and old rate-limit windows |
| `tokenmunchers-stale-presence` | Every 5 min | Removes sessions with no heartbeat for 10 min |

## Edge Functions

Both functions authenticate plugin API keys themselves (`Authorization: Bearer tm_…`), so they're deployed with `verify_jwt = false` (see [`config.toml`](config.toml)). Shared helpers live in [`functions/_shared/`](functions/_shared/common.ts).

### `POST /functions/v1/ingest`

The body is one event, or `{ "events": [...] }` with up to 100 events. The body can be at most 64 KB. Event kinds are `usage`, `session_start`, `heartbeat` and `session_end`; the field allowlist is in [`functions/ingest/index.ts`](functions/ingest/index.ts). Usage events must be less than 7 days old.

| Status | Meaning |
| --- | --- |
| 200 | `{ accepted, inserted }`. Repeated `event_id`s are accepted but not inserted. |
| 400 | Invalid body or event (`index` and `detail` say which). Nothing is stored. |
| 401 | Unknown or revoked key |
| 413 | Body too large |
| 429 | Rate limited |

### `POST /functions/v1/history`

```jsonc
{ "action": "status", "device_id": "<uuid>" }
// → { "first_event_at": "<timestamp>|null", "device": { "rows", "days", "total_tokens", "uploaded_at" } }

{ "action": "upload", "device_id": "<uuid>", "reset": true, "rows": [ /* ≤ 1000 */ ] }
// → { "written": n }
```

A row has `day`, `provider`, `model`, `is_subagent`, `call_count`, the four token counts and `cost_usd`; unknown fields are rejected. `reset: true` deletes this device's previous upload first. The plugin sets it on the first chunk of each import. Bodies can be at most 256 KB.

## Project setup

To run your own instance:

1. **Create a Supabase project** and link it (see [Deploying](#deploying)).
2. **Apply the migrations:** `supabase db push`. The pg_cron and pgcrypto extensions are enabled by the first migration.
3. **Deploy the Edge Functions:** `supabase functions deploy ingest history`.
4. **Enable OAuth providers.** Authentication → Sign In / Providers → GitHub and/or Discord. Each needs an OAuth app on that platform, with the callback URL shown in Supabase. Turn **off** the Email provider; it isn't used.
5. **Set redirect URLs.** Authentication → URL Configuration: set Site URL to the dashboard URL. Under Redirect URLs, add the production URL and `http://localhost:3000/**`.
6. **Point the dashboard and plugin at the project.** Use the dashboard's env vars (see the [dashboard README](../dashboard/README.md)). For the plugin, set `TOKENMUNCHERS_URL`, or change `DEFAULT_INGEST_URL` in [`plugin/src/config.ts`](../plugin/src/config.ts).

### Inviting members

In the SQL editor:

```sql
insert into public.allowlist (email) values ('friend@example.com');
```

Use the email of the person's GitHub or Discord account. It must be lowercase.

## Deploying

```bash
# One-time: install the CLI (or prefix every command with `npx supabase@latest`)
brew install supabase/tap/supabase

supabase login
supabase link --project-ref fywgkzqgtitpwijweojk   # asks for the database password

supabase db push --dry-run     # check what will be applied
supabase db push
supabase functions deploy ingest history
```

Deploy server changes **before** publishing a plugin version that depends on them.

If `supabase migration list` shows versions that don't match the local file names (for example, migrations applied by hand in the SQL editor), fix the history table with `supabase migration repair --status applied|reverted <version>` before pushing. This only edits the history table and never runs SQL.

## Design notes

- **Ranking:** by tokens by default, with a Cost toggle. Subagent calls count by default, with a toggle to exclude them.
- **Presence** is the `live_sessions` table pushed over Realtime, rather than a Realtime presence channel. An Edge Function can't hold a presence connection open, and the table survives page reloads.
- **Rollups** run every 10 minutes instead of nightly. "Today" and "All time" combine daily totals with exact raw counts, so totals never lag behind the feed.
- **Cost** is what OMP reports for live events. Imported history uses OMP's stats estimates, which price subscription usage at public API rates.
