# Claude Code transcript adapter

Read-only, dependency-free source adapter for the [shared collector](../README.md). It reads only the explicitly selected Claude configuration root; it does not choose a home from environment variables, upload records, or change Claude files. Runtime: Node.js 26 or newer with native TypeScript stripping.

The original [source plan](PLAN.md) and [accounting analysis](../../docs/claude-codex-backfill.md) remain historical design documents. This README describes the implemented interface and its evidence limits.

## Interface and discovery

```ts
import { scan } from './src/adapter.ts';

const result = await scan(selectedClaudeRoot, {
  maxBytes: 256 * 1024 * 1024,
  maxLineBytes: 4 * 1024 * 1024,
  exclude: ['projects/private-project'],
});
```

`scan(root, options?: ScanOptions): Promise<ClaudeScanResult>` satisfies the shared `Adapter` interface. `ClaudeScanOptions` aliases shared `ScanOptions`; `ClaudeCursor` is the non-null shared cursor type. The collector persists scan results atomically with accepted metadata.

Discovery enumerates existing `projects/<project>/*.jsonl` and `<session>/subagents/agent-*.jsonl`, without reconstructing project slugs or following discovered symlink files/directories. Legacy project-level `agent-*.jsonl` files are marked subagents, but an unavailable parent remains null. Nested subagents link `parent_session_id` to the containing session and use `<parent>/agent-<id>` for their local session identity. Exclusions are selected-root-relative path prefixes, not globs. No other configuration homes are searched.

Results contain normalized `observations`, local `diagnostics`, `identityAliases`, selected-root-relative `discoveredFiles`/`deferredFiles`, `projects`, `progress`, and `cursor`. Paths, offsets, and diagnostic messages are local scan metadata, not outbound usage. The shared collector owns the outbound allowlist.

## Identity, usage, and uncertainty

- Strong identity is `message:<message.id>`, with no format regex or session/path prefix. Forked prefixes and progressive assistant frames coalesce rather than add.
- Fallback identity is `request:<requestId>`. When a response ID becomes available, `identityAliases: [{ from, to }]` tells the durable ledger to merge/rekey the earlier request. Aliases survive pages, restarts, and later fallback-only copies. When one request names multiple distinct response IDs, the first response seen in the scan owns the alias; later response IDs stay separate, emit no alias, and carry `REQUEST_ID_CONFLICT`. A page or rescan that cannot see the earlier mapping may still propose a different alias; the ledger skips it with a durable `IDENTITY_ALIAS_CONFLICT` diagnostic, and both responses stay separate.
- UUID-only identity is a SHA-256 digest scoped to the selected-root-relative transcript path. It carries `SCOPED_IDENTITY_FORK_UNCERTAIN`: copied/forked UUID-only history cannot be deduplicated globally. Renaming or moving weak-identity transcripts can change identity. Do not import copied weak-identity archives under multiple owners.
- Anthropic `input_tokens`, `cache_read_input_tokens`, and `cache_creation_input_tokens` are already disjoint. They map directly to input, cache read, and cache write. Missing/invalid categories remain null.
- **Request output is null unless cost-state verifies it.** Neither `stop_reason`, a large output value, nor the maximum across assistant frames proves final saved-transcript output. Requests carry `OUTPUT_PLACEHOLDER_UNVERIFIED`, partial quality, one observed call, and unknown cost, except when the cost-state reconciliation below verifies them. No aggregate is ever allocated back to individual requests.
- **Cost-state output reconciliation.** One scan groups each session's main transcript with its `subagents/` transcripts, because cost-state `modelUsage` covers the main pipeline including subagents. For each model, the adapter compares two values: the latest cost-state `modelUsage` from the main transcript, and the sum of saved input, cache read, cache write, and output over that session's distinct response IDs.
  - **Verified.** When every category matches exactly, each response's output is its saved value. The response gets quality `observed`, loses `OUTPUT_PLACEHOLDER_UNVERIFIED`, and carries `OUTPUT_RECONCILED_WITH_COST_STATE`.
  - **Mismatch.** The response stays null and partial and adds `OUTPUT_RECONCILIATION_MISMATCH`. Causes include:
    - no cost-state, for example after a crash;
    - totals that differ, for example because helper calls are absent from the transcript;
    - frames of one response whose usage differs, i.e. placeholders;
    - a request or UUID fallback identity, or conflicting frame models.
  - **Deferred.** A session counts as fully visible only when every one of its transcripts was read from byte 0 to EOF in this scan. Paged, cursor-resumed, deferred, or unreadable sessions keep output null with `OUTPUT_RECONCILIATION_DEFERRED`.
  - **Durability.** The shared ledger reconciles a canonical ID toward `observed` quality, so a later whole-session scan upgrades earlier partial records and later paged replays cannot downgrade them.
  - **Limits.** A session whose transcripts always straddle a page boundary stays unverified until a scan with a larger `maxBytes` sees it whole. Ledger diagnostics are unioned, so an upgraded record may still list the old placeholder code alongside the reconciliation code. Request cost stays null and unknown, and cost-state summaries are never added to requests.
- Provider is never inferred from the model, credentials, headers, settings, or endpoint secrets. Explicit recognized nonsecret `provider` metadata is allowlisted (`firstParty` maps to `anthropic`, plus documented cloud/provider labels); absent or unsupported values produce `unknown`. Conflicting frame providers remain unknown. Persisted `cost-state` in the pinned build omits provider even though SDK result `modelUsage` can include it.
- A parent Task/Agent tool dispatch adds no manufactured call or child tokens. Actual parent assistant API usage still counts; child API IDs coalesce normally. Retained assistant usage for helpers is parsed like any other request, but title/summary metadata and compaction boundaries cannot reconstruct unrecorded helper calls. `HELPER_USAGE_UNAVAILABLE`, `COMPACTION_COVERAGE_UNVERIFIED`, and `HELPER_COVERAGE_INCOMPLETE` expose that limit.

## Summaries and scan lifecycle

Pinned `cost-state.modelUsage` and SDK `result.modelUsage` are cumulative, nonadditive `session_summary` observations. Later snapshots replace earlier snapshots for the same source/session/model identity within a scan; replay never adds them. Summary output is known at that aggregate grain; request output is known only through the exact reconciliation above. Numeric spend is an estimate, not an invoice; unavailable spend and call counts remain null. Pricing uncertainty is diagnosed. Summary timestamps remain null, with only explicit interval bounds preserved. Token counts above 1e9 or spend above 10000 USD exceed the shared metadata bounds; only those fields become null, with `SUMMARY_VALUE_OUT_OF_RANGE` (request token counts above the bound become null with `MALFORMED_TOKEN_COUNT`), so the remaining fields of the summary or request are still recorded rather than the whole observation being rejected.

Cost-state and SDK-result summaries retain separate source identities and carry `SUMMARY_OVERLAP_UNRESOLVED`; they must not be summed together or with requests. The shared daily uploader withholds session summaries. SDK `result.usage` is main-loop/per-turn usage, not the cumulative model ledger, so it is deliberately not substituted for missing `modelUsage`. Crash `error_during_execution` results can be zeroed and are withheld; ordinary assistant rows still work without an exit summary. These shape distinctions are source-derived, not evidence that every SDK result is saved in a normal project transcript.

The reader consumes complete newline-terminated UTF-8 JSON objects. Malformed complete rows and oversized records are diagnosed; a partial trailing row is deferred, never parsed as complete. Positive safe-integer scan/line limits bound reads. Byte-limited scans return `{ file, offset, line, device, inode, discardLine? }`; pass that cursor to the next scan. `discardLine` avoids reinterpreting an oversized record's tail. A single non-oversized record that cannot fit the scan budget reports `SCAN_BOUND_TOO_SMALL`; raise `maxBytes` to advance without persisting raw content. A missing cursor file restarts canonical scanning; inode/device changes or truncation reset an in-file cursor. Once continuation ends, later collection starts another bounded full pass to pick up appended rows.

Partial-at-EOF results may have `cursor: null` but `progress.deferred: true`; the next full pass retries the row. Permission/read/root failures are visible diagnostics, not instructions to delete accepted history. Retention cleanup produces no deletion or replacement directive: the collector's durable normalized ledger, not currently retained files, preserves previously accepted usage.

## Fixture provenance and real-session evidence

Synthetic fixtures, redacted real captures, and test-generated records are cataloged with exact provenance and limits in [test/fixtures/MANIFEST.md](test/fixtures/MANIFEST.md). Schema evidence comes from the already captured `@anthropic-ai/claude-agent-sdk@0.3.312` and native package, embedded CLI 2.1.312 / commit `f375cd85d4f70920de17ce3a4ad646e18834e833`.

Two consented, isolated real sessions are checked in as metadata-only projections (`test/fixtures/real-2.1.312-*.jsonl`), captured with the recorded allowlist [test/fixtures/redact-real.jq](test/fixtures/redact-real.jq). They were produced by Claude Code 2.1.312 (the CLI auto-updated from 2.1.263 earlier in the session; see the manifest). In them, saved per-message output equals the final value, repeated frames of a multi-block response carry identical usage, and per-model sums over distinct message IDs equal the latest cost-state exactly; the SDK result output totals also matched.

This evidence is scoped to one producer build, `claude-haiku-5-5` only, and first-party default authentication. It has no subagent, no compaction, no thinking or server tools, and covers two short sessions. Output verification therefore relies on exact per-session equality, never on the version, and anything outside that equality stays unverified.

## Acceptance mapping and parent-run gates

All source regressions are in [test/adapter.test.ts](test/adapter.test.ts); they have not been executed by this completion worker.

| Plan scenario | Regression evidence |
| --- | --- |
| 1. Streaming | `streaming frames coalesce, exclusive cache math survives, output/provider/cost stay unknown`; request fallback upgrade and cross-page alias tests |
| 2. Fork/new child work | `fork copied prefix does not inflate and genuinely new work is retained`; UUID fallback uncertainty test |
| 3. Subagents | `subagent work is linked once and parent dispatch adds no usage`; `actual parent assistant API work is separate from its child rather than discarded` |
| 4. Unverified output | `real 2.1.312 sessions reconcile saved response output exactly with cost-state without allocating cost`; `paged sessions stay unverified until a whole-session scan upgrades the durable ledger`; `placeholder frames with differing output and absent helper calls never reconcile`; `a real session without its cost-state keeps response output unverified`; `subagent responses join their parent cost-state and excluded subagents block reconciliation`; streaming, crash and invalid/absent category tests |
| 5. Retention | `retention cleanup reports no replacement/delete instruction to its durable ledger caller`; `request aliases coalesce durable pages, replay and cleanup across collector restarts` exercises the real shared ledger |
| 6. Provider neutrality | Streaming/summary tests; explicit-provider conflict test; privacy/authorization test |

Additional regressions cover helper coverage, cumulative resume summaries, known summary output without request repair, missing metadata, malformed UTF-8/JSON, partial append, line limits, bounded cursor continuation, oversized tails, replaced transcripts, exclusions, symlink refusal, metadata privacy, requestIds naming two response IDs, and out-of-range summary/request counts through the real shared ledger. Shared accounting tests cover atomic metadata/cursor rollback, source-isolated pause/cutoff rules, and contribution delivery; those concerns remain collector/backend-owned.

From repository root, the parent should run with Node.js >=26:

```sh
node --test integrations/claude-code/test/*.test.ts
node --test integrations/shared/test/*.test.ts integrations/collector/test/*.test.ts
npm --prefix integrations test
```

These are gate commands, not claims of passing runs. Do not run further paid calls without explicit authorization.
