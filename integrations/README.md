# Claude Code and Codex collector

A standalone command-line collector that reads token usage that **Claude Code** and the **Codex CLI** already record on disk and publishes metadata-only contributions to the tokenmunchers [`/functions/v1/contributions`](../supabase/README.md#post-functionsv1contributions) endpoint. It is separate from the OMP plugin: it doesn't use OMP's `ingest`/`history` functions, OMP's stats database, or OMP state.

- **Explicit roots only.** You choose each source root once. The collector never discovers or scans home directories on its own and never reads `$CODEX_HOME`, `~/.claude` or `~/.codex` unless you name them.
- **Metadata only.** Provider, model, token categories, call count, cost and its basis, original timestamp, session and subagent lineage, and canonical identity. Prompts, responses, tool calls, file contents and file or project paths are never persisted in the outbound projection or uploaded.
- **Nothing uploads without confirmation.** Uploads need `--endpoint`, the `TOKENMUNCHERS_API_KEY` environment variable and `--confirm-upload`, and a preview prints first.
- **Unknown stays unknown.** Missing output, cost, call count or provider are stored and shown as unknown (`null` / `"unknown"`), never as zero or a guessed provider.

Source-specific parsing rules, fixture provenance and evidence limits: [Claude Code adapter](claude-code/README.md), [Codex adapter](codex/README.md). Historical design: [plan](PLAN.md) and [backfill analysis](../docs/claude-codex-backfill.md).

## Contents

- [Requirements and install](#requirements-and-install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [Flags](#flags)
- [Status and preview output](#status-and-preview-output)
- [Local state](#local-state)
- [Credential](#credential)
- [Cutoff: history vs. live](#cutoff-history-vs-live)
- [Pause and resume](#pause-and-resume)
- [Exclusions](#exclusions)
- [What is withheld from upload](#what-is-withheld-from-upload)
- [Delivery protocol](#delivery-protocol)
- [Ownership limits](#ownership-limits)
- [Limits](#limits)
- [Development](#development)
- [Acceptance scenario map](#acceptance-scenario-map)

## Requirements and install

- **Node.js 26 or newer.** Sources are TypeScript run directly through Node's native type stripping. There are no runtime dependencies, so no install step is needed just to run the collector.
- This is a private package (`tokenmunchers-collector`) with no published binary. Run it from this directory:

```sh
cd integrations
npm run collect -- help          # same as: node collector/src/cli.ts help
```

From the repository root, use `node integrations/collector/src/cli.ts <command> …`. `npm install` is only needed for development: it installs TypeScript and `@types/node` for `npm run typecheck`.

## Quick start

```sh
cd integrations

# 1. Activate each source once with its explicit root (pick one or both).
npm run collect -- activate --source codex --root ~/.codex
npm run collect -- activate --source claude-code --root ~/.claude --exclude projects/-home-me-client-work

# 2. Scan locally and inspect the preview. Nothing is uploaded.
npm run collect -- preview --source codex

# 3. Create an API key in the dashboard (Settings → API keys), then upload.
export TOKENMUNCHERS_API_KEY=tm_...
npm run collect -- sync --source codex \
  --endpoint https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions --confirm-upload

# 4. Optional: keep collecting. Scans every 60 s and uploads after each scan.
npm run collect -- watch --source codex \
  --endpoint https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions --confirm-upload
```

The contributions endpoint works only after the collector migration and the `contributions` Edge Function are deployed (see [supabase/README.md](../supabase/README.md#deploying)). Each source is activated, paused and uploaded on its own. Repeat each command with `--source claude-code` for Claude Code.

## Commands

Every command except `help` requires `--source claude-code|codex`. Output is JSON on stdout. Errors print one line on stderr and set exit code 1.

| Command | What it does | Uploads? |
| --- | --- | --- |
| `activate` | Registers the source with `--root` (required) and optional `--exclude` prefixes. Creates a random source-isolated device UUID and records the **cutoff** as the current time. Running it again with the same root and exclusions is a no-op that returns the original cutoff. A different root or different exclusions is rejected. | No |
| `preview` | Runs one bounded scan of the activated root, records normalized metadata into the local ledger, then prints the preview. | No |
| `collect` | Same local operation as `preview`. Use it in scripts where "preview" reads oddly. | No |
| `sync` | Prints the preview of the retained ledger, then uploads the **whole retained ledger** as one staged snapshot. Requires `--endpoint`, `--confirm-upload` and `TOKENMUNCHERS_API_KEY`. Without `--confirm-upload` it prints the preview and exits with an error before any network request. `sync` does not scan; run `collect` first. | Yes |
| `watch` | Loops: scan, print the preview, then upload if `--confirm-upload` is set and the source isn't paused. Then it waits `--interval-ms`. Scan or upload errors print and the loop continues. `Ctrl-C`/`SIGTERM` stops it. | Only with `--confirm-upload` |
| `pause` | Opens a pause for this source at the current time (see [Pause and resume](#pause-and-resume)). Prints status. | No |
| `resume` | Closes the open pause at the current time. Prints status. | No |
| `status` | Prints the preview from local state without scanning. | No |
| `help` | Prints usage. | No |

`--root` and `--exclude` are accepted only by `activate`. Later commands reuse the stored selection and reject those flags.

## Flags

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--source claude-code\|codex` | all | Which source. Required, no default. |
| `--root DIR` | `activate` | Explicit source root, typically your Claude configuration directory (`~/.claude`) or Codex home (`~/.codex`). Stored as an absolute path and immutable afterwards. |
| `--exclude PREFIX` | `activate` (repeatable) | Root-relative path prefix to skip, for example `projects/-home-me-client` or `sessions/2026/01`. Not a glob. |
| `--state DIR` | all | Local state directory. Default `~/.local/state/tokenmunchers-collector`. |
| `--max-bytes N` | `preview`, `collect`, `watch` | Bytes read per scan (default 64 MiB; for Codex `.zst` it also bounds compressed and decompressed bytes). Larger archives continue on the next scan. |
| `--max-line-bytes N` | `preview`, `collect`, `watch` | Longest record parsed (default 1 MiB). Longer records are skipped with a diagnostic. |
| `--endpoint URL` | `sync`, `watch` | Contributions endpoint. Must be `https://`; `http://` is allowed only for `localhost`, `127.0.0.1` and `[::1]`. |
| `--confirm-upload` | `sync`, `watch` | Explicit consent to upload. Without it nothing is sent. |
| `--interval-ms N` | `watch` | Delay between watch iterations (default 60000, minimum 1000). |
| `--help` | all | Prints usage. |

## Status and preview output

`status`, `preview`, `collect`, `pause`, `resume` and the first part of `sync`/`watch` print the same object:

| Field | Meaning |
| --- | --- |
| `harness`, `root`, `cutoff`, `device_id`, `exclusions` | The immutable activation. |
| `paused` | `true` while a pause is open. |
| `range_utc.first` / `.last` | Earliest and latest original timestamps retained. |
| `retained_contributions` | Distinct canonical contributions in the local ledger, including ones whose source files were deleted since. |
| `history` / `live` | Uploadable contributions with original timestamp before / at-or-after the cutoff. |
| `excluded` | Identities permanently excluded by a pause. |
| `unknown` | Per field, how many retained contributions have it unavailable (`null`, or provider `"unknown"`). |
| `qualities` | Counts per quality: `observed`, `reconstructed`, `partial`, `summary`. |
| `weak_identity_count` | Contributions with only a scoped (single-owner) identity. See [Ownership limits](#ownership-limits). |
| `ownership_policy` | Reminder of the single-owner rule. |
| `days` | UTC daily totals per provider/model/subagent flag. A sum that includes an unknown value is `null`, with `known_subtotals` alongside, plus the qualities and cost bases that went into each day. |
| `withheld` | Counts of contributions excluded from totals and upload, by reason (see [below](#what-is-withheld-from-upload)). |
| `progress` | Last scan: count, observations returned, diagnostic code counts, time. |
| `scan_pending.cursor` / `.continuation` | Where the next bounded scan resumes (local root-relative positions; never uploaded). |
| `canonical_conflicts` | Identities seen with differing accounting; the most complete frame is kept and flagged. |
| `identity_alias_conflicts` | Responses flagged `IDENTITY_ALIAS_CONFLICT`: one request named two different responses. The first durable request→response mapping wins, the later alias is skipped, and both responses are kept separately (never merged). |
| `rejected_observations` | Per field, how many source rows failed metadata validation (for example a pre-1970 timestamp, or a model or session ID that is too long or has control characters). Each row is rejected alone and never stored or uploaded; the rest of the scan commits. Only a SHA-256 of the row's canonical ID and the field name are kept. |
| `superseded_intervals` | Reconstructed Codex intervals retired by stronger evidence. |
| `pending_snapshot` | Snapshot UUID staged but not yet committed. |
| `pending_acknowledged` | `{ requests, chunk_count }`: how many of that snapshot's requests (begin, then chunks) the endpoint already acknowledged. The next `sync` resumes there. |

Scans also print `latest_scan`: duplicates merged, the excluded count, rows rejected by metadata validation (`rejected`, also counted as `INVALID_OBSERVATION` in `progress.diagnostics`), local diagnostics (code plus root-relative file and line), project names found under the root (Claude), and adapter progress. All of it is local only.

## Local state

- One SQLite file, `<state>/ledger.sqlite`. The directory is created with mode `0700` and the file is forced to `0600`. Each change runs in one `BEGIN IMMEDIATE` transaction with `synchronous=FULL`, so a crash or kill leaves either the previous state or the new one, never a partial scan.
- Contents per source: root, exclusions, device UUID, cutoff, pause windows, the normalized metadata ledger keyed by canonical ID, excluded/superseded/conflicting IDs, identity aliases and which of them are material for upload, rejected-row field codes keyed by canonical-ID digest, scan cursor/continuation, last-scan progress, and the pending snapshot with its acknowledged progress. Every read and write is validated against a strict schema. A hand-edited or corrupted file fails with `Invalid protected ledger state` instead of being trusted. State written before alias materiality was tracked still loads; all of its existing aliases are treated as material.
- Never stored: raw records, prompts, responses, tool content or local diagnostic messages. The API key isn't stored either.
- One state directory holds at most one Claude Code source and one Codex source. To collect a second installation (another `CODEX_HOME`, for example), use a separate `--state` directory. It gets its own device UUID and cutoff.
- Don't delete or copy the state directory to start over. A new state directory is a new device, and identities it shares with uploads from the old device are rejected as ownership conflicts (see [Ownership limits](#ownership-limits)).

## Credential

`TOKENMUNCHERS_API_KEY` is read from the environment at `sync`/`watch` time and sent as `Authorization: Bearer …`. It is never written to state or printed. Use a dedicated key per machine from dashboard Settings. The key's rate budget is shared with any OMP plugin using the same key. A `401`/`403` stops the upload with `Collector API key unauthorized`, and the staged snapshot is kept for the next attempt.

## Cutoff: history vs. live

- `activate` stores the current UTC time as the source's **cutoff**. It never moves. Re-activation returns the original, and the server rejects a different cutoff for the same device (`409 cutoff_conflict`).
- Every contribution is classified by its **original recorded timestamp**, not by when the collector saw it. Before the cutoff is history; at or after it is live. A response flushed to disk after activation but timestamped before the cutoff is still history.
- Both partitions are stored once in the same contribution table, so history and live never overlap. There is no seven-day age limit. A reconstructed interval that straddles the cutoff is withheld.

## Pause and resume

- `pause` opens a window at the current time for **this source only**. The other source and the OMP plugin are unaffected.
- While paused, `sync` refuses (`Source paused; resume before delivery`). `watch` keeps scanning but doesn't upload.
- Any contribution whose original timestamp (or interval) falls inside a pause window is **permanently excluded**. It is dropped from the ledger and its identity is added to `excluded`, so later scans, late-flushed records and later uploads never count it, even after `resume`.
- `resume` closes the window. Usage after resume is collected normally. Pause windows survive restarts. Contributions without a timestamp can't be placed in or out of a pause; they are withheld from upload anyway.

## Exclusions

`--exclude` prefixes are relative to the root and fixed at activation. Discovery skips them on every scan, and the adapters don't follow symlinked files or directories, so a link can't pull in an unselected path. To change exclusions, stop using that state directory and activate a new one. That is a new device, subject to [ownership limits](#ownership-limits): identities it shares with the old device's uploads will conflict.

## What is withheld from upload

These stay in the local ledger and show up in `withheld`, but are never uploaded or added to totals:

| Reason | Meaning |
| --- | --- |
| `NONADDITIVE_SUMMARY` | Session summaries (for example Claude saved cost state). They would double-count per-response usage. |
| `MISSING_TIMESTAMP` | No original timestamp, so no day or cutoff partition can be assigned honestly. |
| `AMBIGUOUS_INTERVAL_DAY` | A reconstructed interval without both endpoints on one UTC day. |
| `INTERVAL_STRADDLES_CUTOFF` | A reconstructed interval that crosses the activation cutoff. |
| `CONFLICTING_RESPONSE` | A strong response ID reused with incompatible accounting. |

## Delivery protocol

`sync` (and `watch --confirm-upload`) publishes a **full snapshot** of the retained ledger with the staged protocol in [supabase/README.md](../supabase/README.md#post-functionsv1contributions):

1. **Freeze.** On first attempt, every uploadable contribution plus explicit supersessions and material identity aliases is copied into `pending` with a new `snapshot_id`. An alias is material only when this device recorded a request-identity fallback before its response, or merged a later fallback through it. Aliases for responses whose request was never recorded stay local: they still fold any later fallback into the response, so it can't come back as a separate request. A failed upload keeps that exact snapshot, and every retry resends it. Scans that happen meanwhile are included in the next snapshot, after this one commits.
2. **Partition.** The snapshot is split deterministically into chunks of at most 500 entries and 256 KiB per request body. More than 1,000 chunks is refused.
3. **`begin`** (with `cutoff` and `chunk_count`), each **`chunk`** in order, then **`commit`**. The server publishes atomically at commit. An interrupted upload leaves the last committed totals unchanged, and source files deleted since earlier uploads don't remove accepted contributions.
4. **Acknowledge.** The pending snapshot is cleared and `delivered_at` recorded only after a valid commit acknowledgment. `sync` prints `{ accepted, snapshot_id, resumed_at_request, rate_limit_waits }`.

Retry and resume rules:

| Response | Collector behavior |
| --- | --- |
| Network error, timeout (15 s per request), `5xx`, or a `429` without a valid `Retry-After` | Transient: up to 4 attempts per request with 0.5 s / 1 s / 2 s backoff (a `5xx` `Retry-After` is honored up to 30 s). |
| `429` with `Retry-After: 1..60` (or body `retry_after_seconds`) | **Rate-limit reset:** waits that many seconds plus 250 ms, then retries **the same request**. This doesn't consume transient attempts. Hard bounds: 3 consecutive waits per request and `ceil(requests / 60) + 2` waits per sync. |
| `429 staging_limit` | Stops. The account already has four uncommitted snapshots. Retry after other installations commit or their staging expires. |
| `401`/`403` | Stops: the key is unauthorized. |
| `409` (`ownership_conflict`, `cutoff_conflict`, `snapshot_conflict`, `stale_snapshot`, …) | Stops and names the code. Don't reset the device identity to get around it. |
| Other `4xx` | Stops: `Snapshot rejected`. |

When a sync stops, the number of requests the endpoint acknowledged (`begin` plus completed chunks) is saved with the pending snapshot. This includes `Ctrl-C`/`SIGTERM` during `sync` or `watch`, which abort the in-flight request or wait. The next `sync` **resumes at the failed request instead of chunk zero**. If the server no longer has that staged snapshot (staging expires after seven days), it answers `snapshot_conflict`/`incomplete_snapshot` and the collector replays once from `begin`. Exact chunk replays are idempotent server-side. A killed process (`SIGKILL`, power loss) loses only that resume hint and restarts at `begin`, which is still safe.

Throughput: each request costs 10 of the key's 600 units per UTC minute, so one minute fits 60 requests (begin plus 59 chunks, about 29,500 contributions). Bigger snapshots wait out minute resets: about 17 minutes for the 1,000-chunk maximum, longer if other traffic shares the key.

## Ownership limits

- **One collector per archive.** Canonical identities are owned server-side by the first device that publishes them, per user and harness. A copied archive activated on another machine, or in a second state directory, publishes the same strong IDs, and its commit fails with `409 ownership_conflict` rather than counting twice. The collector surfaces this. It doesn't resolve it.
- **Weak identities can't be matched globally.** Claude UUID fallbacks and Codex legacy reconstructed intervals carry only scoped identities (`weak_identity_count`). Independently rewritten or renamed copies of such sessions can't be recognized as duplicates. Activate exactly one collector for any archive that contains them, and don't treat totals that include them as globally exact.
- **Corrections are evidence-led and same-owner.** Supersessions retire only same-device reconstructed intervals, and aliases only upgrade same-device request identities to response identities. Missing source files never delete or retract accepted usage.
- **Separate from OMP.** Collector contributions and OMP plugin usage are different sources. Don't use the collector to re-import usage OMP already reported.

## Limits

- Accounts only for usage the retained files record. Responses a harness never wrote to disk, or deleted before the first scan, can't be recovered.
- Claude: request output is verified only when a fully read session's distinct responses sum exactly to the latest cost-state per model (real 2.1.312 Haiku evidence; see the [Claude README](claude-code/README.md#fixture-provenance-and-real-session-evidence)). Otherwise output stays null; request cost and provider stay unknown.
- Codex: rollouts carry no prices (`cost_usd: null`). Legacy-only usage is reconstructed into intervals with unknown call counts. Real evidence covers Codex CLI 0.154.0 only; see the [Codex README](codex/README.md#gates-and-remaining-evidence-gaps).
- Scans are full bounded passes, with cursors and continuations as optimizations. The durable canonical ledger, not file offsets, provides replay idempotence.
- Per snapshot: at most 1,000 chunks × 500 entries, 256 KiB per request. Values: tokens and calls ≤ 1e9, cost ≤ 10,000 USD, IDs ≤ 512 characters. Values are never clamped. The Claude adapter reports an out-of-range token or cost field as unavailable (`null`). Any other observation that fails validation is rejected on its own and counted in `rejected_observations`, and the rest of the scan still commits.

## Development

From this directory with Node.js ≥ 26:

```sh
npm install            # TypeScript + @types/node (dev only)
npm run typecheck      # tsc --noEmit over shared, collector, claude-code, codex
npm test               # node --test over all four suites
```

Layout:

| Path | Role |
| --- | --- |
| `shared/src/observations.ts` | Adapter contract: `Observation`, `Diagnostic`, `ScanOptions`, `AdapterResult`, `Adapter`. |
| `shared/src/metadata.ts` | The single persistence/outbound allowlist (`metadata`, `outbound`) and deterministic `reconcile`. |
| `shared/src/aggregate.ts` | `attributionIssue` (withholding reasons) and nullable UTC daily `aggregate`. |
| `shared/src/ledger.ts`, `shared/src/state.ts` | Durable SQLite ledger: activation, record, pause; strict state validation. |
| `shared/src/delivery.ts` | Staged snapshot delivery with rate-reset waits and resumable progress. |
| `collector/src/collector.ts`, `collector/src/cli.ts` | Scan orchestration, preview, and the CLI. |
| `claude-code/`, `codex/` | Read-only source adapters (`scan(root, options)`). |

## Acceptance scenario map

Collector and shared regressions are deterministic and use only synthetic data, except the Codex fixtures labeled real in the [Codex manifest](codex/test/fixtures/manifest.json). Prefixes: S = `shared/test/accounting.test.ts`, D = `shared/test/delivery.test.ts`, C = `collector/test/collector.test.ts`, CL = `claude-code/test/adapter.test.ts`, CA = `codex/test/accounting.test.ts`, CR = `codex/test/reader.test.ts`. Backend checks live in [supabase/README.md](../supabase/README.md#scenario-to-regression-map). Listing a test here doesn't mean it has run.

| Scenario | Regressions |
| --- | --- |
| Same history imported twice: no increase | S `canonical replay, child work and cleanup retain one contribution across restart`; D `lost commit acknowledgment retains snapshot and replay is harmless`; CR `chosen-root discovery …` |
| Same response in several files or a fork; new child work retained | CL `fork copied prefix does not inflate …`; CA `fork prefix ownership skips legacy …`; CA `ordinary forks are not fabricated as subagents …`; CA `strong identities deduplicate across sessions …` |
| Subagents | CL `subagent work is linked once …`; CL `actual parent assistant API work is separate …`; CA `subagent lineage uses explicit source metadata …`; CR `redacted real 0.154.0 parent and subagent rollouts …`; S `canonical replay, child work …` |
| Source logs deleted after import | S `canonical replay, child work and cleanup …`; C `both actual adapters feed the same retention-safe collector …`; CL `retention cleanup reports no replacement/delete instruction …`; D `interrupted staged snapshot …` (cleanup between attempts) |
| Exit mid-record or mid-scan | S `process termination inside transaction leaves no partial metadata or cursor`; S `metadata, progress and cursor roll back atomically on a rejected scan`; CL `partial trailing JSON is withheld until newline …`; CR `incomplete trailing record is deferred …`; CR `valid but unterminated final JSON …`; C `bounded scan resumes protected progress …` |
| One malformed source row or conflicting request alias never blocks later scans | S `one invalid observation is rejected alone with a durable field diagnostic while the scan commits`; S `a request naming a second response skips that alias durably and keeps both responses` |
| Pre-cutoff response flushed after activation | C `bounded scan resumes protected progress …` (scanned after activation, counted as history); S `original UTC timestamps partition history/live without overlap …` |
| One source paused or excluded; the other unaffected | S `persistent source-isolated pause excludes late-flushed calls after restart and resume`; D `no upload without confirmation, paused source, …`; S `canonical replay …` (immutable root/exclusions); CL `explicit selected roots, prefix exclusions and symlink refusal …`; CR `root-relative exclusions and symlink refusal …`; C `bounded scan …` (stored exclusions passed to adapter) |
| Output, pricing, calls or provider unavailable | S `reconciliation is deterministic, metadata allowlisted and null never zeroed`; S `original UTC timestamps …` (null sums with known subtotals); C `bounded scan …` (`unknown` counts); CL `streaming frames coalesce …`; CL `invalid and absent token categories remain null …`; CA `unknown timestamp is null …` |
| Legacy and modern Codex in one file | CA `mixed legacy/modern exact cumulative boundary …`; CA `legacy snapshot already containing modern response …`; CA `transition with absent or invalid cumulative evidence …`; S `request identity upgrade and evidence-based legacy supersession survive replay`; D `full snapshots carry explicit supersessions and only material identity upgrades` |
| Request→response aliases upload only when material | D `full snapshots carry explicit supersessions and only material identity upgrades` (fallback-then-response and late fallback upload; response-only stays local); D `response-only aliases at scale upload only the material alias subset`; D `legacy ledger state without alias materiality or rejections loads and keeps uploading its aliases` |
| Overflow and counter resets | CA `legacy fixture discards 200000 context fill …`; CA `legacy repeats and counter drops reset safely …`; CA `malformed cache subsets, totals, reasoning, null and unsafe counts are rejected …`; CL `invalid and absent token categories …`; S `reconciliation …` (out-of-range values rejected, never clamped) |
| Compressed discovery | CR `chosen-root discovery reads nested active and flat archives, plain and streaming zstd …`; CR `compressed expansion consumes the decompressed byte budget …`; CR `concatenated zstd frames …` |
| Snapshot larger than one request; interruption keeps last-good totals | D `interrupted staged snapshot leaves last-good totals; restart resumes at the failed request`; D `resume after server-side staging expiry replays once from begin`; D `lost commit acknowledgment …` |
| Large snapshot crossing a rate-limit reset | D `begin plus 59 chunks crossing a rate reset waits once and commits without replaying the prefix`; D `persistent rate limiting stops at a hard bound and the next sync resumes at the limited request`; D `staging_limit 429 without Retry-After is reported, not retried as a reset wait` |
| Archive copied from another machine | D `no upload without confirmation, …, ownership conflict` (409 surfaced); CA `strong identities deduplicate across sessions; conflicting accounting is visibly withheld`; CL `UUID fallbacks are stable only within each transcript …`; server-side ownership in the supabase smoke |
| Metadata privacy | S `reconciliation …` (poisoned fields dropped); C `both actual adapters …` (no message/content persisted); D `interrupted staged snapshot …` (no diagnostics or root path in requests) |
