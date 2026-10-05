# omp-tokenmunchers

Oh My Pi plugin that reports per-call token usage to your crew's tokenmunchers leaderboard.

**Only metadata is sent:** provider, model, input/output/cache token counts, cost, timestamps, session id and a subagent flag. Never prompts, responses, code, file paths, repo names or tool output. The server rejects any request containing other fields.

## Install

```bash
omp plugin install omp-tokenmunchers
```

Then create a key on the dashboard's Settings page and run, inside OMP:

```
/usage-login tm_…
```

## Commands

| Command | What it does |
| --- | --- |
| `/usage-login [key]` | Saves your API key (prompts if omitted) and checks it against the server |
| `/usage-pause` | Stops reporting for this process, e.g. for private or client work |
| `/usage-resume` | Resumes after a pause |
| `/usage-status` | Shows key prefix, paused state, queued events and last error |

## Behavior

- Hooks `message_end` and sends one event per finished assistant message, immediately (no batching).
- Sends `session_start` on session start, a heartbeat every 30 s while a turn runs, and `session_end` on shutdown, so the dashboard's "Live now" strip stays accurate.
- Subagent calls (`ctx.agent.kind === "sub"`) are reported with `is_subagent: true`.
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
