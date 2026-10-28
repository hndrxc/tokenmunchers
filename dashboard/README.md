# tokenmunchers dashboard

The web front end for [tokenmunchers](../README.md), live at **[tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com)**: a real-time leaderboard, activity feed, profiles and account settings. Built with Next.js 16 (App Router) and Supabase. Styled as Windows 95.

## Pages

| Route | Description |
| --- | --- |
| `/` | Leaderboard (Today / This week / All time; tokens or cost; with or without subagent calls), "Live now", the activity feed, and group tokens per day |
| `/u/[handle]` | Member profile: totals, streaks, biggest day, tokens per day, model mix, recent calls |
| `/settings` | Profile, API keys, plugin setup instructions, account deletion |
| `/login` | GitHub / Discord sign-in |
| `/auth/callback`, `/auth/signout` | Supabase auth handlers |

Signed-in users who aren't on the allowlist see a "not a member" page.

## Data sources

All reads run as the signed-in user, with row-level security (RLS) applied. The dashboard never writes usage data.

| Data | Source |
| --- | --- |
| Today / this week totals | `leaderboard_since(timestamptz)`, over OMP raw events plus canonical collector contributions |
| All-time totals | `leaderboard_all_time()`: OMP imported history, past daily totals and today's raw events, plus collector contributions once |
| Charts, streaks, model mix | `usage_daily_all` view (OMP daily totals + imported history + published collector contributions) |
| Activity feed, recent calls | `usage_events` |
| Live now | `live_sessions` (a heartbeat in the last 90 s) |
| Live updates | Supabase Realtime for OMP events/presence; aggregate refresh every 30 seconds also picks up atomic collector publications |

The database is documented in the [Supabase README](../supabase/README.md).

## Claude Code and Codex onboarding

Deploy both collector migrations and the `contributions` Edge Function first; see the [backend protocol and deployment instructions](../supabase/README.md#post-functionsv1contributions). In Settings, create a source-labeled API key. Follow the [standalone collector README](../integrations/README.md) to select only intended source roots, preview accounting coverage/exclusions, confirm historical publication and activate incremental collection. OMP keeps its existing `/munch` commands and identities; it does not ingest Claude/Codex observations.

Unavailable output, calls or USD are shown as **Unknown**, never zero, and unavailable leaderboard metrics are unranked. Null is absorbing in group totals, daily charts and model mix. Unknown chart days have `?` markers (and explicit table values); muted sparkline bars indicate unavailable days, not measured usage. Cost estimates and reconstructed usage are labeled; neither reconstructed intervals nor session summaries become invented calls. A day with recorded usage can establish activity even when its total is unavailable; “Biggest day” remains unknown if any daily total is unavailable.

Collector totals do not manufacture live presence or OMP activity-feed entries. Those sections explicitly describe OMP calls. Strong canonical IDs are owned once per user/harness across devices; copied archives with conflicting owners require resolution rather than changing IDs. Weak archive identities require a single source owner. Previously accepted contributions survive source cleanup and interrupted/replayed snapshot publication. Source logs are not complete billing ledgers; the collector preview reports skipped/uncertain coverage.

Totals change only when a snapshot commits. A large first import shares the API key's per-minute request budget with the OMP plugin, so the collector waits for the server's `Retry-After` reset and resumes where it stopped. Expect such an import to take several minutes before the dashboard shows it, all at once.


## Local development

```bash
cp .env.example .env.local
npm install
npm run dev        # http://localhost:3000
npm run typecheck
```

| Variable | Description |
| --- | --- |
| `NEXT_PUBLIC_SUPABASE_URL` | Supabase project URL |
| `NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY` | Publishable (anon) key. It's safe to expose: RLS protects the data. |

For sign-in to work locally, `http://localhost:3000/**` must be in the Supabase project's redirect URLs (see [Supabase setup](../supabase/README.md#project-setup)).

## Deploying

The dashboard is a standard Next.js app. To deploy on Vercel:

1. Import the repository and set the **root directory** to `dashboard`.
2. Add the two environment variables above.
3. Add the production URL to the Supabase project's Site URL and Redirect URLs.

Every push to `main` redeploys.

## Structure

| Path | Contents |
| --- | --- |
| `app/` | Routes, layouts and server components |
| `components/` | Leaderboard, charts, live updates and the Windows 95 chrome |
| `lib/stats.ts` | Total and streak calculations, UTC day helpers |
| `lib/supabase/` | Browser, server and Realtime clients, and the session proxy |

Nullable aggregation and display checks are in `test/accounting.test.mjs`; run `node --test dashboard/test/accounting.test.mjs` from the repository root, plus `npm --prefix dashboard run typecheck` and `npm --prefix dashboard run build`. These commands are release gates, not evidence that they have already run.
