# tokenmunchers

A live leaderboard of AI token usage for the crew. An Oh My Pi (OMP) plugin reports every model call's token counts to Supabase, and a Next.js dashboard ranks everyone in real time.

## Privacy: exactly what leaves your machine

Each model call sends one event with these fields and nothing else:

| Field | Example |
| --- | --- |
| `event_id` | random UUID (makes retries idempotent) |
| `session_id` | OMP session id |
| `ts` | when the call finished |
| `provider`, `model` | `anthropic`, `claude-opus-5-5` |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | integers |
| `cost_usd` | as reported by OMP (0 on subscriptions) |
| `is_subagent` | true for subagent / swarm calls |
| `client_version` | plugin version |

Presence events (`session_start`, `heartbeat`, `session_end`) carry only the session id, provider, model and start time.

**Never sent:** prompts, responses, code, file paths, repo names, tool calls or tool output, working directory. The ingest function validates every request against this allowlist and rejects the whole request if any other field appears. `/munch pause` stops reporting for a session. Deleting your account from Settings removes every event you ever sent.

## How it works

```
OMP plugin ──POST /functions/v1/ingest──▶ Edge Function ──rpc ingest()──▶ Postgres
 (per model call, fire-and-forget,          (validate, hash key,             │
  JSONL queue + retry on failure)            rate limit 600/min/key)         │ Realtime (postgres_changes)
                                                                             ▼
                                                                  Next.js dashboard (websocket)
```

Measured on the live project: ingest round trip ~0.2–0.3 s warm, model call end → dashboard render 0.45–1.0 s.

| Piece | Where |
| --- | --- |
| OMP plugin (`omp-tokenmunchers`) | [`plugin/`](plugin/) |
| Ingest Edge Function | [`supabase/functions/ingest/`](supabase/functions/ingest/index.ts) |
| Schema, RLS, rollups, cron | [`supabase/migrations/`](supabase/migrations/) |
| Dashboard (Next.js 16, App Router) | [`dashboard/`](dashboard/) |

Supabase project: `tokenmunchers` (`fywgkzqgtitpwijweojk`, us-east-2).

### Data model

- `profiles`, created by a trigger on sign-up **only if the email is in `allowlist`** (invite-only).
- `api_keys` store only a SHA-256 hash and a display prefix; the plaintext `tm_…` key is shown once.
- `usage_events` hold raw per-call events, kept 30 days, and feed Realtime.
- `usage_daily` holds per user/day/provider/model/subagent rollups, kept forever. pg_cron re-rolls the last 8 days every 10 minutes; ingest rejects events older than 7 days, so late queued events are always captured before raw retention deletes them.
- `live_sessions` is presence. A user is live if a heartbeat arrived in the last 90 s.

RLS: signed-in members can read everything except other people's keys. No client role can write usage data or call `ingest`; only the Edge Function (secret key) can.

## Setup checklist

1. **Allowlist friends.** In the Supabase SQL editor:
   ```sql
   insert into public.allowlist (email) values ('friend@example.com');
   ```
   Use the email on their GitHub/Discord account.
2. **Enable OAuth providers.** Supabase Dashboard → Authentication → Sign In / Providers → GitHub and/or Discord (needs an OAuth app on each). Set the callback URL shown there in the GitHub/Discord app. Then turn **off** the Email provider, which isn't used.
3. **Set redirect URLs.** Authentication → URL Configuration: set Site URL to the dashboard URL and add `http://localhost:3000/**` plus your production URL under Redirect URLs.
4. **Deploy the dashboard.** Vercel → import this repo, root directory `dashboard`, env vars from [`dashboard/.env.example`](dashboard/.env.example).
5. **Publish the plugin.** `cd plugin && npm publish`, then everyone runs `omp plugin install omp-tokenmunchers` and `/munch login`.

## Development

```bash
cd plugin && npm install && npm test && npm run typecheck
cd dashboard && cp .env.example .env.local && npm install && npm run dev
```

The Edge Function deploys with `supabase functions deploy ingest` (it authenticates plugin keys itself, so `verify_jwt = false` in [`supabase/config.toml`](supabase/config.toml)).

## Decisions on the design doc's open questions

- **Rank by tokens or cost?** Tokens by default, with a Cost toggle. Cost is as reported by OMP, and subscription plans report $0.
- **Subagent calls?** Counted in the total by default, with the subagent share shown per person and a "Main only" toggle.
- **Hosting?** Either works. The dashboard is a standard Next.js app, so Vercel is the least effort, and a `hndrxc.com` subdomain can point at it.
- **Sign-in?** Both GitHub and Discord buttons are wired; enable whichever providers you configure.

## Differences from the design doc

- **Presence** is a `live_sessions` table pushed over Realtime instead of a Realtime presence channel, because an Edge Function can't hold a presence connection open, and the table survives page reloads.
- **Rollups** run every 10 minutes (idempotent upsert over the last 8 days) instead of nightly. "Today" and "All time" combine rollups with exact raw totals, so nothing on the dashboard lags the feed.
- **API-equivalent cost** using a price table is not built yet; cost is whatever OMP reports.
