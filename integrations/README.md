# Claude Code and Codex integrations

Native plugins and a standalone command-line collector that read token usage that **Claude Code** and the **Codex CLI** already record on disk, and publish metadata-only contributions to the tokenmunchers [`/functions/v1/contributions`](../supabase/README.md#post-functionsv1contributions) endpoint. Both approaches share the same underlying engine, run on **Node.js 26 or newer** (with native TypeScript execution and zero runtime dependencies), and operate independently of the OMP plugin.

- **Native plugins with lifecycle-triggered automatic collection.** Claude Code (`tokenmunchers`) and Codex CLI (`tokenmunchers-codex`) plugins hook directly into session lifecycle events without background daemons.
- **Explicit roots only.** You choose each source root once. The collector never discovers or scans home directories on its own and never reads `$CODEX_HOME`, `~/.claude` or `~/.codex` unless you name them.
- **Metadata only.** Provider, model, token categories, call count, cost and its basis, original timestamp, session and subagent lineage, and canonical identity. Prompts, responses, tool calls, file contents and file or project paths are never persisted in the outbound projection or uploaded. Hook stdin is immediately drained and ignored.
- **Nothing uploads without confirmation.** Both manual snapshot uploads (`sync`) and recurring lifecycle uploads (`auto-on`) require explicit user consent (`--confirm-upload`). Default installation uploads nothing.
- **Credential safety.** The API key is read solely from the `TOKENMUNCHERS_API_KEY` environment variable at upload time. It is never stored in configuration files, saved in SQLite, passed as arguments, or displayed in model chat.
- **Unknown stays unknown.** Missing output, cost, call count or provider are stored and shown as unknown (`null` / `"unknown"`), never as zero or a guessed provider.

Source-specific parsing rules, fixture provenance and evidence limits: [Claude Code adapter](claude-code/README.md), [Codex adapter](codex/README.md). Historical design: [backfill analysis](../docs/claude-codex-backfill.md).

## Contents

- [Requirements and install](#requirements-and-install)
- [Native harness plugins](#native-harness-plugins)
  - [Safe staging and local installation](#safe-staging-and-local-installation)
  - [Remote Git installation](#remote-git-installation)
  - [Installed cache containment](#installed-cache-containment)
  - [Interactive user skills](#interactive-user-skills)
  - [Lifecycle hook semantics and execution](#lifecycle-hook-semantics-and-execution)
  - [Codex minimal hook flush semantics](#codex-minimal-hook-flush-semantics)
  - [SQLite process serialization and missed hook safety](#sqlite-process-serialization-and-missed-hook-safety)
- [Shared plugin runtime CLI](#shared-plugin-runtime-cli)
- [Standalone collector CLI](#standalone-collector-cli)
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

- **Node.js 26 or newer.** Sources are TypeScript run directly through Node's native type stripping. There are no runtime dependencies, so no `npm install` or compile step is needed just to run the plugins or the standalone collector.
- For local development, `npm install` inside `integrations/` installs TypeScript and `@types/node` for `npm run typecheck`.

## Native harness plugins

Tokenmunchers provides native installable plugins for both supported AI coding harnesses:

| Harness | Plugin name | Marketplace | User skill | Invocation | Lifecycle hooks |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Claude Code** | `tokenmunchers` | `tokenmunchers` | `munch` | `/tokenmunchers:munch` (or `/munch`) | `SessionStart`, `SessionEnd`, `Stop` |
| **Codex CLI** | `tokenmunchers-codex` | `tokenmunchers` | `tokenmunchers-codex:munch` | `$tokenmunchers-codex:munch` | `SessionStart`, `SessionEnd` |

### Safe staging and local installation

When installing plugins from a local directory, both Claude Code and Codex CLI register the marketplace source path and copy plugin files into their internal plugin caches (`~/.claude/plugins/cache` or `~/.codex/plugins/cache`). Running marketplace registration against an uncommitted working checkout would copy dirty files, `.env` secrets, and `node_modules`.

To prevent secret leakage and keep the cache clean, always stage a clean plugin root first using `integrations/package-plugin.ts`:

```sh
# 1. Stage clean plugin artifacts into an explicit empty directory
export PLUGIN_DIR="$HOME/.local/share/tokenmunchers/plugin"
mkdir -p "$PLUGIN_DIR"
node integrations/package-plugin.ts --output "$PLUGIN_DIR"

# 2. For Claude Code:
claude plugin marketplace add "$PLUGIN_DIR"
claude plugin install tokenmunchers@tokenmunchers

# 3. For Codex CLI (subcommand is 'add', NOT 'install'):
codex plugin marketplace add "$PLUGIN_DIR" --json
codex plugin add tokenmunchers-codex@tokenmunchers --json
```

*Important:* The staging helper requires `--output` to name an explicit directory that is empty or does not yet exist (it refuses existing non-empty directories to guarantee clean packaging). Keep `$PLUGIN_DIR` intact on disk as long as the local marketplace remains registered in Claude Code or Codex. Only remove `$PLUGIN_DIR` after removing the marketplace registration (`claude plugin marketplace remove tokenmunchers` or `codex plugin marketplace remove tokenmunchers`).
*(Codex verifies hook hashes before execution; approve the interactive trust prompt in the terminal. Do not advise `--dangerously-bypass-hook-trust` in normal use).*

### Remote Git installation

Once changes are committed and pushed to the upstream Git repository:

```sh
# Claude Code:
claude plugin marketplace add hndrxc/tokenmunchers
claude plugin install tokenmunchers@tokenmunchers
```

### Installed cache containment

The plugins use root-source packaging (`source: "."` or `{ "source": "local", "path": "." }`). When installed, the harness copies the entire repository structure required for runtime into its cache.
- **Zero checkout dependency:** The installed plugin executes entirely from `${CLAUDE_PLUGIN_ROOT}` in the cache. Deleting or modifying the original checkout does not break the installed plugin.
- **Zero build step:** Node >= 26 directly executes `<cache>/integrations/plugin/src/cli.ts`.

### Interactive user skills

Both plugins provide an administrative skill named `munch` designed for explicit user control:
- **Invocation settings:**
  - **Claude Code:** Configured with `disable-model-invocation: true` and `user-invocable: true` in `plugins/claude-code/skills/munch/SKILL.md`.
  - **Codex CLI:** Configured with `user-invocable: true` in `plugins/codex/skills/munch/SKILL.md` and `policy: { allow_implicit_invocation: false }` in `plugins/codex/skills/munch/agents/openai.yaml` (Codex rejects Claude's `disable-model-invocation` frontmatter).
- **Procedural and runtime safeguards:** No native harness policy can physically prevent a general-purpose model with shell access from executing arbitrary shell commands. However, the skill instructions strictly prohibit autonomous invocation or inferred consent, and the runtime CLI enforces explicit human confirmation flags (`--confirm-upload`) before making any network requests or modifying recurring configuration.
- **Explicit root selection:** Activation requires the user to supply an explicit `--root` path (e.g. `~/.claude` or `~/.codex`). The model never guesses roots.
- **Credential protection:** The skill instructs the user to export `TOKENMUNCHERS_API_KEY` in their terminal. It never asks the user to paste an API key into chat. Tool subshells in Codex may filter environment variables matching `*KEY*` or `*TOKEN*`; when tool filtering prevents access to the key, users should run manual `sync` in their launch terminal outside Codex or enable recurring hooks (`$tokenmunchers-codex:munch auto-on --confirm-upload`), which execute with host terminal environment inheritance.
- **Invocation routes:** In Claude Code, invoke via `/tokenmunchers:munch` (or `/munch`). In Codex CLI, the host app-server discovers and registers the plugin skill namespaced as `tokenmunchers-codex:munch` (plain `$munch` is not registered by the host), so invoke via `$tokenmunchers-codex:munch` (which injects full runtime instructions in both interactive sessions and `codex exec`). Alternatively, scripts or headless workflows can invoke the installed plugin runtime CLI directly (`node <cache_root>/integrations/plugin/src/cli.ts <COMMAND> --source codex [options]`).

### Lifecycle hook semantics and execution
Lifecycle hooks run via command invocations in `plugins/claude-code/hooks.json` and `plugins/codex/hooks.json`:
- **Execution models:**
  - **Claude Code:** Hooks (`SessionStart`, `SessionEnd`, `Stop`) specify `"async": true` and are spawned detached in the background without delaying inference or exit.
  - **Codex CLI 0.154.0:** `SessionStart` runs detached in the background (`async: true`). However, Codex applies a synchronous host override to `SessionEnd` during process teardown (`running async SessionEnd hook synchronously`), ensuring flushed transcripts are collected before exit. Active model inference is never delayed, but exiting Codex may wait up to the configured hook timeout (`timeout: 10` seconds on `SessionEnd`). If network delivery times out or is interrupted at shutdown, the uncommitted snapshot and acknowledged request metadata are retained locally in SQLite (never committed remotely when incomplete), and actual upload resumes on the next opted-in lifecycle hook (such as `SessionStart` or `SessionEnd`) or explicitly confirmed manual sync (`$tokenmunchers-codex:munch sync`). (`$tokenmunchers-codex:munch preview` scans locally only and does not trigger network delivery).
  - **Void stdin (metadata-only privacy):** Harnesses pipe turn transcripts and user prompts to hook stdin. The hook runner immediately drains and unrefs `process.stdin` without parsing, storing, or logging any turn content. (To collect accounting metrics, the engine scans local files on disk, but parses and persists allowlisted metadata only—never prompt, response, or code text).

### Codex minimal hook flush semantics

Codex CLI 0.154.0 hooks use minimal reliable triggers: `SessionStart` and `SessionEnd`.
- Codex flushes the session transcript and turn usage to disk before triggering `SessionEnd`.
- If an unexpected termination races with or prevents the final flush before exit, the next lifecycle event (`SessionStart`), `$tokenmunchers-codex:munch preview`, or manual `$tokenmunchers-codex:munch sync` scans accessible flushed logs on disk (usage never written to disk by the harness or lost before writing cannot be recovered).
### SQLite process serialization and missed hook safety

Rapid turn generation or multiple open sessions can invoke lifecycle hooks concurrently:
- **SQLite process lock:** The hook runner acquires a non-blocking SQLite lock transaction in `<state>/<harness>.lock.sqlite` held across operations and automatically released by the OS on process exit (no PID file tracking, mtime staleness, or manual lock deletion). If an active worker is already scanning or delivering, subsequent hooks skip safely without blocking or queueing duplicate processes.
- **Catch-up semantics:** Any records flushed to disk while a hook was busy or skipped will be scanned during the next preview, or scanned and delivered during the next opted-in lifecycle event or confirmed manual sync (limited to accessible flushed disk logs; preview never delivers).
- **No historical completeness claim:** Only usage recorded in accessible, on-disk session files can be collected. Sessions deleted before scanning or unrecorded by the harness cannot be recovered.
## Shared plugin runtime CLI

The shared runtime CLI (`integrations/plugin/src/cli.ts`) powers both native hooks and user skills:

```sh
node "${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts" <COMMAND> --source claude-code|codex [options]
```

### Commands

| Command | What it does | Uploads? |
| :--- | :--- | :--- |
| `activate` | Registers explicit source `--root` (required) and optional `--exclude` prefixes. Establishes immutable cutoff timestamp and device UUID. Calling again with identical arguments is a no-op; different arguments throw an error. | No |
| `preview` | Runs a local scan of the root, records new observations in SQLite, and prints a compact metadata-only summary. Any field with unknown coverage has its total reported as `null` (no known subtotal mislabelled as a total; genuine complete counts remain 0). Never triggers network delivery. | No |
| `status` | Reads and prints current ledger summary, cutoff, pause state, and auto-upload consent without scanning. | No |
| `sync` | **Scans the source root first**, then uploads the full retained ledger snapshot. Requires `--confirm-upload` and `TOKENMUNCHERS_API_KEY`. Without confirmation, prints preview and exits before any network call. | Yes (with `--confirm-upload`) |
| `auto-on` | Enables recurring background uploads when lifecycle hooks execute. Requires `--confirm-upload` and an activated source. Writes `<state>/<harness>-plugin.json`. | Config only |
| `auto-off` | Revokes recurring background hook uploads. Does not reset device identity, cutoff, or ledger records. | No |
| `pause` | Opens a pause window at the current timestamp for this source. Usage timestamped during the pause is permanently excluded. | No |
| `resume` | Closes the open pause window. Prints status. | No |
| `hook` | Executed by native lifecycle hooks. Drains stdin. Skips if unactivated. Acquires non-blocking SQLite process lock (`<state>/<harness>.lock.sqlite`, skips if busy). Scans via Collector. Delivers only if `auto-on` is consented and `TOKENMUNCHERS_API_KEY` is present. Skips delivery if paused. Suppresses delivery if no new records exist. | Yes (if `auto-on` + key) |
| `help` | Prints CLI usage and options. | No |

*Key differences on `sync`:* The plugin `sync` scans the source root into the ledger first and then delivers the snapshot, unlike the standalone CLI `sync` which only uploads the already retained ledger without scanning (run `collect` first when using the standalone CLI). Do not run the standalone `watch` loop concurrently with native plugin hooks.
### Flags

| Flag | Commands | Meaning |
| :--- | :--- | :--- |
| `--source claude-code\|codex` | all | Target harness. Required on all commands. |
| `--root PATH` | `activate` | Explicit source directory (e.g. `~/.claude` or `~/.codex`). Stored as absolute path and immutable afterwards. |
| `--exclude PREFIX` | `activate` | Path prefix to exclude (repeatable). Not a glob. |
| `--state DIR` | all | Local state directory. Default: `~/.local/state/tokenmunchers-collector`. *Caveat:* Installed native hooks invoke statically without `--state` and always resolve to the default directory; `--state DIR` in manual/testing commands does not redirect registered native hooks, so `auto-on` configured in a custom state directory is not picked up by default hooks. |
| `--endpoint URL` | `sync`, `hook` | Contributions endpoint. Default: Supabase production URL. HTTPS required unless localhost. |
| `--confirm-upload` | `sync`, `auto-on` | Explicit user confirmation to upload. Without it, nothing is transmitted. |
| `--max-bytes N` | `preview`, `sync`, `hook` | Scan batch size limit (default: 64 MiB). |
| `--max-line-bytes N` | `preview`, `sync`, `hook` | Scan line size limit (default: 1 MiB). |
| `--help` | all | Prints usage. |

## Standalone collector CLI

The standalone collector in `integrations/collector/src/cli.ts` (runnable via `npm run collect -- ...`) remains supported for manual scans, archive backfilling, or headless environments:

```sh
cd integrations

# 1. Activate explicit root
npm run collect -- activate --source codex --root ~/.codex

# 2. Scan locally (records to SQLite ledger; nothing uploaded)
npm run collect -- preview --source codex

# 3. Upload retained ledger snapshot
export TOKENMUNCHERS_API_KEY=tm_...
npm run collect -- sync --source codex \
  --endpoint https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions --confirm-upload

# 4. Optional: keep collecting via polling loop
npm run collect -- watch --source codex \
  --endpoint https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions --confirm-upload
```

*Concurrency warning:* Do not run the standalone `watch` loop concurrently with native plugin hooks.

## Status and preview output

The standalone collector CLI (`npm run collect -- preview/status`) and shared plugin CLI (`preview`/`status`/`sync`) display consistent metadata accounting:

### Standalone collector output
`collect`, `status`, `preview`, `pause`, `resume` and the first part of `sync`/`watch` print the detailed ledger state:
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
| `days` | UTC daily totals per provider/model/subagent flag. A sum that includes an unknown value is `null`, with `known_subtotals` alongside, plus the qualities and cost bases that went into each day. Known subtotals are never mislabelled as complete totals. |
| `withheld` | Counts of contributions excluded from totals and upload, by reason (see [below](#what-is-withheld-from-upload)). |
| `progress` | Last scan: count, observations returned, diagnostic code counts, time. |
| `scan_pending.cursor` / `.continuation` | Where the next bounded scan resumes (local root-relative positions; never uploaded). |
| `canonical_conflicts` | Identities seen with differing accounting; the most complete frame is kept and flagged. |
| `identity_alias_conflicts` | Responses flagged `IDENTITY_ALIAS_CONFLICT`: one request named two different responses. The first durable request→response mapping wins, the later alias is skipped, and both responses are kept separately (never merged). |
| `rejected_observations` | Per field, how many source rows failed metadata validation (for example a pre-1970 timestamp, or a model or session ID that is too long or has control characters). Each row is rejected alone and never stored or uploaded; the rest of the scan commits. Only a SHA-256 of the row's canonical ID and the field name are kept. |
| `superseded_intervals` | Reconstructed Codex intervals retired by stronger evidence. |
| `pending_snapshot` | Snapshot UUID staged but not yet committed. |
| `pending_acknowledged` | `{ requests, chunk_count }`: how many of that snapshot's requests (begin, then chunks) the endpoint already acknowledged. The next `sync` resumes there. |

### Plugin compact status output
The shared plugin CLI prints a compact summary object (`formatCompactStatus`):
`{ harness, root, device_id, cutoff, paused, recurring_upload, endpoint, range_utc, retained_contributions, live, history, excluded, totals, qualities, unknown, weak_identity_count, exclusions, diagnostics, progress, pending_snapshot, pending_acknowledged }`.

- **Nullable compact totals (`totals`):** Includes `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `call_count`, and `cost_usd`. Any field with missing or unverified coverage (such as unpriced Codex rollouts or unverified Claude output) is reported as `null`, avoiding known-subtotal mislabelling. Complete fields with genuine zero usage remain numeric `0`.
- **Separate known subtotals:** In daily aggregates (`days[].known_subtotals`), known subtotals are tracked alongside `null` sums and are never conflated with complete totals.

Scans also print `latest_scan`: duplicates merged, the excluded count, rows rejected by metadata validation (`rejected`, also counted as `INVALID_OBSERVATION` in `progress.diagnostics`), local diagnostics (code plus root-relative file and line), project names found under the root (Claude), and adapter progress. All of it is local only.
## Local state

- One SQLite file, `<state>/ledger.sqlite`. The directory is created with mode `0700` and the file is forced to `0600`. Each change runs in one `BEGIN IMMEDIATE` transaction with `synchronous=FULL`, so a crash or kill leaves either the previous state or the new one, never a partial scan.
- Contents per source: root, exclusions, device UUID, cutoff, pause windows, the normalized metadata ledger keyed by canonical ID, excluded/superseded/conflicting IDs, identity aliases and which of them are material for upload, rejected-row field codes keyed by canonical-ID digest, scan cursor/continuation, last-scan progress, and the pending snapshot with its acknowledged progress. Every read and write is validated against a strict schema. A hand-edited or corrupted file fails with `Invalid protected ledger state` instead of being trusted. State written before alias materiality was tracked still loads; all of its existing aliases are treated as material.
- **Per-harness plugin configuration:** In addition to `ledger.sqlite`, recurring hook delivery consent and settings are saved in `<state>/<harness>-plugin.json` (forced to mode `0600`). It tracks `{ auto_upload: boolean, endpoint?: string, updated_at: string }`. Disabling auto-upload (`auto-off`) updates this file without touching `ledger.sqlite` or resetting device identities. Installed native lifecycle hooks invoke statically without `--state` and always read from the default state directory (`~/.local/state/tokenmunchers-collector`), so `auto-on` configured under a custom `--state` directory is not picked up by native hooks.
- **Process serialization:** Per-harness SQLite lock databases (`<state>/<harness>.lock.sqlite`) hold an active transaction to serialize native hook and CLI executions, preventing overlapping scans or uploads with automatic OS-level release on exit.
- Never stored: raw records, prompts, responses, tool content or local diagnostic messages. The API key isn't stored either.
- One state directory holds at most one Claude Code source and one Codex source. To collect a second installation (another `CODEX_HOME`, for example), use a separate `--state` directory. It gets its own device UUID and cutoff.
- Don't delete or copy the state directory to start over. A new state directory is a new device, and identities it shares with uploads from the old device are rejected as ownership conflicts (see [Ownership limits](#ownership-limits)).
- **One shared default directory.** Both native plugins and the standalone CLI use `~/.local/state/tokenmunchers-collector` unless you pass `--state`. Never delete or recreate it to start over: that creates a new device UUID and previously uploaded records then fail with `409 ownership_conflict`.

## Credential

`TOKENMUNCHERS_API_KEY` is read from the environment at `sync`/`watch` and hook upload time, sent as `Authorization: Bearer …`. It is never written to state, saved in configuration, passed as arguments, or printed. Use a dedicated key per machine from dashboard Settings. The key is **not** required when activating or configuring `auto-on` (which records user consent locally), but is strictly required when network delivery executes. A `401`/`403` stops the upload with `Collector API key unauthorized`, and the staged snapshot is kept for the next attempt.
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
3. **`begin`** (with `cutoff` and `chunk_count`), each **`chunk`** in order, then **`commit`**. The server publishes atomically at commit. Interrupted snapshots or acknowledged request counts are retained locally in SQLite and are **never committed remotely when incomplete**; the server's committed totals remain unchanged until commit succeeds. Source files deleted since earlier uploads don't remove accepted contributions.
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

### Native verification and parent observations
Parent smoke testing verified both native CLI install and lifecycle hook cycles using local mock providers and fixture metadata in isolated sandboxes:
- Verified Claude Code marketplace addition, installation of `tokenmunchers@tokenmunchers`, and detached async hooks (`SessionStart`, `SessionEnd`, `Stop`).
- Verified Codex CLI 0.154.0 marketplace addition, addition of `tokenmunchers-codex@tokenmunchers`, background `SessionStart`, and synchronous teardown override on `SessionEnd` (`running async SessionEnd hook synchronously`) with configured 10-second timeout.
- Verified default installation makes zero network uploads, and hook runs cleanly skip when unactivated or unconsented.
These verification runs test harness integration mechanics and do not represent an expansion of real-version or provider evidence beyond the cataloged fixtures.

Layout:

| Path | Role |
| :--- | :--- |
| `shared/src/observations.ts` | Adapter contract: `Observation`, `Diagnostic`, `ScanOptions`, `AdapterResult`, `Adapter`. |
| `shared/src/metadata.ts` | The single persistence/outbound allowlist (`metadata`, `outbound`) and deterministic `reconcile`. |
| `shared/src/aggregate.ts` | `attributionIssue` (withholding reasons) and nullable UTC daily `aggregate`. |
| `shared/src/ledger.ts`, `shared/src/state.ts` | Durable SQLite ledger: activation, record, pause; strict state validation. |
| `shared/src/delivery.ts` | Staged snapshot delivery with rate-reset waits and resumable progress. |
| `collector/src/collector.ts`, `collector/src/cli.ts` | Scan orchestration, preview, and standalone collector CLI. |
| `claude-code/`, `codex/` | Read-only source adapters (`scan(root, options)`). |
| `plugin/src/` | Shared native plugin runtime CLI (`cli.ts`), hooks runner, lock (`lock.ts`), config (`config.ts`), and compact formatting (`format.ts`). |
| `package-plugin.ts` | Safe clean staging helper for local plugin marketplace distribution. |

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
