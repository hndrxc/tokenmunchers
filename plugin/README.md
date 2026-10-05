# omp-tokenmunchers

Oh My Pi plugin that reports per-call token usage to your crew's tokenmunchers leaderboard.

**Only metadata is sent:** provider, model, input/output/cache token counts, cost, timestamps, session id and a subagent flag. Never prompts, responses, code, file paths, repo names or tool output. The server rejects any request containing other fields.

## Install

```bash
omp plugin install omp-tokenmunchers
```

Then create a key on the dashboard's Settings page and run, inside OMP:

```
/munch login tm_…
```

## Commands

Everything lives under one command, `/munch`, with tab completion for its options:

| Command | What it does |
| --- | --- |
| `/munch` or `/munch status` | Shows key prefix, paused state, queued events and last error |
| `/munch login [key]` | Saves your API key (prompts if omitted) and checks it against the server |
| `/munch pause` | Stops reporting for this process, e.g. for private or client work |
| `/munch resume` | Resumes after a pause |
| `/munch sync` | Refreshes OMP's stats (`omp stats`) and resends recent calls the live reporter missed |
| `/munch backfill [--exclude a,b] [--yes]` | Previews, then uploads daily totals of your usage from before this machine went live |

0.2.0 replaced the old `/usage-login`, `/usage-pause`, `/usage-resume` and `/usage-status` commands.

## Behavior

- Hooks `message_end` and sends one event per finished assistant message, immediately (no batching).
- Sends `session_start` on session start, a heartbeat every 30 s while a turn runs, and `session_end` on shutdown, so the dashboard's "Live now" strip stays accurate.
- Subagent calls (`ctx.agent.kind === "sub"`) are reported with `is_subagent: true`.
- Event ids are deterministic: a UUIDv5 of the session transcript's file name, the call's timestamp, provider and model. A call reported twice (a retry, or a resend from OMP's local `stats.db`) is stored once. Only the file name feeds the hash, never its directory, which encodes the working directory.
- `/munch pause` records the paused time range per session in `<agent dir>/tokenmunchers/pauses.json`. It never leaves the machine; gap fill and backfill skip calls inside those ranges.

## Filling gaps from OMP's stats

OMP indexes every session into a local SQLite database (`~/.omp/stats.db`, kept current by `omp stats` and `/usage`). The plugin reads it read-only, and only these columns: session file name, project label (for local filtering only), timestamp, duration, provider, model, token counts, cost and agent type. It never reads error messages, prompts or tool data, and never uploads file paths or project names. This needs OMP's Bun runtime.

`<agent dir>/tokenmunchers/state.json` tracks this install: a random device id, **live since** (when this machine first delivered a call), and how far the gap fill has scanned.

- **Gap fill** (automatic every minute, or `/munch sync`): resends calls from the last ~6.5 days made after "live since", e.g. ones lost to a crash. They carry the same event ids as the live path, so the server ignores calls it already has. Up to 400 per minute, and only one OMP process on the machine does it at a time, to stay inside the ingest rate limit.
- **Backfill** (`/munch backfill`, one-off): sums every call before "live since" into UTC daily totals per model and uploads them. You see a preview first (calls, tokens, date range, projects), and `--exclude` drops projects whose label contains any of the given text. Each run replaces this machine's previous upload, so re-running is safe. Queued events must be delivered first.
- Installs upgraded from 0.1.0 ask the server when your account's first live event was, since 0.1.0 didn't record it. With several machines on 0.1.0, the account's earliest event is used for each of them, so a machine's usage between that date and its own first live event isn't imported.
- Calls the 0.1.0 plugin may have missed are not gap-filled, because there is no pause record for that period.
- All network calls are fire-and-forget with a 3 s timeout, so the agent is never blocked.
- Failed usage events go to `<agent dir>/tokenmunchers/queue.jsonl` and retry with backoff (1 min doubling to 15 min), or immediately after the next successful send. Events older than ~7 days are dropped.

## Config

Stored at `<agent dir>/tokenmunchers/config.json` (mode 600). Environment variables override it:

| Variable | Purpose |
| --- | --- |
| `TOKENMUNCHERS_KEY` | API key |
| `TOKENMUNCHERS_URL` | Ingest URL (defaults to the crew's Supabase project); the history URL is derived from it |
| `TOKENMUNCHERS_STATS_DB` | Path to OMP's `stats.db`, if it isn't found automatically |

## Development

```bash
npm install
npm test          # adapter, retry queue, pause log, gap fill and history tests
npm run typecheck # against @oh-my-pi/pi-coding-agent 18.6.1 types
```

All OMP-specific parsing lives in [`src/adapter.ts`](src/adapter.ts); if an OMP release changes its message or usage shapes, that file is the one to update.
