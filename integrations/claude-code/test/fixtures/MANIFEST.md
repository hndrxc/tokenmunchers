# Claude fixture provenance manifest

## Classification

The five JSONL files listed in the first inventory table are **synthetic, source-derived accounting fixtures**, authored for this implementation. They are not captured conversations, redacted real transcripts, model responses, paid-call evidence, or complete billing ledgers. Session IDs, message/request IDs, line UUIDs, model names, timestamps, tool content, tokens, and dollars are deliberately invented. Test-created temporary JSONL records in `../adapter.test.ts` have the same synthetic classification.

The two `real-2.1.312-*.jsonl` files are **redacted real captures** described under [Redacted real captures](#redacted-real-captures). They are metadata projections of transcripts written by real, consented Claude Code runs, not synthetic data.

The targeted source schema is Claude Agent SDK **0.3.312**, whose package `manifest.json` identifies embedded Claude Code **2.1.312**, commit **f375cd85d4f70920de17ce3a4ad646e18834e833**, build date **2026-10-27T17:16:03Z**. This pins schema provenance, not a claim that the fixtures were produced by that build.

## Fixture inventory

| Path | Invented records and purpose | Source relationship and limits |
| --- | --- | --- |
| `streaming.jsonl` | Two frames of one API message: exclusive input 100, cache read 40, cache creation 60, output placeholders 1 then 999; original timestamp before UTC midnight, later frame after it. | Source-shaped assistant envelopes; `message.id`, `requestId`, `uuid`, `timestamp`, and nested Anthropic usage. Deliberately future-format response ID tests nonrestrictive identity. Terminal stop reason is not proof that 999 is final. No real stream is represented. |
| `fork.jsonl` | Copied parent API message with a fresh UUID plus a new message with exclusive input 7. | Synthetic fork of the streaming fixture; proves response IDs are not prefixed with session/path and genuinely new work is retained. Does not prove fork stability of UUID-only records. |
| `subagent.jsonl` | One child assistant response, input 11, explicit sidechain marker. | Synthetic child is installed by tests at `<parent>/subagents/agent-child-a.jsonl`; discovery, not tool-result text, supplies its parent association. Output 4 is deliberately unverified. |
| `cost-state.jsonl` | Two cumulative saved-state snapshots for one session/model: input 100→107, output 30→35, estimated cost 0.01→0.015, cache read 40 and creation 60. | Field names follow the native build's cost-state serializer/validator. No provider is invented for this saved shape: pinned serializer omits it. Cumulative output is aggregate-only, not request-final proof; neither totals nor cost are evidence of a real session. |
| `dispatch-compaction.jsonl` | User tool result, agent progress, compaction boundary, conversation summary text. No assistant API usage. | Synthetic source-shaped metadata exercises zero manufactured child/parent usage and missing-helper diagnostics. Not a full Agent tool lifecycle or a claim of compaction billing completeness. |

All files are small newline-terminated JSONL. Tests also construct a missing final newline, a mid-record scan bound, oversized content, invalid UTF-8, malformed JSON, replacement files, symlinks, and deleted files to exercise the reader/discovery seam. Those are explicitly synthetic corruption/lifecycle cases, not additional real fixtures.

## Test-generated shapes

`../adapter.test.ts` constructs the following synthetic variants in temporary, explicitly selected roots:

- Request-only rows followed by message-ID rows, including a durable SQLite ledger restart and later fallback-only replay. This exercises the actual collector identity-alias contract, not a simulated ledger.
- UUID-only copies, absent identities, missing timestamps/models/categories, negative/unsafe counts, provider conflicts, and private content/header canaries. Provider values on assistant envelopes are explicit synthetic metadata; their presence is not asserted to be guaranteed by normal Claude transcripts.
- SDK-style `result` rows with cumulative `modelUsage`, `total_cost_usd`, optional explicit model provider, and a separate main-loop `usage` field. These validate the source-derived SDK shape if encountered; they are **not** proof that normal project JSONL persists SDK result rows. The adapter does not substitute main-loop `usage` for cumulative `modelUsage`.
- An `error_during_execution` result with zeroed usage, proving it is not used as a final-output or zero-cost fallback.
- Custom-title metadata plus retained helper assistant rows, distinguishing unavailable auxiliary usage from actual retained API usage.
- Cost-state reconciliation counterexamples: placeholder frames whose output differs (1, then 30) while the last frame alone would match cost-state; a cost-state above the retained responses, standing in for an unrecorded helper call; and a parent plus subagent whose summed usage equals the parent cost-state, also scanned with the subagent excluded. These are synthetic. Real evidence does not yet cover subagent joining.
- A **derived** variant of `real-2.1.312-tool-turn.jsonl` with its cost-state rows removed, standing in for a crash before cost-state was saved. Only the row deletion is synthetic.

No fixture includes a secret, credential, real prompt, private project name, or private source path. The collector persists normalized metadata only; synthetic secret markers in privacy tests must not appear in observations.

## Primary schema evidence

The following already captured packages were inspected; no new package downloads or model calls were required for completion:

1. [SDK npm tarball, version 0.3.312](https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.312.tgz):
   - `package/manifest.json`: embedded version, complete commit, build date. Its Linux-x64 binary checksum is `4f4178ce41d83e23341ebfbaecdbcf8ea6bf7baea0690025abe49eeec3a5d01d` (upstream manifest declaration, not an independently executed integrity gate).
   - `package/sdk.d.ts`, `SDKAssistantMessage`: streaming content blocks may share `message.id`; assistant usage is not final. `parent_tool_use_id` identifies forwarded subagent messages on that SDK surface. SDK envelope `session_id`/`request_id` must not be confused with local transcript `sessionId`/`requestId`.
   - `ModelUsage`: disjoint `inputTokens`, `outputTokens`, cache fields, `costUSD`, optional `provider`, and `costBasis` (`list`, `managed`, `unknown`). Thinking/cache-lifetime details are subsets, not extra additive categories.
   - `SDKResultSuccess`/`SDKResultError`: cumulative estimated `modelUsage` includes pipeline main/subagent/sidechain/internal calls, excludes helpers outside that pipeline, can restore prior session totals, and can reset after clear. `usage` is main-loop-only and per-turn; crash/startup errors may be zeroed. Results therefore cannot be indiscriminately summed or joined back to one assistant response.
2. [Native Linux-x64 npm tarball, version 0.3.312](https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-x64/-/claude-agent-sdk-linux-x64-0.3.312.tgz), embedded `package/claude`:
   - Read-only inspection of the retained native package found `type:"cost-state"` serializer at uncompressed binary byte offset `249382701`, with `sessionId`, `totalCostUSD`, duration/start fields, `modelUsage` token/cost fields, and `hasUnknownModelCost`.
   - The serializer's model usage projection includes no provider. Its validator appears around byte offset `248711592`; restoration maps saved `totalDuration` to `lastDuration` around `249383664`. These offsets are package-specific locating aids, not public interfaces or runtime gates.
   - This establishes a saved aggregate shape in the pinned build, not completeness of actual retained transcripts or per-request final output.
3. Official [cost tracking documentation](https://code.claude.com/docs/en/agent-sdk/cost-tracking), [sessions documentation](https://code.claude.com/docs/en/sessions), and the repository's original [source analysis](../../../../docs/claude-codex-backfill.md) explain placeholder output, cumulative estimates, forks, and retention. The local implementation-specific distinction between saved cost-state and SDK results is retained rather than assuming those surfaces are interchangeable.

## Redacted real captures

| Path | Real session shape | Output sha256 |
| --- | --- | --- |
| `real-2.1.312-resume.jsonl` | Session `7c0ffee0-0000-4000-8000-000000000001`: two single-frame responses (`msg_011CgTr3FQP69RJgD4LAGoRV` output 22, input 2, cache creation 608; `msg_011CgTr3rQorXiUUREsPA7d4` output 21, input 2, cache creation 705, written after a `--resume` into the same file). Two cumulative cost-state rows: input 2→4, output 22→43, cache creation 608→1313, cost 0.0001328→0.0002845. | `4fef1b3670610bc43809b26621a13efb6f57ae039029c254bad6b12b7bc73eb8` |
| `real-2.1.312-tool-turn.jsonl` | Session `7c0ffee0-0000-4000-8000-000000000002`: one tool turn. `msg_011CgTr5z7DfTdiqDSK2jhGr` has two frames (text, then tool_use), both with output 83, input 2, cache creation 1700. `msg_011CgTr68TkxDhWCNnhGC6vN` has output 20, cache read 1700, cache creation 163. Final cost-state: input 4, output 103, cache read 1700, cache creation 1863, cost 0.0004415. | `6d508af920c057742c0fa13e5d85259243809f66eeaac913cc4811bfd3be9d93` |

Provenance:

- **Producer.** The parent ran consented, isolated calls with working default authentication. Claude Code **2.1.312** (`entrypoint: "sdk-cli"` in every assistant record). An earlier `claude --version` in the same session printed 2.1.263; by capture time the launcher `~/.local/bin/claude` resolved to `~/.local/share/claude/versions/2.1.312` and `claude --version` printed 2.1.312, so the CLI auto-updated between the two checks. The transcripts and the launcher agree on 2.1.312.
- **Recording.** Transcript timestamps run from 2026-10-28T02:28Z to 02:29Z. The fixtures were redacted the same day.
- **Isolated source root.** `/tmp/tokenmunchers-smoke-EKLNb6/claude-root/projects/-tmp-tokenmunchers-smoke-EKLNb6-work/`. Source files and their sha256 values:
  - `7c0ffee0-0000-4000-8000-000000000001.jsonl`: `f4b0fb2b61257e8f69efc0578397a4f7cacfb5d55bbfd81f58740b37998fa9c7`
  - `7c0ffee0-0000-4000-8000-000000000002.jsonl`: `eda206d720591d70200c613ee8977495bff5c0247329cfc905aa8c5b841e2dea`
- **Filter.** [`redact-real.jq`](redact-real.jq), sha256 `da935610a5d12c139c5c14b900e60baf5094f09e4547fc613600468cf3baaf31`. It was run as `jq -c -f redact-real.jq <source> > <fixture>`; the `jq` on the capture host is jaq 3.1.1.
  - Assistant rows keep `type`, `timestamp`, `uuid`, `parentUuid`, `sessionId`, `isSidechain`, `requestId`, `version`, and `message.id`/`model`/`stop_reason`/`usage`.
  - Cost-state rows keep `type`, `sessionId`, and their numeric/boolean fields: duration, line counts, `totalCostUSD`, `hasUnknownModelCost`, and `modelUsage`.
  - Every other row keeps only `type`, `timestamp`, `uuid`, `parentUuid`, `sessionId`, and `isSidechain`.
  - Prompts, attachments, request blobs and shapes, tool inputs and results, assistant content, `cwd`, and git metadata are never copied.
- **What the captures establish for this producer.** Saved per-message output equals the final value, and repeated frames of a multi-block response carry identical usage. For each model, the sum over distinct message IDs equals the latest cost-state `modelUsage`. The parent also reported that the SDK result JSON output totals matched.
- **Limits.** One producer build, `claude-haiku-5-5` only, first-party default authentication (no transcript provider field, so provider stays `unknown`), no subagent, no compaction, no thinking tokens, no server tools, and two short sessions. Subagent joining, other models and providers, compaction, and long sessions are not evidenced by these captures. Exact cost-state equality, not a version check, gates verification.

## Real-session verification status

The earlier isolated attempt failed with expired authentication; the redacted real captures above come from a later, successful run. They prove final per-request output **only where** a fully read session's distinct responses sum exactly to the latest cost-state per model. Every other case stays unverified, and the regressions cover that conservative behaviour. Cost is never allocated from cost-state to requests. This worker made no paid calls and ran no tests, builds, linters, formatters, or runtime gates. The [adapter README](../../README.md) lists parent-run commands and scenario mappings.
