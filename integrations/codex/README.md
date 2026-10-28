# Codex CLI rollout adapter

Read-only, dependency-free source adapter for the [shared collector](../README.md). It reads only the explicitly selected Codex home; it never falls back to `$CODEX_HOME`/`~/.codex` by itself, never reads credentials, `auth.json`, SQLite state or config, makes no network calls, and never modifies rollouts. Runtime: Node.js 26 or newer with native TypeScript stripping and built-in `node:zlib` Zstandard.

The original [source plan](PLAN.md) and [accounting analysis](../../docs/claude-codex-backfill.md) remain historical design documents. This README describes the implemented interface and its evidence limits.

## Interface and discovery

```ts
import { scan } from './src/adapter.ts';

const result = await scan(selectedCodexHome, {
  maxBytes: 256 * 1024 * 1024,      // decompressed bytes per scan (compressed source bytes are bounded too)
  maxLineBytes: 8 * 1024 * 1024,    // one JSONL record
  exclude: ['archived_sessions/2025'],
  continuation: { nextFile: 'sessions/2026/10/27/rollout-….jsonl' },
});
```

`scan(root, options?: ScanOptions): Promise<CodexScanResult>` satisfies the shared `Adapter`. Besides `observations` and `diagnostics`, the result carries local-only scan metadata: `progress` (`files`, decompressed `bytesRead`, compressed `sourceBytesRead`, `deferred`), per-file `cursors`, per-file format `segments`, `continuation.nextFile`, `deferredFiles`, `supersededCanonicalIds` and `conflictingCanonicalIds`. Paths are selected-root-relative and never become outbound usage fields.

| Module | Plan phase |
| --- | --- |
| `src/reader.ts` `discover` | 7. Root discovery: `sessions/**/rollout-*.jsonl[.zst]` (nested `YYYY/MM/DD`) and flat `archived_sessions/rollout-*.jsonl[.zst]`, sorted, `lstat` only, symlinks (including the selected root) refused, root-relative prefix exclusions. Nothing else under the home is opened. |
| `src/reader.ts` `readRows` | 1. Streaming reader: 64 KiB chunks, streaming `createZstdDecompress` (window log capped at 27, concatenated frames supported), only newline-committed strict UTF-8 JSON accepted; partial trailing/unterminated rows and truncated frames are deferred, oversized/malformed rows are diagnosed without content. |
| `src/parser.ts` `RolloutParser` | 2. Context, 4. modern parser, 5. legacy evaluator, 6. in-file stitching. |
| `src/token-normalizer.ts` | 3. Disjoint normalizer. |
| `src/adapter.ts` | 6. Cross-file stitching, global `response_id` deduplication, 8. collector-facing result. |

## Accounting rules

- **Disjoint math.** Wire input is inclusive: `input = input_tokens − cached_input_tokens − cache_write_input_tokens`, `cache_read = cached_input_tokens`, `cache_write = cache_write_input_tokens`, `output = output_tokens` (reasoning is a subset and never re-added). An absent `cache_write_input_tokens` is serde's default zero; `null` is malformed. Preconditions are enforced, never clamped: `MALFORMED_CACHE_SUBSET` (cache subsets exceed input), `MALFORMED_REASONING_SUBSET`, `MALFORMED_TOTAL` (`input + output ≠ total`), `MALFORMED_USAGE` (missing/null/negative/unsafe). No `max(0)` exists anywhere; failing records are withheld and reset legacy baselines.
- **Modern (`token_usage_record`) → `request` grain.** Canonical identity is `payload.response_id` (`identity_strength: 'response'`), `call_count: 1`, `quality: 'observed'`. Only per-response `payload.usage` is counted; `turn_token_usage`/`thread_token_usage` are cumulative and serve solely as continuity evidence. Responses without usage are never written upstream and emit nothing. Identical copies across files coalesce (`DUPLICATE_RESPONSE`); copies with different accounting are withheld entirely and listed in `conflictingCanonicalIds` (`CONFLICTING_RESPONSE`).
- **Legacy (`event_msg` `token_count`) → `reconstructed_interval` grain.** Only a recognized append counts: the cumulative delta must be non-negative in every category, equal `last_token_usage` exactly, and stay inside one model/provider/session/time-ordered segment. Intervals carry `call_count: null`, `quality: 'reconstructed'`, a session-scoped hash identity (`SCOPED_SINGLE_OWNER`) and explicit `interval_start`/`interval_end`. The first snapshot is a baseline only; repeated snapshots add nothing; non-monotonic counters (`COUNTER_RESET`), `compacted` checkpoints (`COMPACTION_BOUNDARY`, embedded records ignored), model/provider/session changes and malformed records reset the baseline without negative tokens. The synthetic `fill_to_context_window` line (total = context window, zero categories) is dropped (`SYNTHETIC_OVERFLOW`).
- **Segments.** Capability is detected per record, not per file. A modern record opens a `modern` segment; real 0.154.0 rollouts also mirror each response's thread cumulative as a `token_count`, which stays in the modern segment (`REPEATED_SNAPSHOT`). Only an unexplained snapshot opens a `legacy` segment. Transitions are reported as `SEGMENT_MODERN`/`SEGMENT_LEGACY` diagnostics so collector preview shows format capability per file.
- **Mixed stitching.** Pending legacy intervals are released at the next modern record only if some retained snapshot equals `thread_token_usage − usage` exactly and the interval ends at or before it (`STITCH_CONFIRMED`). Otherwise they are withheld and listed in `supersededCanonicalIds` (`UNCERTAIN_TRANSITION`, `MISSING_STITCH_EVIDENCE`); the modern response is still counted. Across files, a retained legacy interval must be connected before or after every other-file modern response of its session by an exact cumulative chain, otherwise it is superseded (`UNCERTAIN_CROSS_FILE_TRANSITION`). Numeric ordering alone is never accepted as proof.
- **Lineage and fork prefixes.** `session_meta.id` is the rollout's own thread. `parent_thread_id`, `forked_from_id` and `source.subagent|sub_agent.thread_spawn.parent_thread_id` populate `parent_session_id`; `is_subagent` is `true` for parent/spawn lineage, `false` for a string source (including ordinary forks), else `null`. Forked/spawned rollouts replay the parent's history first, including the parent's `session_meta`; a replayed lineage `session_meta` or any row below `subagent_history_start_ordinal` never replaces the child context (`INHERITED_CONTEXT`). Accounting rows below the boundary are excluded (`INHERITED_PREFIX`); rows without an ordinal under a boundary are refused (`MISSING_OWNERSHIP_ORDINAL`). Modern records whose `thread_id` is another thread are excluded (`INHERITED_RESPONSE`). Legacy intervals in a child/fork without an explicit boundary are refused (`UNOWNED_FORK_INTERVAL`), while genuinely new child-local modern responses still count.
- **Provider, model, cost.** Provider comes only from explicit `session_meta.model_provider`, else `"unknown"` (`UNKNOWN_PROVIDER`), never inferred as `"openai"`. Model comes from the owned `turn_context.model`, else `"unknown"`. Rollouts contain no prices: `cost_usd: null`, `cost_basis: 'unknown'`, `UNPRICED`. Unparseable timestamps become `null` (`MISSING_TIMESTAMP`).

## Bounds and scan lifecycle

`maxBytes` limits decompressed bytes and, for `.zst`, compressed source bytes; expansion cannot bypass it. When reached, remaining files are listed in `deferredFiles`, `continuation.nextFile` names where to resume, and `SCAN_TRUNCATED` is reported. Only a budget stop sets the continuation: a file that alone exceeds a fresh budget is reported as `SCAN_BOUND_TOO_SMALL`, stays in `deferredFiles`, and the continuation moves past it. Partial trailing lines, corrupt archives and unreadable files are listed in `deferredFiles` with their diagnostics and retried on the next full pass; they never pin the continuation, so every other rollout keeps being scanned. A stale continuation returns `STALE_CONTINUATION` without observations. Invalid limits throw `RangeError`. The adapter performs a full bounded pass each time; the collector's durable canonical ledger supplies replay idempotence, retention safety and copied-archive ownership. Read failures are visible (`READ_FAILED`, `DECOMPRESSION_FAILED`, `DISCOVERY_FAILED`, `INVALID_ROOT`) and are never deletion instructions.

## Fixture provenance

[test/fixtures/manifest.json](test/fixtures/manifest.json) records provenance, coverage and limits for every fixture. `modern-source-derived.jsonl`, `legacy-synthetic.jsonl` and `builders.ts` are synthetic. The two `codex-0.154.0-real-*` files are real, metadata-only redactions from a parent-created isolated `CODEX_HOME` running the installed Codex CLI 0.154.0, produced by the jq allowlist recorded in the manifest (session/turn identifiers, model, provider, ordinals, lineage and usage only; no prompts, tool content, paths, instructions or rate limits):

- `codex-0.154.0-real-metadata.jsonl`: parent rollout lines 1–30 (first exec turn plus first resume). Three responses sum to **43952** (input 43889, cached 40960, output 63). The resume stdout `turn.completed` usage is thread-cumulative and equals, not adds to, this sum.
- `codex-0.154.0-real-subagent-metadata.jsonl`: the whole child rollout spawned during the final attempt. It replays the parent's `session_meta` at ordinal 1, uses `subagent_history_start_ordinal: 21` and spells the source `subagent`. Its one own response (ordinal 30) is input 16440, output 9, total 16449.

## Scenario map

Tests are in [test/accounting.test.ts](test/accounting.test.ts) (A) and [test/reader.test.ts](test/reader.test.ts) (R). They are gate commands for the parent, not claims of passing runs.

| Plan scenario / requirement | Regression |
| --- | --- |
| Modern multi-response A=120 B=80 C=30, no-usage response emits nothing, `response_id` dedupe | A `modern source-derived A=120 B=80 C=30…`; A `unknown timestamp is null, zero usage…`; A `strong identities deduplicate…`; R `chosen-root discovery…` |
| Cache-write conversion 100/40/60/10 → 0/40/60/10 = 110 | A `cache-write is disjoint, reasoning is a subset…`; A `legacy fixture discards 200000…` |
| Synthetic overflow discard | A `legacy fixture discards 200000 context fill…` |
| Legacy recognized interval, `call_count: null`, compaction/counter resets | A `legacy fixture…`; A `legacy repeats and counter drops reset safely…`; A `unknown initial baseline, mismatched last delta, model changes and compaction…` |
| Mixed-segment stitching, skip unprovable overlap | A `mixed legacy/modern exact cumulative boundary…`; A `legacy snapshot already containing modern response…`; A `transition with absent or invalid cumulative evidence…`; A `a repeated modern record cannot rewind continuity…`; A `stitching never reuses pre-reset cumulative evidence…` |
| Malformed precondition (`MALFORMED_CACHE_SUBSET`, no clamping) | A `malformed cache subsets, totals, reasoning, null and unsafe counts are rejected…` |
| Unset provider stays `unknown` | A `modern source-derived…`; A `subagent lineage uses explicit source metadata…` |
| Fork-prefix ownership, subagent lineage | A `fork prefix ownership skips legacy…`; A `explicit child-local ordinal boundary…`; A `parent-only and source-spawn lineage…`; A `ordinary forks are not fabricated as subagents…`; A `replayed parent session_meta in a fork…`; A `redacted real Codex 0.154.0 subagent rollout…`; R `redacted real 0.154.0 parent and subagent rollouts scan once…` |
| Real 0.154.0 reconciliation (43952; cumulative not counted as calls) | A `redacted real Codex 0.154.0 resumed metadata reconciles three responses…` |
| Active/archived discovery, plain and Zstandard, exclusions, symlinks | R `chosen-root discovery…`; R `root-relative exclusions and symlink refusal…` |
| Interrupted line / partial append | R `incomplete trailing record is deferred…`; R `valid but unterminated final JSON…`; R `concatenated zstd frames…` |
| Bounded reads, continuation, compressed expansion, malformed/oversized rows, UTF-8 chunking | R `scan budgets expose continuation…`; R `oversized first rollout cannot pin continuation…`; R `permanent read failure never pins continuation…`; R `compressed expansion consumes the decompressed byte budget…`; R `malformed and oversized complete records…`; R `chunk boundaries preserve UTF-8…`; R `unavailable roots, stale continuations and invalid limits…` |

Replay idempotence across runs, cleanup retention, original-timestamp cutoff, pause/exclusion persistence, copied-archive ownership and chunked delivery are collector/backend responsibilities covered by the shared and collector suites.

## Gates and remaining evidence gaps

From the repository root with Node.js ≥ 26:

```sh
node --test integrations/codex/test/*.test.ts
npm --prefix integrations test
npm --prefix integrations run typecheck
node --input-type=module -e "const { scan } = await import('./integrations/codex/src/adapter.ts'); const r = await scan('/tmp/tokenmunchers-smoke-EKLNb6/codex'); console.log(r.observations.length, r.observations.reduce((s, o) => s + o.input_tokens + o.output_tokens + o.cache_read_tokens + o.cache_write_tokens, 0), r.observations.filter(o => o.is_subagent).length)"
```

The last command is a runtime probe of the isolated real root; if that root is unchanged since capture, the expected output is `7 110787 1` (six parent responses totalling 94338 plus the child's 16449).

Open limits:

- Only Codex CLI 0.154.0 modern rollouts were captured, on one machine and one provider/model. No real legacy-only, mixed legacy/modern, compacted, cache-write, reasoning or `.zst` rollout has been captured; those paths are synthetic/source-derived only.
- The real child evidence is one depth-1 spawn with one response. Its existence is proven by the on-disk rollout; the parent's exec stdout showed only a collab wait with empty `receiver_thread_ids`. Complete child coverage, nested spawns and inherited modern records below the boundary are unverified against real data.
- Rollouts record observed completed responses with usage, not billing. Responses Codex never recorded cannot be recovered; exact usage across all historical sessions is not claimed.
