# tokenmunchers dashboard

The web front end for [tokenmunchers](../README.md), live at **[tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com)**: a real-time leaderboard, activity feed, profiles and account settings. Built with Next.js 16 (App Router) and Supabase. Styled as Windows 95.

## Pages

| Route | Description |
| --- | --- |
| `/` | Leaderboard (Today / This week / All time; tokens or cost; with or without subagent calls), "Live now", the activity feed, and group tokens per day |
| `/u/[handle]` | Member profile: totals, streaks, biggest day, tokens per day, model mix, recent calls |
| `/settings` | Profile, API keys, setup instructions for OMP and munch, account deletion |
| `/login` | GitHub / Discord sign-in |
| `/auth/callback`, `/auth/signout` | Supabase auth handlers |

Signed-in users who aren't on the allowlist see a "not a member" page.

## Data sources

All reads run as the signed-in user, with row-level security (RLS) applied. The dashboard never writes usage data.

| Data | Source |
| --- | --- |
| Today / this week totals | `leaderboard_since(timestamptz)`, over raw call events plus earlier Claude Code/Codex contributions |
| All-time totals | `leaderboard_all_time()`: imported history, past daily totals and today's raw events, plus earlier Claude Code/Codex contributions once |
| Charts, streaks, model mix | `usage_daily_all` view (daily totals + imported history + earlier Claude Code/Codex contributions) |
| Activity feed, recent calls | `usage_events` |
| Live now | `live_sessions` (a heartbeat in the last 90 s) |
| Live updates | Supabase Realtime for call events and presence; an aggregate refresh every 30 seconds |

The database is documented in the [Supabase README](../supabase/README.md).

## Claude Code and Codex

Claude Code and Codex are reported live by [munch](../proxy/README.md), the same way as other tools, so their calls appear in the activity feed and under Live now. Only calls routed through the munch proxy are captured — anything on the machine that doesn't go through it isn't seen. Settings has the setup steps, and the [proxy README](../proxy/README.md) documents munch itself. Importing Claude Code/Codex history from disk isn't available at the moment.

Usage that the former Claude Code/Codex collector already uploaded stays counted in the leaderboards and charts, and nothing new arrives through that path. That earlier work remains visible because the `collector_contributions` tables and the views over them are retained in Postgres; reading them doesn't depend on the `contributions` Edge Function being deployed. Deploy both the collector migrations and the `contributions` Edge Function with the rest of the backend to support future uploads; see the [backend notes](../supabase/README.md#post-functionsv1contributions).

Unavailable output, calls or USD are shown as **Unknown**, never zero, and unavailable leaderboard metrics are unranked. Null is absorbing in group totals, daily charts and model mix. Unknown chart days have `?` markers (and explicit table values); muted sparkline bars indicate unavailable days, not measured usage. Cost estimates and reconstructed usage are labeled; neither reconstructed intervals nor session summaries become invented calls. A day with recorded usage can establish activity even when its total is unavailable; “Biggest day” remains unknown if any daily total is unavailable.

Earlier contributions don't create live presence or activity-feed entries. Strong canonical IDs are owned once per user/harness across devices, and previously accepted contributions survive source cleanup. Source logs were not complete billing ledgers.


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
| `app/icon.svg` | Browser tab icon (pixel-art muncher); Next.js emits its `<link rel="icon">` automatically |
| `components/` | Leaderboard, charts, live updates and the Windows 95 chrome |
| `lib/stats.ts` | Total and streak calculations, UTC day helpers |
| `lib/supabase/` | Browser, server and Realtime clients, and the session proxy |

Nullable aggregation and display checks are in `test/accounting.test.mjs`; run `node --test dashboard/test/accounting.test.mjs` from the repository root, plus `npm --prefix dashboard run typecheck` and `npm --prefix dashboard run build`. These commands are release gates, not evidence that they have already run.
