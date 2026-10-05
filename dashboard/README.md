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
| Today / this week totals | `leaderboard_since(timestamptz)`, over raw events |
| All-time totals | `leaderboard_all_time()`: imported history, plus daily totals for past days, plus raw events for today |
| Charts, streaks, model mix | `usage_daily_all` view (live daily totals + imported history) |
| Activity feed, recent calls | `usage_events` |
| Live now | `live_sessions` (a heartbeat in the last 90 s) |
| Live updates | Supabase Realtime `INSERT`s on `usage_events` and changes to `live_sessions` |

The database is documented in the [Supabase README](../supabase/README.md).

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
