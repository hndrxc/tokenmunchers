# Claude Code & Codex CLI Backfill and Historical Accounting

*Note: the native Claude Code and Codex plugins and the collector (`integrations/`) were removed. Those tools are now reported through `munch`. Paths to `integrations/` below refer to removed code.*

**Status (2026-11-22):** Observed Codex usage backfill shipped in Munch 0.1.7; the database migration and updated `contributions` function are deployed to production. See [section 8](#8-munch-issue-4-observed-codex-import) for the import rules and [section 9](#9-codex-desktop-compatibility-check) for desktop compatibility. Claude Code history import remains unavailable.

**Date:** 2026-10-27 | **Scope:** Design review for Claude Code and OpenAI Codex CLI in `tokenmunchers`  
**Sources:** Upstream repositories and packages at pinned commits. No application code changes.  
- **Claude Code:** Embedded commit `f375cd85d4f7` inside [`@anthropic-ai/claude-agent-sdk@0.3.312`](https://www.npmjs.com/package/@anthropic-ai/claude-agent-sdk) (`manifest.json`), [Monitoring Docs](https://code.claude.com/docs/en/monitoring-usage), [Sessions Docs](https://code.claude.com/docs/en/sessions), [Cost Tracking Docs](https://code.claude.com/docs/en/agent-sdk/cost-tracking). Note: Transcript schema is internal and version-specific without public SLA.  
- **OpenAI Codex CLI:** [`openai/codex`](https://github.com/openai/codex) at pinned commit [`6562117a81a9`](https://github.com/openai/codex/tree/6562117a81a9457c9d67a4d15b884bf80e7805f7).  
- **tokenmunchers:** [`20261004220000_init.sql`](../supabase/migrations/20261004220000_init.sql), [`20261005120000_history_and_retention.sql`](../supabase/migrations/20261005120000_history_and_retention.sql), [`history/index.ts`](../supabase/functions/history/index.ts).

---

## 1. Executive Summary & Core Architectural Invariants

1. **Local Files Are the Backfill Source, Not a Billing Ledger:** Inspect retained Claude transcripts and Codex rollouts directly; high-level session listings and Codex's SQLite index do not provide the same granular usage history.
2. **Preserve Availability and Quality:** Unreported cost is unknown, not $0.00. Claude can persist cumulative estimated spend on normal exit; those summaries are not per-call invoices or automatically attributable to UTC days. Never infer a provider solely from the model name.
3. **Disjoint Token Math:** Tokenmunchers computes $\text{total} = \text{input} + \text{output} + \text{cache\_read} + \text{cache\_write}$. Claude API input is exclusive. The inspected Codex Responses API accounting requires subtracting cache-read and cache-write from inclusive input.
4. **OTel Joins Are Not Uniform:** The inspected Codex completed-token log lacks `response_id`. Claude `api_request.request_id` can join transcript `requestId` when both are present, but this is not guaranteed for every connection.
5. **Prefer Shared Local Parsing:** Reuse one harness-specific parser and canonical-ID ledger for backfill and incremental collection. This removes the need to reconcile independent telemetry identities; it does not by itself solve missing usage or live/history overlap.
6. **Preserve Previously Accepted Contributions:** A full replacement rebuilt only from currently retained logs can delete earlier imports after cleanup. Keep a durable metadata ledger, or change the server's replacement contract.

---

## 2. Discovery, Paths, and Retention Lifecycles

| Dimension | Claude Code | OpenAI Codex CLI |
| :--- | :--- | :--- |
| **Config Home** | `$CLAUDE_CONFIG_DIR` (fallback: `~/.claude`) | `$CODEX_HOME` (fallback: `~/.codex`) |
| **Active Storage** | `$CLAUDE_CONFIG_DIR/projects/<slug>/<sessionId>.jsonl` | [`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl[.zst]`](https://github.com/openai/codex/blob/main/codex-rs/rollout/src/lib.rs#L86) |
| **Subagents** | `<sessionId>/subagents/agent-<agentId>.jsonl` | Separate rollout file; identified by `SessionMeta.parent_thread_id` |
| **Slugification** | `/[^a-zA-Z0-9]/g -> -`; $>200$ chars capped + path hash; overridable via `CLAUDE_CODE_PROJECT_DIR_NAME` | No slugification; nested by UTC date `YYYY/MM/DD` |
| **Archived Sessions**| Manual purge ([`claude purge`](https://code.claude.com/docs/en/claude-directory#clear-local-data)) | [`$CODEX_HOME/archived_sessions/rollout-*.jsonl[.zst]`](https://github.com/openai/codex/blob/main/codex-rs/rollout/src/lib.rs#L87) (flat directory) |
| **Retention** | Default 30-day cleanup policy ([`cleanupPeriodDays`](https://code.claude.com/docs/en/settings-reference#cleanupperioddays)); configurable, and writes can be disabled | Discover both active and archived retained rollouts; deleted files are not recoverable by this importer |
| **Ephemeral Opt-out**| [`--no-session-persistence`](https://code.claude.com/docs/en/cli-reference) / `CLAUDE_CODE_SKIP_PROMPT_HISTORY` | Ephemeral test flags or custom `CODEX_HOME=/tmp/...` |

---

## 3. Call Identity, Deduplication, & Normalization

### 3.1 Claude Code Identity, Streaming, & Placeholder Caveats
- **Identity Hierarchy:**
  - *Primary:* `entry.message.id` (Anthropic API `msg_...`). Stable across streaming blocks and copied across forks.
  - *Secondary Fallback:* `entry.requestId` (server `request-id` header). Present on completions when headers are retained.
  - *Scoped Fallback Only:* `entry.uuid`. Local line identifier within one session; **cannot** dedup across forks because `forkSession` assigns fresh random UUIDs (`Cd()`) to every copied line.
- **Streaming Blocks:** Coalesce duplicate response representations by canonical identity before counting. Do not sum each assistant line. A non-null stop reason or maximum field value is not proof that a placeholder output count has become final.
- **Output Completeness:** [Official SDK documentation](https://code.claude.com/docs/en/agent-sdk/cost-tracking#read-output-tokens-from-the-result-message) explicitly warns that per-step assistant `output_tokens` is a placeholder and final output is reported separately. This warning must be checked against the actual saved transcript formats; it is not evidence that a hypothetical terminal assistant line repairs the count.
- **Saved Summaries:** [Current cost-tracking documentation](https://code.claude.com/docs/en/agent-sdk/cost-tracking#accumulate-costs-across-multiple-calls) says session totals are saved on normal exit and restored on resume/fork. These are cumulative estimates; never add every restored summary. A summary may recover session totals without recovering per-call or daily attribution. Crashes and older versions may lack usable summaries.
- **Subagents:** Discover subagent transcripts as well as main sessions. Do not count a parent tool invocation as an additional copy of its subagent's model usage.
- **Compaction:** Scan retained history rather than only the conversation graph loaded for resume. Validate coverage of compaction/helper model calls separately; do not advertise complete account usage from visible assistant entries alone.
- **Disjoint Math:** Anthropic `input_tokens` is already exclusive.
  Map `input_tokens`, `cache_read_input_tokens`, and `cache_creation_input_tokens` into the corresponding disjoint fields. Map output only when final recorded output is established; missing output is not a genuine zero.

### 3.2 Codex CLI Two-Tier Rollout Accounting
Commit [`53885816`](https://github.com/openai/codex/commit/53885816) introduced `RolloutItem::TokenUsageRecord` on 2026-10-14. That source date is not a verified minimum released CLI version. Detect records and capabilities, not just file dates: a resumed rollout can contain an older prefix and newer records.

- **Tier 1: Observed Response Records (`TokenUsageRecord`):**
  - Non-cumulative usage observed for a completed response ([pinned protocol definition](https://github.com/openai/codex/blob/6562117a81a9457c9d67a4d15b884bf80e7805f7/codex-rs/protocol/src/protocol.rs#L2499)).
  - Deduplicate by canonical server `payload.response_id`; consume its `usage`, not cumulative turn/thread totals or embedded checkpoint copies.
  - Responses lacking usage metadata are omitted upstream ([`token_usage_rollout.rs:61-70`](https://github.com/openai/codex/blob/main/codex-rs/core/tests/suite/token_usage_rollout.rs#L61)).
  - Forks: Parent `TokenUsageRecord`s are cleanly stripped from child rollouts ([`core/src/agent/history.rs:83-88`](https://github.com/openai/codex/blob/main/codex-rs/core/src/agent/history.rs#L83)).
- **Tier 2: Estimated Legacy Tier (Rollouts with `EventMsg::TokenCount` only):**
  - Delta accounting across `total_token_usage` is a **candidate heuristic subject to known uncertainty**, not a proven exact ledger.
  - *Context Window Overflow:* [`fill_to_context_window`](https://github.com/openai/codex/blob/6562117a81a9457c9d67a4d15b884bf80e7805f7/codex-rs/protocol/src/protocol.rs#L2552) can set total usage to context capacity while zeroing category counts. This is not completed-call usage. Recognizing this shape is a diagnostic, not a complete legacy-accounting algorithm.
  - *Repeated Snapshots and Resets:* Ignore unchanged snapshots; examine category deltas only inside validated accumulation intervals. Counter drops, synthetic context fills, model changes, and missing baselines require explicit handling, not silently clamping negative deltas.
  - *Forks and Mixed Formats:* Establish inherited-prefix ownership from verified lineage and record semantics. Do not drop all child sessions, which loses genuine subagents. Stitch legacy prefixes to modern records only with validated non-overlap; neither counting both streams nor discarding the whole legacy prefix is safe.
  - *Call Count Dilemma:* Call granularity is unobservable in `TokenCount`. Fabricating a count of 1 per delta or counting snapshots is fake precision. Setting 0 corrupts tokens/call metrics. *Recommendation:* Support nullable `call_count` or quality metadata in backend schema for legacy imports.
- **Disjoint Math:** In Responses API, `input_tokens` includes cached and cache-write ([`responses.rs:870-896`](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs#L870) test fixture).
  $\text{input} = \text{raw\_input} - \text{cached\_input\_tokens} - \text{cache\_write\_input\_tokens}$.
  Precondition $\text{raw\_input} \ge \text{cached} + \text{cache\_write}$ must hold; anomalous records must be flagged. Reasoning is a subset of output; do not add again.

---

## 4. OTel Live vs. Local Transcript Correlation

| Attribute / Key | Claude Code OTel ([`api_request`](https://code.claude.com/docs/en/monitoring-usage#api-request-event)) | Claude Code Transcript | Codex CLI OTel ([`sse_event`](https://learn.chatgpt.com/docs/config-file/config-advanced#observability-and-telemetry)) | Codex CLI Rollout (`TokenUsageRecord`) |
| :--- | :--- | :--- | :--- | :--- |
| **Call Identity** | `request_id` (conditional) | `message.id` (`msg_...`), `requestId` | **None** (dropped in `client.rs`) | `response_id` (`resp_...`) |
| **Frame / Line** | None (`message.uuid` on text only) | `uuid` (line UUID) | None | `ordinal` |
| **Turn / Thread** | `prompt.id` (UUID) | `parentUuid` chain | `conversation.id` (thread UUID) | `turn_id`, `thread_id` |
| **Coverage validation** | Verify supported providers/versions | Final output and helper-call coverage need verification | Inspected WebSocket telemetry path lacks the SSE completion signal | Only usage actually recorded can be recovered |

**Correlation Reality:** Claude Code `api_request.request_id` can join `transcript.requestId` when both are present, but correlation is not uniformly guaranteed across third-party/Bedrock proxy configs or connection aborts. Codex drops `response_id` from OTel logs entirely. A dual OTel/transcript pipeline risks live/history double-counting.

---

## 5. Backend Ingestion Seam & Safe Reimport Architecture

1. **Daily History Contract ([`/functions/v1/history`](../supabase/functions/history/index.ts)):** Accepts UTC daily aggregates (`day`, `provider`, `model`, `is_subagent`, `call_count`, 4 token fields, `cost_usd`). Conflict key is `(user_id, device_id, day, provider, model, is_subagent)`. No event-level deduplication exists.
2. **`UNION ALL` Double-Counting:** Leaderboard views compute `public.usage_daily UNION ALL public.usage_history` into `public.usage_daily_all` ([`20261005120000_history_and_retention.sql:145-154`](../supabase/migrations/20261005120000_history_and_retention.sql#L145)). If a call is reported live and also imported in history, it is doubled.
   - *Cutoff Rule:* Fixed `activation_cutoff` per source installation. History import strictly filters $\text{timestamp} < \text{cutoff}$. Live collection strictly processes $\text{timestamp} \ge \text{cutoff}$.
3. **The Full-Reset Pruning Trap:** `reset=true` deletes all history for `(user, device)`. If transcripts age out under retention sweeps (Claude 30d), a naive disk re-scan will permanently erase older historical days from the server.
   - *Requirement:* The client must maintain a durable local metadata ledger of previously accepted contributions by canonical call ID, preserving historical totals independently of ephemeral disk files.
4. **7-Day Ingest Window vs. Outage Gap:** Live ingest rejects events older than 7 days. An extended collector outage cannot be healed simply by advancing the history cutoff without double-counting days that had partial live events.
5. **Cross-Machine Duplication:** Copying the same history to another installation can double-count it under a new `device_id`. A local ledger and stable import identity make same-source reruns idempotent; they do not provide global deduplication across independent machines. That guarantee requires server-side canonical contribution identities or an explicit single-owner import policy.

---

## 6. Upstream Test Evidence & Verification Status

1. **Codex Upstream Test ([`codex-rs/core/tests/suite/token_usage_rollout.rs`](https://github.com/openai/codex/blob/main/codex-rs/core/tests/suite/token_usage_rollout.rs#L32)):**
   - Test `observed_response_usage_accumulates_per_turn_and_thread` mounts completed responses A=120, B=80, C=30, and response-without-usage.
   - Asserts `records.len() == 3` (response without usage omitted), Turn 1 accumulates $120 \to 200$, Turn 2 response C adds 30 accumulating thread tokens to 230. Proves `TokenUsageRecord` records only observed completed responses.
2. **Local Runtime Probe Executed During This Review:**
   - Imported the existing `buildHistory` function and supplied explicitly synthetic normalized calls: one call produced 150 tokens; duplicating the input produced 300 tokens and two calls. A call exactly at the cutoff was excluded, and an empty/pruned source produced no rows.
   - This demonstrates that deduplication and retention-safe snapshot construction must happen before the current aggregator. It is not a Claude/Codex parser test or deployed database exercise. The upstream Rust integration test was read, not executed; no actual Claude/Codex session was run.

---

## 7. Recommended Architectural Decisions & Release Gates

### 7.1 Recommended Architecture Decisions
1. **Use a Shared Local Parsing Module:** Separate Claude and Codex adapters, one durable normalized ledger, historical scan plus incremental scan. Final Claude output recovery remains a release gate, so this is not a promise of complete transcript-only usage.
2. **Support Both Codex Format Families Honestly:** Prefer observed response records where present. Offer explicitly labeled legacy reconstruction only for recognized, validated intervals; preview uncertain/skipped ranges. Preserve unknown call counts rather than inventing calls. Handle mixed-format files explicitly.
3. **Preserve Unknown and Estimated Values:** Extend storage/display semantics for unavailable output, cost, or call count where required. Do not disguise missing values as zero, or allocate session-wide totals to days/models without evidence.
4. **Keep Imports Repeatable and Non-Overlapping:** Use source-isolated stable import identity, an immutable activation cutoff, and retained accepted metadata. Deduplicate before UTC aggregation. Preserve original event timestamps so late-flushed pre-cutoff calls remain historical.

### 7.2 Candidate Release Gates

| Gate | Requirement | Status |
| :--- | :--- | :--- |
| **Token normalization** | Disjoint categories and conservation checks against recorded usage. | Source/test evidence exists; validate each supported format/provider. |
| **Claude historical output** | Establish final output availability and attribution in actual retained transcript fixtures. | Unresolved; assistant placeholders cannot be treated as final counts. |
| **Forks, subagents, and mixed Codex formats** | Count newly incurred usage once without counting inherited prefixes. | Implemented for observed Codex responses: inherited prefixes are excluded and cumulative counters are not imported, including in mixed-format rollouts. |
| **Legacy Codex quality** | Distinguish reconstructed totals, unknown call count, ambiguous ranges, and missing metadata. | Open; Munch does not reconstruct usage from legacy cumulative counters. |
| **Retention-safe reimport** | Preserve accepted contributions when upstream files disappear; rerunning one import does not add again. | Resolved for Codex by the append-only server ledger: absent records are not removed and existing response identities are skipped. |
| **Upload replacement** | Do not expose a partial replacement after an interrupted multi-chunk import. | Resolved for Codex: chunks are staged and published in a single commit; backfill appends rather than replacing accepted rows. |
| **Cross-machine ownership** | Do not count a copied archive twice. | Mitigated for observed Codex contributions: the server deduplicates response IDs per account and harness across source devices. This does not deduplicate proxy ingest. |

## 8. Munch issue #4: observed Codex import

- **What a rollout proves.** The pinned Codex [rollout writer](https://github.com/openai/codex/blob/6562117a81a9457c9d67a4d15b884bf80e7805f7/codex-rs/rollout/src/recorder.rs#L2128-L2155) stamps each line with UTC milliseconds at write time. [`TokenUsageRecord`](https://github.com/openai/codex/blob/6562117a81a9457c9d67a4d15b884bf80e7805f7/codex-rs/protocol/src/protocol.rs#L2479-L2518) supplies the response ID and per-response `usage`, but no per-response model, provider, cost, or separate timestamp. Model and provider are rollout context, not billing metadata. Cache-write omitted by the recorded schema means zero; an absent required counter does not. Use only top-level observed records; a cumulative `token_count` line or a copy embedded in a compaction checkpoint is not another response.
- **What a window proves.** [Munch's managed Codex provider and manual API-key routes](../proxy/README.md#4-codex-chatgpt-sign-in) are both possible paths to proxy ingest. [Live event identity](../proxy/internal/proxy/server.go) is not reconciled with the contributions ledger, and neither a first retained event nor a config file's modification time proves when a particular Codex home could first reach the proxy. Require an independently verified, source-specific `[from, before)` across all included files and all proxy routes; no inferred cutoff or post-cutover recovery. Use the earliest plausible proxy use as the conservative upper bound.
- **What the desktop collector proves.** [Munch's desktop collector](../proxy/README.md#desktop-accounting-pilot) and backfill share Codex response IDs in the contributions ledger, so the same call can't be counted twice under different identities. Backfill excludes records the collector owns: those at or after the current activation, and any ID in its ledger. If a live delivery still repeats an ID a backfill source already accepted (for example from another machine), the server keeps the backfill row and acknowledges the delivery without raising an ownership conflict, so the collector doesn't stall.
- **What the retired ledger proves.** The [former Codex collector](https://github.com/hndrxc/tokenmunchers/blob/3aaae3659811a7cf6e768600d46a62e750a59cf7/integrations/codex/src/parser.ts) used the raw response ID for observed calls, but a distinct `legacy:` hash for reconstructed intervals. The [retained SQL ledger](../supabase/migrations/20261028000000_collector_contributions.sql) keys identities by account and harness, not just source UUID. An append-only import can skip accepted response identities without overwriting their ownership, metadata or history/live partition. A reconstructed interval cannot prove response equivalence, so the importer conservatively withholds candidates on a UTC day already containing active Codex reconstruction for the account.
- **Why not use TokenHub totals.** [TokenHub's pinned importer](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L70-L102) is useful for understanding legacy JSONL and cumulative deltas. Its [first-live cutoff and rebuild path](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L119-L158) do not establish per-source non-overlap or preserve accepted observations when rollouts disappear. Munch uses neither SQLite totals nor that reset strategy.

## 9. Codex Desktop compatibility check

**App Server and signed-in Windows desktop app verified.** [OpenAI documents](https://learn.chatgpt.com/docs/windows/windows-app#share-config-auth-and-sessions-with-wsl) that the Windows desktop app shares the native CLI's Codex home. WSL uses a different home unless configured to share it. OpenAI [also documents CLI session reuse in the macOS app](https://openai.com/index/introducing-the-codex-app/) and a [Linux desktop preview](https://learn.chatgpt.com/docs/linux/linux-app.md). A shared home establishes where to look, not which records a given desktop build writes.

On Linux, a test JSON-RPC client drove one local turn through the installed Codex App Server (`0.171.0`) with a synthetic Responses endpoint in a loopback-only network namespace. Both persisted history modes (`paginated`, the observed default, and explicit `legacy`) wrote one top-level `token_usage_record` and one cumulative `token_count` event. The built `munch backfill` preview reported one eligible response with its original UTC timestamp, 65 input, 20 output, 30 cache-read and 5 cache-write tokens; the cumulative event was excluded. No account, transcript upload, or external inference was used.

The [App Server initialization code](https://github.com/openai/codex/blob/6562117a81a9457c9d67a4d15b884bf80e7805f7/codex-rs/app-server/src/request_processors/initialize_processor.rs) accepts a client-supplied originator; a desktop-looking label is not proof of desktop provenance. The desktop check therefore drove the app's own UI rather than trusting existing rollouts labeled `Codex Desktop`.

On Windows 11 (`10.0.26200.9974`), one disposable local turn (**This computer**, no project, no tools) in the signed-in Codex desktop app (package `OpenAI.Codex` `26.1111.9664.0`, embedded Codex `0.169.1`) wrote one `paginated` rollout in the default Codex home, with originator `Codex Desktop` and model provider `openai`. That Codex home has no Munch provider configured. The rollout held one top-level `token_usage_record` and one cumulative `token_count` event. `munch backfill`, built from this branch and run natively on Windows over `[2026-11-22T08:11:24.524Z, 2026-11-22T08:11:53.047Z)`, reported one eligible response at its recorded `08:11:29.807Z`: 7,361 input, 20 output, 12,800 cache-read and 0 cache-write tokens (raw input 20,161 minus cached; 13 reasoning tokens not added), total 20,181. The `token_count` event was excluded as legacy. A window ending exactly at the record's timestamp excluded it; one starting there included it. A no-window preview of the whole home (68 rollouts, no archive directory) completed without error. Nothing was uploaded. Cloud tasks and ordinary ChatGPT chats remain outside this check.
