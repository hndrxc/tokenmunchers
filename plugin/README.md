# omp-tokenmunchers

The [Oh My Pi](https://github.com/can1357/oh-my-pi) plugin that reports per-call token usage to a [tokenmunchers](../README.md) leaderboard.

**Requires** OMP running on Bun **1.3.14 or newer**. New users should follow the [setup guide](../README.md#getting-started).

## Install

```bash
omp plugin install omp-tokenmunchers
```

Create an API key on the dashboard's Settings page, then run this inside OMP:

```
/munch login tm_…
```

## Commands

Everything is under one command, `/munch`, with tab completion:

| Command | Description |
| --- | --- |
| `/munch`, `/munch status` | Key prefix, reporting state, queued events, last error, "live since" time and gap-fill progress |
| `/munch login [key]` | Saves the API key (asks for it if omitted) and checks it against the server |
| `/munch pause` | Stops reporting for this OMP process |
| `/munch resume` | Resumes after a pause |
| `/munch sync` | Refreshes OMP's stats database (`omp stats`) and re-sends recent calls that didn't get through |
| `/munch backfill [--exclude a,b] [--yes]` | Shows a preview, then uploads daily totals of usage from before this machine started reporting |

`--exclude` skips projects whose name contains any of the comma-separated text. `--yes` skips the confirmation dialog; it's required when OMP runs without a UI.

> 0.2.0 replaced `/usage-login`, `/usage-pause`, `/usage-resume` and `/usage-status` with `/munch`.

## Data sent

### Per call (`ingest`)

| Field | Description |
| --- | --- |
| `event_id` | UUIDv5 computed from the call (see [Event identity](#event-identity)) |
| `session_id` | OMP session id |
| `ts` | When the call finished (start + duration) |
| `provider`, `model` | e.g. `anthropic`, `claude-opus-5-5` |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | Integers |
| `cost_usd` | Cost as OMP reports it (0 on subscription plans) |
| `is_subagent` | `true` for subagent and swarm calls |
| `client_version` | Plugin version |

Presence events (`session_start`, `heartbeat`, `session_end`) send only the session id, provider, model and session start time.

### History import (`history`)

`/munch backfill` sends one row per UTC day, provider, model and subagent flag, containing the call count, the four token counts and cost. It also sends a random device id for this install.

Costs in imported history come from OMP's stats database. That database estimates subscription usage at public API prices, so imported days can show a cost where live events report $0.

### Never sent

Prompts, responses, code, file paths, repository or project names, tool calls or their output, and the working directory. Both Edge Functions check every request against a fixed list of allowed fields and reject the whole request if it contains anything else.

## Behavior

### Live reporting

- Hooks `message_end` and sends one event per finished assistant message, right away (no batching).
- Sends `session_start` on start, a heartbeat every 30 s while a turn runs, and `session_end` on shutdown. These drive the dashboard's "Live now" strip.
- Subagent calls (`ctx.agent.kind === "sub"`) are sent with `is_subagent: true`.
- Network calls never block the agent: the plugin doesn't wait for them, and each times out after 3 s.
- Failed usage events are written to `queue.jsonl` and retried with backoff (1 min, doubling up to 15 min), or immediately after the next successful send. Queued events older than ~7 days are dropped, because the server rejects them.

### Event identity

Each event id is a UUIDv5 of the session transcript's file name, the call's start timestamp, the provider and the model. The same call always gets the same id, whether it's sent live, retried, or re-sent from OMP's stats database. The server ignores ids it already has.

Only the transcript's *file name* goes into the hash, never its directory, because the directory name encodes the working directory. Subagent transcripts are keyed under their parent session.

### Pausing

`/munch pause` records the paused time range for each session in `pauses.json`. This file never leaves the machine. Gap fill and backfill skip every call inside a paused range, including calls from subagents of a paused session.

## Gap fill and backfill

OMP indexes every session into a local SQLite database, `stats.db`. `omp stats` and `/usage` keep it up to date. The plugin opens it **read-only** and reads only these columns: `id`, `session_file`, `folder`, `timestamp`, `duration`, `provider`, `model`, the four token counts, `cost_total` and `agent_type`. `folder` is used only for the backfill preview and `--exclude`, and is never uploaded. Error messages, prompts and tool data are never read.

`state.json` records for this install: a random device id, **live since** (the time of the earliest call this machine delivered live), and how far the gap fill has scanned. Calls before "live since" belong to the history import; calls from then on are reported live. So the two never overlap.

| | Gap fill | Backfill |
| --- | --- | --- |
| Trigger | Automatic, every minute; or `/munch sync` | `/munch backfill` |
| Covers | Calls after "live since" from the last ~6.5 days | Every call before "live since" |
| Sends | Normal usage events with the same ids, which the server ignores if it already has them | Daily totals, which replace this device's previous upload |
| Limits | Up to 400 calls per minute, one OMP process per machine at a time | Waits until queued live events are delivered |

**Upgrading from 0.1.0.** 0.1.0 didn't record when a machine started reporting live. So an upgraded install asks the server for the account's first live event and uses that as "live since". If several machines ran 0.1.0, they all get the account's earliest date. Any machine's usage between that date and its own first live event isn't imported. Gap fill also skips the 0.1.0 period, since there are no pause records for it.

## Configuration

Config is stored in `<agent dir>/tokenmunchers/config.json` (file mode 600). Environment variables override the file:

| Variable | Description |
| --- | --- |
| `TOKENMUNCHERS_KEY` | API key |
| `TOKENMUNCHERS_URL` | Ingest URL (defaults to the group's Supabase project). The history URL is derived from it. |
| `TOKENMUNCHERS_STATS_DB` | Path to OMP's `stats.db`, if it isn't found automatically |

### Files

All in `<agent dir>/tokenmunchers/` (usually `~/.omp/agent/tokenmunchers/`). None of them is ever uploaded.

| File | Purpose |
| --- | --- |
| `config.json` | API key and endpoint |
| `state.json` | Device id, "live since" time, gap-fill progress |
| `pauses.json` | Paused time ranges per session |
| `queue.jsonl` | Usage events waiting to be retried |
| `gapfill.lock` | Lets only one OMP process gap-fill at a time |

## Development

```bash
npm install
npm test            # adapter, retry queue, pause log, gap fill and history tests
npm run typecheck   # against @oh-my-pi/pi-coding-agent 18.6.1 types
```

| File | Responsibility |
| --- | --- |
| [`index.ts`](index.ts) | Lifecycle hooks and the `/munch` command |
| [`src/adapter.ts`](src/adapter.ts) | OMP message → event conversion, event ids, session keys. **The only file that knows OMP's message format.** |
| [`src/stats.ts`](src/stats.ts) | Read-only access to `stats.db`. **The only file that knows its schema.** |
| [`src/transport.ts`](src/transport.ts) | Sending, retry queue, history client |
| [`src/sync.ts`](src/sync.ts), [`src/syncer.ts`](src/syncer.ts) | Gap fill and history import |
| [`src/state.ts`](src/state.ts), [`src/pauses.ts`](src/pauses.ts), [`src/config.ts`](src/config.ts) | Local files |

If an OMP release changes its message format or its stats database, the fix goes in `adapter.ts` or `stats.ts`.

### Releasing

1. Bump `version` in `package.json` and `CLIENT_VERSION` in `src/adapter.ts`.
2. Deploy any server changes first (see the [Supabase README](../supabase/README.md#deploying)).
3. `npm test && npm run typecheck`
4. `npm login` (as a maintainer), then `npm publish`.

## License

MIT
