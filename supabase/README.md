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
- Each API key is limited to 600 hits per minute, shared between `ingest`, `history` and `contributions` (history and contribution requests count as 10).
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
| `collector_contributions` | Permanently owned Claude Code/Codex canonical metadata, nullable accounting, history/live partition and supersession reservations | Until account deletion; source cleanup never deletes accepted rows |
| `usage_daily_all` (view) | OMP `usage_daily` + `usage_history` + published collector contributions, with unknown-propagating sums and quality flags | n/a |
| `live_sessions` | Presence. A user is live if a heartbeat arrived in the last 90 s. | Pruned after 10 min |

Deleting an account cascades to every row the user owns.

### Why the retention windows line up

`ingest` rejects events older than 7 days, so a late event (from a retry queue) always lands on a UTC day between today−7 and today. Every 10 minutes, the `tokenmunchers-rollup` job recomputes the daily totals for exactly those days. Raw rows are kept until the start of today−8, so a recomputed day always still has all its raw rows. A recompute therefore never replaces a correct daily total with a partial one.

History and live data never overlap: the plugin uploads history only for calls made before that machine's first live event.

### Read access

Members (signed-in users whose profile exists) can read every usage table, view and leaderboard function, plus other members' profiles. Each user can read only their own API keys. Clients have no write access to usage data, cannot read private staging tables, and cannot call `ingest`, `ingest_history` or `publish_contributions`.

The Discord bot operates under a dedicated, read-only role (`tokenmunchers_discord_reader`) with no direct table access and no access to member-only functions. It can only execute the security-definer wrapper `public.discord_leaderboard(p_period)`.
### Functions

| Function | Callable by | Description |
| --- | --- | --- |
| `leaderboard_since(timestamptz)` | Members | OMP raw events (up to 7 days back) plus collector contributions since the original timestamp |
| `leaderboard_all_time()` | Members | OMP history, past daily rollups and today's raw events, plus published collector contributions once |
| `create_api_key(label)` | Members | Creates a key and returns its plaintext once |
| `delete_my_account()` | Members | Deletes the caller's auth user (cascades) |
| `ingest(key_hash, events)` | `service_role` | Stores usage events, updates presence, applies the rate limit |
| `ingest_history(key_hash, action, device_id, reset, rows)` | `service_role` | History status and uploads |
| `publish_contributions(key_hash, request)` | `service_role` | Stages and atomically publishes collector metadata; canonical IDs are permanently deduplicated per user/harness |
| `discord_leaderboard(p_period)` | `tokenmunchers_discord_reader` | Aggregated period totals (`today`/`week`/`all`); zeros only for profiles with no usage, never for unknown accounting |

### Scheduled jobs (pg_cron)

| Job | Schedule | Action |
| --- | --- | --- |
| `tokenmunchers-rollup` | Every 10 min | Recomputes `usage_daily` for today−7 to today |
| `tokenmunchers-retention` | Daily 03:15 UTC | Deletes raw events before today−8 and old rate-limit windows |
| `tokenmunchers-stale-presence` | Every 5 min | Removes sessions with no heartbeat for 10 min |

## Edge Functions

All three functions authenticate API keys themselves (`Authorization: Bearer tm_…`), so they're deployed with `verify_jwt = false` (see [`config.toml`](config.toml)). Shared helpers live in [`functions/_shared/`](functions/_shared/common.ts).

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

### `POST /functions/v1/contributions`

For the standalone [Claude Code/Codex collector](../integrations/README.md), not the OMP plugin. Deploy the migrations and function before activating a collector. Create an API key in dashboard Settings, preview explicitly selected source roots locally, then follow the collector's confirmation flow. Do not upload these observations through OMP `ingest` or `history`.

Each request includes `device_id` (stable source-isolated UUID), `harness` (`claude-code` or `codex`) and `snapshot_id` (UUID retained across exact retries). The first begin registers an immutable source cutoff and harness; another cutoff/harness for that device returns `409 cutoff_conflict`.

```jsonc
{ "action": "begin", "device_id": "<uuid>", "harness": "codex", "snapshot_id": "<uuid>",
  "cutoff": "2026-10-27T12:00:00.000Z", "chunk_count": 2 }
// → { "ok": true, "action": "begin", "snapshot_id": "<uuid>" }
{ "action": "chunk", "device_id": "<uuid>", "harness": "codex", "snapshot_id": "<uuid>",
  "index": 0, "observations": [ /* normalized metadata */ ],
  "superseded_canonical_ids": [], "identity_aliases": [] }
// → { "ok": true, "action": "chunk", "snapshot_id": "<uuid>" }
{ "action": "commit", "device_id": "<uuid>", "harness": "codex", "snapshot_id": "<uuid>" }
// → { "ok": true, "action": "commit", "snapshot_id": "<uuid>", "accepted": 123, "committed": true }
```

- **Bounds:** streaming UTF-8 body limit 256 KiB before parsing, at most 1,000 chunks, and 1–500 combined observations/correction entries per chunk. Empty snapshots use `chunk_count: 0` and no chunks. Maximum four pending snapshots per user; abandoned staging expires after seven days on the next begin. Begin/commit receipts and exact chunk digests persist; committed chunk payloads are cleared. Requests share the API-key rate budget.
- **Projection:** all normalized fields are required: `harness`, `canonical_id`, `identity_strength`, `grain`, `timestamp`, `interval_start`, `interval_end`, `provider`, `model`, `session_id`, `parent_session_id`, `is_subagent`, four token fields (`input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`), `call_count`, `cost_usd`, `cost_basis`, `quality`. Local `diagnostics`, paths, cursors and raw source content are forbidden. Unknown keys reject the entire chunk. See [validator](functions/contributions/validation.ts) for exact caps/enums: IDs ≤512 characters, provider ≤64, model/session IDs ≤128, token/call values nullable nonnegative safe integers ≤1e9, cost nullable finite USD ≤10,000. `cost_basis: unknown` iff cost is null. Provider/model unknowns use `"unknown"`.
- **Attribution:** timestamps are valid UTC ISO strings at/after 1970, no more than five minutes in the future. Timestamp-null observations and `session_summary` grains are withheld, not assigned invented days. Reconstructed intervals require both endpoints on the recorded UTC date, ordered endpoints, reconstructed quality and null calls; cutoff-straddling intervals reject at commit. Original `timestamp < cutoff` is history; equality/after is live. Both live and history share the same canonical contribution table, with no seven-day age restriction.
- **Atomicity/retention:** chunks never affect published totals; commit checks every chunk then publishes in one database transaction. Missing chunks, owner conflicts or failed commits leave last-good totals unchanged. Publication upserts accepted identities without resetting/deleting entries absent from the new snapshot, so source cleanup does not remove accepted usage. Exact replay is idempotent, changed chunks return `409 snapshot_conflict`, and an older uncommitted snapshot cannot overwrite a newer committed one (`409 stale_snapshot`). `accepted` counts observations in that snapshot, not newly incurred calls.
- **Ownership:** canonical keys are `(user_id, harness, canonical_id)` and retain their owning source UUID permanently. Another source installation copying the same canonical identity receives `409 ownership_conflict`, never another counted copy. This also applies to superseded identity reservations. Scoped/weak identities cannot prove independently renamed/forked archive equivalence: assign a single source owner to such archives, and do not advertise global exactness from weak IDs.
- **Corrections only:** optional `superseded_canonical_ids` atomically tombstones same-owner reconstructed intervals when newer evidence proves overlap. Retirement reservations persist even if the original interval never reached the server: no fabricated observation is stored, a late same-owner interval stays excluded, and another owner cannot claim the identity. Requests cannot be arbitrarily removed. Optional `identity_aliases: [{from,to}]` upgrades a same-owner request-strength fallback to a response-strength observation in the same snapshot; source IDs stay reserved, aliases cannot chain/cycle or retarget, and late fallback replays never resurrect them. Corrections are permanent evidence-led ledger state, never inferred from missing source files. Aliases/supersession collisions reject the entire commit. Newly discovered conflicts in previously accepted strong-response metadata are surfaced locally; this protocol does not silently retract prior accepted requests.
- **Rate limits:** every contributions request costs 10 of the API key's 600 hits per UTC minute window, shared with OMP `ingest`/`history` (so one minute fits begin plus 59 chunks). A limited request returns `429` with `Retry-After: <seconds>` and the same integer in the body, `{ "error": "rate_limited", "retry_after_seconds": 1..60 }`, computed as the whole seconds until the current window resets. Limited requests still count against that window, and there is no bypass. Clients wait out the window, then retry the same request (same snapshot, same chunk), not chunk zero; staged chunks and receipts persist while waiting. `429 staging_limit` (four pending snapshots) has no `Retry-After` because it clears only when snapshots are committed or expire.
- **Status:** `400` invalid body/observations/incomplete snapshot, `401` invalid/revoked key, `409` ownership/cutoff/snapshot/supersession/alias conflicts or stale snapshot, `413` oversized body, `429` shared rate limit (with `Retry-After`) or staging limit (without), `502` storage failure. Do not drop ownership errors or replace source IDs to bypass them.

### Nullable totals and accounting quality

Unknown is absorbing in daily totals and all leaderboard sums: a missing output/cost/call count is `null`, not zero or a silently partial sum. Quality flags are `has_unknown_tokens`, `has_unknown_cost`, `has_unknown_calls`, `has_estimated_cost`, `has_reconstructed`. Known OMP rows retain their numeric behavior; imported OMP history cost is marked estimated. Collector estimated USD is not an invoice; reconstructed intervals are not fabricated calls. The dashboard displays unknown values without ranking them and marks estimates/reconstruction. The bot preserves null totals and explains the same limitations. OMP activity/presence remain separate: collector publication updates aggregate views, not fake OMP events or heartbeats.

### Parent-run validation and database smoke

No runtime checks are implied by the presence of these tests. From the repository root:

```bash
node --test supabase/test/contributions-validation.test.mjs
# With an installed Deno runtime:
deno check supabase/functions/contributions/index.ts
# Real PostgreSQL/WASM fallback when no server/psql/container is available:
npm install --prefix /tmp/tokenmunchers-db @electric-sql/pglite
PGLITE_MODULE_DIR=/tmp/tokenmunchers-db/node_modules node supabase/test/contributions-smoke.mjs
```

The permanent smoke harness executes every real migration and exercises staged RPC/rollups, replay, interruption, empty cleanup snapshots, committed payload clearing, staging-limit and seven-day staging expiry, partition equality, old post-cutoff recovery, cutoff/harness immutability, copied ownership, nullable output/cost/calls, evidence-led supersession (including before initial delivery), request/response alias upgrades, cross-chunk duplicates, stale snapshots, cutoff-straddling rejection, member/nonmember RLS, bot-role privileges, rate-limit reset timing and key revocation. The rate scenario stages begin plus 59 chunks inside one real minute window. It checks that the limited commit's `retry_after_seconds` equals the seconds remaining in that window, and that in-window retries stay limited and nothing publishes. It then sleeps exactly that long and commits the already-staged chunks. It aligns itself to the clock first, so the smoke takes up to about a minute longer. OMP raw-today/past-rollup/history accounting is seeded alongside collector metadata to check it still counts once. Only Supabase `auth.users`/`auth.uid`, cron scheduler functions and the Realtime publication are stubbed; real pgcrypto and PostgreSQL execute accounting SQL. The harness explicitly clears its synthetic rate-limit windows between independent scenario groups and ages abandoned staging to exercise normal begin-time expiry. PGlite's official [extension documentation](https://pglite.dev/extensions/#pgcrypto) provides `@electric-sql/pglite/contrib/pgcrypto`, and its [API](https://pglite.dev/docs/api) documents `exec`/`query`. Auth-provider integration, actual cron scheduling, Realtime transport, hosted Edge Function bearer requests, multi-connection races and deployed Supabase extension/role permissions still require a real Supabase environment. Fresh installs use all migrations; existing installs apply both `20261028000000_collector_contributions.sql` and `20261028010000_quality_accounting.sql` in order.

#### Scenario-to-regression map

These are available checks, not claims that any gate has run. Source-specific fixture provenance and detailed parser scenarios remain documented in each integration README and fixture manifest.

| Accepted scenario | Behavioral coverage |
| --- | --- |
| Reimport/replay; genuinely new child work versus copied prefixes | `integrations/shared/test/accounting.test.ts`, both source adapter suites; SQL smoke replay/ownership |
| Source cleanup and collector restarts | Shared ledger and delivery suites; SQL smoke empty snapshot and late replay |
| Incomplete line, interrupted bounded scan or process exit | `integrations/collector/test/collector.test.ts`, source reader/adapter suites, shared accounting process-termination case |
| Original timestamp, delayed pre-cutoff flush and old live recovery | Shared partition tests; SQL smoke cutoff equality, since-query and straddling rejection |
| Independent persistent pause and exclusions | Shared accounting pause/restart case, source discovery suites, shared delivery guard cases |
| Unknown output/provider/cost/calls, estimates, subagents and reconstruction | Validator suite; SQL nullable/quality/attribution cases; `dashboard/test/accounting.test.mjs`; `discord-bot/src/leaderboard.test.ts` |
| Legacy/modern transition, counter reset/overflow and compressed archives | `integrations/codex/test/accounting.test.ts` and `reader.test.ts`; SQL permanent interval retirement |
| Historical snapshot spans multiple requests or loses acknowledgments | `integrations/shared/test/delivery.test.ts`; SQL incomplete snapshot, changed chunk, duplicate identity and stale commit cases |
| Large snapshot exceeds one rate window | Validator suite `Retry-After` response contract; SQL smoke 59-chunk burst with reset-timed commit resume; shared delivery rate-wait cases |
| Archive copied to another installation | Shared delivery ownership failure; SQL accepted and never-uploaded-retired identity ownership cases |
| Claude final output and real-session reconciliation | Requires authenticated real Claude evidence; synthetic fixtures cannot establish final output. Real Codex fixture provenance is recorded separately, not inferred from cumulative stdout. |
| UI presentation and deployed transport | Actual dashboard browser smoke with clearly synthetic metadata; hosted authenticated endpoint/database/Realtime gates remain separate from the local SQL harness |



## Project setup

To run your own instance:

1. **Create a Supabase project** and link it (see [Deploying](#deploying)).
2. **Apply the migrations:** `supabase db push`. The pg_cron and pgcrypto extensions are enabled by the first migration.
3. **Deploy the Edge Functions:** `supabase functions deploy ingest history contributions`.
4. **Enable OAuth providers.** Authentication → Sign In / Providers → GitHub and/or Discord. Each needs an OAuth app on that platform, with the callback URL shown in Supabase. Turn **off** the Email provider; it isn't used.
5. **Set redirect URLs.** Authentication → URL Configuration: set Site URL to the dashboard URL. Under Redirect URLs, add the production URL and `http://localhost:3000/**`.
6. **Point the dashboard and plugin at the project.** Use the dashboard's env vars (see the [dashboard README](../dashboard/README.md)). For the plugin, set `TOKENMUNCHERS_URL`, or change `DEFAULT_INGEST_URL` in [`plugin/src/config.ts`](../plugin/src/config.ts).

### Inviting members

In the SQL editor:

```sql
insert into public.allowlist (email) values ('friend@example.com');
```

Use the email of the person's GitHub or Discord account. It must be lowercase.

### Provisioning the Discord bot database user

The Discord bot connects directly to PostgreSQL and reads aggregated leaderboard totals. It is intentionally sandboxed to the minimum necessary database privileges:

1. **Role and least privilege:**
   Migration `20261026000000_discord_leaderboard.sql` creates the `tokenmunchers_discord_reader` role as `NOLOGIN`. It is granted `USAGE` on schema `public` and `EXECUTE` on `public.discord_leaderboard(text)`.
   - The role has **no** `SELECT`, `INSERT`, `UPDATE`, or `DELETE` grants on any tables (`usage_events`, `usage_daily`, `profiles`, `api_keys`, `allowlist`).
   - The wrapper function `public.discord_leaderboard(p_period)` runs as `SECURITY DEFINER` with `SET search_path = ''`. Its execution is revoked from `PUBLIC`, `anon`, and `authenticated`.
   - The wrapper only exposes aggregated totals (`handle`, `display_name`, `total_tokens`, `subagent_tokens`, `cost_usd`) and strictly validates `p_period` (`today` for UTC day start, `week` for rolling 7 days, or `all` for all-time totals).

2. **Create a dedicated LOGIN role:**
   In the Supabase SQL Editor, provision a separate login user that inherits the reader role:

   ```sql
   CREATE ROLE tokenmunchers_bot WITH LOGIN PASSWORD 'strong-random-password' IN ROLE tokenmunchers_discord_reader;
   ```

   **Never** grant this user `SUPERUSER`, `BYPASSRLS`, `CREATEDB`, or `CREATEROLE`. Never use the Supabase `service_role` key or postgres administrative credentials for the Discord bot.

3. **Construct the connection string:**
   ```
   postgresql://tokenmunchers_bot:<escaped_password>@<db_host>:5432/postgres?sslmode=verify-full
   ```
   - **Password escaping:** If the generated password contains special characters (such as `@`, `:`, `/`, `?`, or `#`), URL-encode them (e.g. `@` becomes `%40`).
   - **Verified TLS:** Always require `sslmode=verify-full`. Do not omit `sslmode` or use `sslmode=require`, as omitting it allows plaintext and `require` may skip certificate verification. While `sslmode=verify-full` is standard in the connection URL, explicit driver TLS options verify the certificate chain and hostname in code.
   - **Direct connection only:** Connect directly to PostgreSQL on port 5432 using username `tokenmunchers_bot`. Do not use Supabase transaction pooling (port 6543) because the bot client enables prepared statements (`prepare: true`), which transaction pooling mode does not support.

4. **TLS Certificate Authority & Trust:**
   Direct connections to Supabase PostgreSQL instances use certificates issued by Supabase's production root Certificate Authority (`Subject: Supabase Root 2021 CA`, expires `2031-04-26 10:56:53 UTC`). Because Node.js does not bundle this CA in its default trust store, direct connections require trusted CA configuration to avoid `SELF_SIGNED_CERT_IN_CHAIN` errors.
   - **Automated bot trust:** The Discord bot automatically loads the official bundled Supabase production root CA (`discord-bot/certs/supabase-prod-ca.crt`) alongside Node's standard root certificates, maintaining strict TLS certificate chain validation and hostname verification (`rejectUnauthorized: true`).
   - **Official sources:**
     - Root CA download: [https://supabase-downloads.s3-ap-southeast-1.amazonaws.com/prod/ssl/prod-ca-2021.crt](https://supabase-downloads.s3-ap-southeast-1.amazonaws.com/prod/ssl/prod-ca-2021.crt)
     - Supabase Studio source references: [custom-content.json#L63](https://github.com/supabase/supabase/blob/master/apps/studio/hooks/custom-content/custom-content.json#L63) and [SSLConfiguration.tsx#L114-L121](https://github.com/supabase/supabase/blob/master/apps/studio/components/interfaces/Settings/Database/SSLConfiguration.tsx#L114-L121)
   - **Operator maintenance & rotation:** The bundled root CA certificate is valid through **2031-04-26**. On rotation or before expiration, replace `discord-bot/certs/supabase-prod-ca.crt` strictly from the official Supabase source and restart the bot process.
   - **Security rules:** Never disable TLS verification (`rejectUnauthorized: false`), never use `NODE_TLS_REJECT_UNAUTHORIZED=0`, and do not configure `NODE_EXTRA_CA_CERTS` inside `.env` (Node `--env-file` cannot evaluate it before internal TLS startup).

## Deploying

```bash
# One-time: install the CLI (or prefix every command with `npx supabase@latest`)
brew install supabase/tap/supabase

supabase login
supabase link --project-ref fywgkzqgtitpwijweojk   # asks for the database password

supabase db push --dry-run     # check what will be applied
supabase db push
supabase functions deploy ingest history contributions
```

Deploy server changes **before** publishing a plugin version that depends on them.

If `supabase migration list` shows versions that don't match the local file names (for example, migrations applied by hand in the SQL editor), fix the history table with `supabase migration repair --status applied|reverted <version>` before pushing. This only edits the history table and never runs SQL.

## Design notes

- **Ranking:** by tokens by default, with a Cost toggle. Subagent calls count by default, with a toggle to exclude them.
- **Presence** is the `live_sessions` table pushed over Realtime, rather than a Realtime presence channel. An Edge Function can't hold a presence connection open, and the table survives page reloads.
- **Rollups** run every 10 minutes instead of nightly. "Today" and "All time" combine daily totals with exact raw counts, so totals never lag behind the feed.
- **Cost** is what OMP reports for live events. Imported history uses OMP's stats estimates, which price subscription usage at public API rates.
