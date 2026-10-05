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

0.2.0 replaced the old `/usage-login`, `/usage-pause`, `/usage-resume` and `/usage-status` commands.

## Behavior

- Hooks `message_end` and sends one event per finished assistant message, immediately (no batching).
- Sends `session_start` on session start, a heartbeat every 30 s while a turn runs, and `session_end` on shutdown, so the dashboard's "Live now" strip stays accurate.
- Subagent calls (`ctx.agent.kind === "sub"`) are reported with `is_subagent: true`.
- Event ids are deterministic: a UUIDv5 of the session transcript's file name, the call's timestamp, provider and model. A call reported twice (a retry, or a future backfill from OMP's local `stats.db`) is stored once. Only the file name feeds the hash, never its directory, which encodes the working directory.
- `/munch pause` records the paused time range per session in `<agent dir>/tokenmunchers/pauses.json`. It never leaves the machine; it exists so a backfill can skip calls made while paused.
- All network calls are fire-and-forget with a 3 s timeout, so the agent is never blocked.
- Failed usage events go to `<agent dir>/tokenmunchers/queue.jsonl` and retry with backoff (1 min doubling to 15 min), or immediately after the next successful send. Events older than ~7 days are dropped.

## Config

Stored at `<agent dir>/tokenmunchers/config.json` (mode 600). Environment variables override it:

| Variable | Purpose |
| --- | --- |
| `TOKENMUNCHERS_KEY` | API key |
| `TOKENMUNCHERS_URL` | Ingest URL (defaults to the crew's Supabase project) |

## Development

```bash
npm install
npm test          # adapter + retry queue tests
npm run typecheck # against @oh-my-pi/pi-coding-agent 18.6.1 types
```

All OMP-specific parsing lives in [`src/adapter.ts`](src/adapter.ts); if an OMP release changes its message or usage shapes, that file is the one to update.
