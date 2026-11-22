# tokenmunchers dashboard

The web front end for [tokenmunchers](../README.md), live at **[tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com)**: a real-time leaderboard, activity feed, profiles and account settings. Built with Next.js 16 (App Router) and Supabase. Styled as Windows 95.

## Pages

| Route | Description |
| --- | --- |
| `/` | Leaderboard (Today / Week / All time; tokens or cost; with or without subagent calls) with a per-member token sparkline for the selected period, "Live now", the activity feed, and group tokens per day |
| `/u/[handle]` | Member profile: totals, streaks, biggest day, tokens per day, model mix, recent calls |
| `/settings` | Profile, API keys, setup instructions for OMP and munch, account deletion |
| `/login` | GitHub / Discord sign-in |
| `/auth/callback`, `/auth/signout` | Supabase auth handlers |

Signed-in users who aren't on the allowlist see a "not a member" page.

## Data sources

All reads run as the signed-in user, with row-level security (RLS) applied. The dashboard never writes usage data.

| Data | Source |
| --- | --- |
| Today / this week totals | `leaderboard_since(timestamptz)`, over raw call events plus published Claude Code/Codex contributions |
| All-time totals | `leaderboard_all_time()`: imported history, past daily totals and today's raw events, plus published Claude Code/Codex contributions once |
| Charts, streaks, model mix | `usage_daily_all` view (daily totals + imported history + published Claude Code/Codex contributions) |
| Activity feed, recent calls | `recent_activity`, over ingest events and eligible committed contributions |
| Live now | `live_sessions` (a heartbeat in the last 90 s) |
| Live updates | Supabase Realtime for ingest call events and presence; Activity contributions and totals refresh every 30 seconds |

The database is documented in the [Supabase README](../supabase/README.md).

The leaderboard's period picker selects what each column shows and also drives every member's sparkline: **Today** is the current UTC day, **Week** is the latest seven UTC dates including today, and **All time** spans all available history. The leaderboard total for the selected period uses the same cutoff as its sparkline — for example, the Week total covers the same seven UTC days. The separate group tokens-per-day chart always shows the last 30 UTC days, independent of the selected leaderboard period.

## Claude Code and Codex

[Munch](../proxy/README.md) reports supported proxy-routed calls through ingest. The opt-in desktop pilot reports eligible first-party local Claude Code and Codex requests through the existing contributions endpoint instead. Once committed, live exact requests appear in **Activity** and member recent calls with **Claude Code** or **Codex** badges. The badges identify the harness, not necessarily the desktop app. Contributions arrive on the next poll (roughly every 30 seconds) and don't create **Live now** presence; ingest updates still arrive over Realtime.

**Activity** on the home page and **Recent calls** on member profiles share the same per-browser tag settings. **Claude Code** and **Codex** are shown by default; the **Standard** badge for ingest calls is hidden by default. Use the tag controls in either section to show, hide, or rename each source badge, or reset to the defaults. These settings affect only badge display, not which calls appear, subagent badges, or usage reporting.

Activity reads the existing ledger and does not show historical, reconstructed, scoped-identity or superseded contributions. Both sources share the ingest feed's UTC retention horizon and are capped together at the newest 50 calls on the home page or newest 25 for a member. Previously published usage still contributes to totals where eligible, even after it ages out of Activity. Strong canonical IDs are owned once per user/harness across devices, and accepted contributions survive source cleanup. Source logs are not complete billing ledgers.

For mixed usage, the dashboard shows known spend alongside unpriced token coverage. When there is usage but none of it is priced, spend is **Unknown**, not $0; an actual reported zero remains zero. The dashboard labels estimates separately and indicates when the unpriced token count itself is unknown.

Unavailable token and call counts are shown as **Unknown**, never zero, and unavailable leaderboard metrics are unranked. Unknown token counts propagate in group totals, daily charts and model mix. Unknown chart days have `?` markers (and explicit table values); muted sparkline bars indicate unavailable days, not measured usage. Reconstructed usage is labeled, not turned into invented calls. A day with recorded usage can establish activity even when its total is unavailable; “Biggest day” remains unknown if any daily total is unavailable.


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

Apply the [Supabase migrations](../supabase/README.md#deploying) before deploying this dashboard version: it expects the Activity RPC and spend coverage fields. This change does not include a production deployment.

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
