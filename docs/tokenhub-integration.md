# TokenHub integration exploration

**Status (2026-11-22):** The native Claude Code and Codex collectors under `integrations/` have been removed. Munch now handles supported routed calls and desktop usage, and Munch 0.1.7 adds [observed Codex history import](../proxy/README.md#codex-history-backfill), not Claude Code history import. The collector descriptions below refer to the pinned review snapshot, not the current implementation.

**Date:** 2026-11-19 | **Status:** research only; every design below is a **proposal**. No application code, schema, or dependency was changed.

**Question:** how could TokenMunchers (this repo) integrate with [`thecandylane/tokenhub`](https://github.com/thecandylane/tokenhub)?

**Pinned sources**

| Source | Version | Notes |
| :--- | :--- | :--- |
| TokenHub | commit [`7760c3c76e1f`](https://github.com/thecandylane/tokenhub/tree/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064) (2026-11-18 16:24:59 -0600, "Grey out data-only menu lines…"), cloned and read in full: `hub.py`, `backfill.py`, `stats.py`, `prices.py`, `tokenhub.zsh`, `tokenhub.1m.py`, `README.md`, `test_hub.py`, `test_stats.py`, `omp-models.yml.example` | The repo is 14 files, Python stdlib only, no releases or schema versions. Links below use the pinned commit. |
| TokenMunchers | Reviewed at `1f017a6` | The collector pipeline (`contributions`) was introduced in `1d4f9e4`. |

**Reading conventions.** *Fact* = read in source (cited). *Probe* = observed by running pure TokenHub functions in a scratch directory (Section 9). *Proposal* = my design. *Unknown* = not established; Section 8 lists them. TokenHub link shorthand: `hub.py#L52-L55` means <https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L52-L55>.

---

## 1. Summary and recommendation

1. **TokenHub and TokenMunchers measure different things at different layers.** TokenHub is a local, per-machine, *wire-level* proxy writing a personal SQLite usage log. TokenMunchers is a multi-user, *source-owned* leaderboard whose collectors read each harness's own records and publish allowlisted metadata with explicit opt-in, ownership, and nullable-quality semantics.
2. **Accounting ownership must stay one-owner-per-harness.** TokenMunchers already owns OMP (plugin, `ingest`/`history`), Claude Code and Codex (collector, `contributions`). TokenHub *also* observes and back-fills all three, plus Grok Build, Gemini CLI, Aider, OpenCode, Ollama and OpenRouter. Nothing in either system can detect that the same call arrived by two paths, because TokenHub live rows carry no upstream request/message ID (Section 3.2) and the server's identity is `(user_id, harness, canonical_id)` (Section 4.2). So: **no adapter may re-publish a TokenHub row for a harness TokenMunchers already collects natively.**
3. **Recommended order**
   1. **Now: zero-change coexistence**, optionally with a SwiftBar link to the existing member-authenticated dashboard (reverse display, Section 6.C). No data flows between systems.
   2. **Then: a narrow, explicit opt-in, one-way pilot for coverage TokenMunchers lacks, starting with Grok Build only**, via the existing `contributions` pipeline, under its own harness label (`grok-build`), **not** a generic `tokenhub` harness. TokenHub is the *collector*, not a provider or harness. Provenance (route + client) is a whitelist, and the pilot has **no broad historical backfill**.
   3. **Prefer an upstream-owned, versioned, metadata-only export with a stable persisted opaque capture ID** over reading TokenHub's raw SQLite rows as a maintained interface. A pinned **read-only SQLite adapter is acceptable only for a local, no-network feasibility preview**, never for durable leaderboard sync keyed on raw `id` (Section 3.1 shows why).
   4. **Rejected:** merged/shared database, dual counting, importing Claude/Codex/OMP rows from TokenHub, using TokenHub's `stats.py`/SwiftBar numbers as an adapter source (Section 6.D).
4. **Incremental slice** (Section 7): S1, a local read-only *preview* of Grok-only rows against a pinned TokenHub schema fixture, with no upload and no TokenMunchers schema change. Acceptance evidence is listed there. Uploading needs a separate contract change (enum, migration, tests) that is explicitly **not** part of S1.

---

## 2. Decision criteria used

Compare each option on accounting ownership, stable identity, token semantics, historical versus live coverage, and privacy.

---

## 3. TokenHub facts (pinned commit)

### 3.1 SQLite row identity and constraints

- **Location.** `DB = $TOKENHUB_DB or ~/.tokenhub/usage.db` ([`hub.py#L21`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L21)). Directory created `0700`; the database file itself takes the process umask ([`hub.py#L58-L64`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L58-L64)). `PRAGMA journal_mode=WAL` is set once in `init_db`.
- **Schema, exactly** ([`hub.py#L52-L55`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L52-L55)):

  ```sql
  CREATE TABLE IF NOT EXISTS usage(
    id INTEGER PRIMARY KEY, ts TEXT, source TEXT, model TEXT, status INT, stream INT,
    input INT, output INT, cache_read INT, cache_write INT, reasoning INT, cost_usd REAL,
    client TEXT, session TEXT, path TEXT, ms INT, est_usd REAL)
  ```

  Every column except `id` is nullable and untyped by constraint. There is **no `UNIQUE`, no `NOT NULL`, no `CHECK`, no index, no foreign key, no `AUTOINCREMENT`, no installation/device column, and no schema version** (`PRAGMA user_version` is `0`). The only schema migration is a one-off `ALTER TABLE … ADD COLUMN est_usd` guarded by `PRAGMA table_info` ([`hub.py#L63-L64`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L63-L64)). *Probe* confirmed `sqlite_master` lists no index and that identical rows insert without error (3 identical rows kept).
- **`id` is the rowid alias, not a durable identity.** Without `AUTOINCREMENT`, SQLite may reuse the largest rowid after that row is deleted ([SQLite rowid / AUTOINCREMENT docs](https://www.sqlite.org/autoinc.html)). *Probe* (pinned schema, scratch DB): after `backfill`-style deletion of rows `1,2` and re-insertion, the re-imported "same" backfill row got `id=4`; after deleting the max row and inserting a different request, **`id=4` was reused by a different row**. Consequences:
  - `--rebuild` ([`backfill.py#L124-L125`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L124-L125)) deletes every `client='backfill'` row and re-inserts it with **new ids**, so a sync keyed on `id` would count every imported row twice.
  - Any manual deletion of the newest row followed by a new request can re-issue its id to a different call.
  - There is no way to tell two *distinct* identical-looking requests (same second, model, tokens) from a duplicate.
- **Inserts are per-row connections.** `save` opens a new connection with `timeout=10` for every row ([`hub.py#L67-L70`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L67-L70)). A lock longer than 10 s raises `sqlite3.Error`, which `_record` catches and writes only to stderr: `usage not saved: …` ([`hub.py#L296-L299`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L296-L299)). Such a call is simply absent. *Unknown:* real-world frequency.
- **Readers.** `stats.py` opens the DB read-only (`file:…?mode=ro`, [`stats.py#L73`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L73)). A read-only adapter is feasible; a WAL database also needs `-wal`/`-shm` accessible to the reader (SQLite behavior; not exercised here).

### 3.2 Timestamps

| Row kind | `ts` value | Source |
| :--- | :--- | :--- |
| Live proxy | `datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S")`: **UTC, naive text, 1-second resolution, taken when the row is saved, after the response finished and after the client saw EOF** | [`hub.py#L269-L272`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L269-L272), [`#L296`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L296) |
| Backfill | `ts_of(s) = s[:19].replace("T", " ")` of the source log's timestamp | [`backfill.py#L47-L48`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L47-L48) |

- The live timestamp is **completion/save time, not request start**. Request duration is in `ms` (up to `TIMEOUT = 600` s, [`hub.py#L38`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L38)), but a local-only column.
- `ts_of` truncates fractional seconds and **drops any UTC offset without converting**. *Probe:* `2026-11-18T10:00:00+02:00` and `…-06:00` both become `2026-11-18 10:00:00`. This is only correct for `Z`-suffixed source timestamps. *Unknown:* whether Claude Code, Codex, OMP/pi or Grok ever emit non-`Z` offsets (the local TokenMunchers adapters normalize via `Date.parse`, `integrations/shared/src/metadata.ts` `metadata()`).
- TokenMunchers' server accepts only `YYYY-MM-DDTHH:MM:SS[.mmm]Z` ([`supabase/functions/contributions/validation.ts#L10-L14`](../supabase/functions/contributions/validation.ts)). An adapter must convert naive text to `…Z` explicitly (treating it as UTC), after which sub-second precision is permanently lost.
- `stats.py` converts with `date(ts,'localtime')` ([`stats.py#L74`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L74)), i.e. **local calendar days**, while the TokenMunchers dashboard buckets by **UTC day** (`dashboard/lib/stats.ts#L10-L21` `utcDay`; `supabase/migrations/20261028010000_quality_accounting.sql#L8-L10` `at time zone 'utc'`).

### 3.3 Dedupe and rebuild

- **Live rows:** no dedupe at all. One `INSERT` per recorded response ([`hub.py#L291-L299`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L291-L299)). A client retry that the upstream bills twice produces two rows (correct); a proxy-level duplicate cannot arise from the code, but nothing guards it either.
- **Backfill rows** carry their identity only in `path = '/backfill/<key>'` with `client = 'backfill'`. Idempotence is enforced by a Python `set` of existing `path` values ([`backfill.py#L126`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L126)), **not** by the database; two concurrent `backfill.py` runs could both insert.
  - Keys: Claude Code `<requestId or message.id>` ([`#L37`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L37)); omp/pi `omp/<entry id>` ([`#L65`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L65)); Codex `codex/<session>/<ordinal or timestamp>` ([`#L101`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L101)); Grok Build `grok/<sessionId>/<turnNumber>/<model>` ([`#L115`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L115)).
  - Claude Code: last transcript line per request key wins ("usage grows while streaming", [`#L24-L40`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L24-L40)). This is the exact heuristic TokenMunchers' Claude adapter declines to trust, because saved per-step `output_tokens` can be a placeholder (`integrations/claude-code/README.md#L67-L68`; `integrations/claude-code/src/normalizer.ts`, which sets `output_tokens: null` and `OUTPUT_PLACEHOLDER_UNVERIFIED`).
  - Codex: per-event deltas of cumulative `total_token_usage`, with replay skipped by `(timestamp, total_tokens, input_tokens)` and negative deltas treated as a counter reset ([`#L70-L102`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L70-L102)). TokenMunchers' design notes call this a *candidate heuristic*, not an exact ledger (`docs/claude-codex-backfill.md` §3.2 "Tier 2").
- **Live/history cutoff is global and moving:** `live = MIN(ts) FROM usage WHERE client!='backfill'` and **every** backfill row with `ts >= live` is skipped, for **all** sources ([`backfill.py#L127-L143`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L127-L143)). Consequences (*inference from code*): (a) the first proxied request of *any* tool (e.g. OpenRouter) closes the history window for *every* tool, so a harness first used through the hub later, or used outside the wrapper after that moment, has an unrecoverable gap; (b) Claude, Codex, Grok and omp history rows are inserted only if older than that single instant.
- **`--rebuild`** ([`#L124-L125`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L124-L125)) deletes only `client='backfill'` rows. The README recommends it "after changing prices" ([`README.md#L90`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/README.md#L90)). History rows are therefore *re-derivable and mutable by design*, and any consumer of them must not treat them as immutable accepted contributions. (TokenMunchers' contributions endpoint, by contrast, is upsert-on-conflict and never deletes accepted usage when the source file disappears: `supabase/README.md#L95-L124`, "Atomicity/retention".)
- **Backfill's intended retention-loss protection is absent:** deleting transcripts and re-running `--rebuild` permanently loses those rows. TokenMunchers solves this with a durable local ledger (`integrations/shared/src/ledger.ts`, `deliver()` stages the whole ledger, `integrations/shared/src/delivery.ts#L41`).

### 3.4 Usage and token mapping

`extract(source, objs)` ([`hub.py#L96-L148`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L96-L148)) returns one tuple `(input, output, cache_read, cache_write, reasoning, cost, est_usd)` **per billed model per HTTP request**.

| Field | Anthropic-style (`source=="anthropic"` or cache fields present) | OpenAI-style (`openai`, `chatgpt`, `openrouter`, `ollama`, custom) | Gemini | xAI/Grok |
| :--- | :--- | :--- | :--- | :--- |
| `input` | `input_tokens` (already exclusive) | `prompt_tokens − cached − cache_write_tokens`, floor 0 (`hub.py#L138-L144`) | `(promptTokenCount + toolUsePromptTokenCount) − cached` | as OpenAI-style |
| `output` | final `output_tokens` after `usage` dicts from every event are `dict.update`-merged (so the `message_delta` value overwrites the `message_start` placeholder) | `completion_tokens` / `output_tokens` | `candidates + thoughts` (thoughts billed as output) | `completion_tokens` **plus** reasoning **only when** `total_tokens ≥ prompt+completion+reasoning` (`hub.py#L142-L143`) |
| `cache_read` | `cache_read_input_tokens` | `prompt_tokens_details.cached_tokens` (or Ollama `prompt_eval_cached_count`) | `cachedContentTokenCount` | same as OpenAI-style |
| `cache_write` | `cache_creation_input_tokens` | `prompt_tokens_details.cache_write_tokens` (OpenRouter) else 0 | 0 | same |
| `reasoning` | `thinking_tokens` (separate, informational) | `reasoning_tokens` (a subset of output, informational) | `thoughtsTokenCount` | `reasoning_tokens` |

- `TOK = input + output + cache_read + cache_write` ([`stats.py#L12`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L12)) matches the TokenMunchers definition ("total = input + output + cache_read + cache_write", `supabase/migrations/20261004220000_init.sql#L50-L55`). **For the four primary categories the semantics are compatible**; `reasoning` is *not* additive in either system and has no TokenMunchers field.
- **Anthropic multi-iteration responses** (compaction/fallback entries in `usage.iterations`) become **several rows per request**, one per model, using only the iteration totals ([`hub.py#L120-L128`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L120-L128); `test_hub.py` "accounting edge cases"). *Probe:* one response with three iterations (`message`, `compaction`, `message` on a second model) produced two rows (opus: 12/14, haiku: 2/3). **A row is therefore not a request; `call_count=1` per row is wrong for these.** Each such row also repeats the same `ms`.
- **Streams cut short.** *Probe:* an Anthropic stream with only `message_start` (client disconnect before the final `message_delta`) yields `(10, 1, 5, 3, …)`, i.e. `output=1`, the *start-of-stream placeholder*, with an estimated cost. The loop `break`s when the client goes away ([`hub.py#L250-L254`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L250-L254)), so the upstream's remaining events are never read. The row looks complete but is not.
- **Codex live path** (OpenAI Responses `response.completed.usage`): *probe* `input_tokens=1000, cached=400, output=50, reasoning=20` → `(600, 50, 400, 0, 20)`. Cache **writes** are never extracted for Responses (`cw` reads `prompt_tokens_details.cache_write_tokens` only), while the TokenHub *backfill* reads `cache_write_input_tokens` from rollouts (`backfill.py#L91`). Live and history therefore differ for Codex cache writes (*inference*).
- **Grok Build mapping is inconsistent between live and history** (*fact from code; which is right is Unknown*): live subtracts cache writes from input and adds reasoning into output when the totals say completion excludes it (`hub.py#L138-L144`); history computes `input = inputTokens − cachedReadTokens` only, takes `cacheCreationTokens` as `cache_write` **without subtracting it from input**, and uses `outputTokens` without adding reasoning (`backfill.py#L113-L117`). If Grok's `inputTokens` includes cache creation, the history path double-counts it; if `outputTokens` excludes reasoning, history under-counts output relative to live. TokenHub's xAI test (`test_hub.py`: `prompt_tokens 32, completion 9, total 151, reasoning 110 → input 24, output 119`) documents live behavior for the `/xai` shape; there is no equivalent test of the `/grok` subscription route or of the usage.json shape (`README.md#L16` marks `/xai` "untested").

### 3.5 Cost mapping

- `cost_usd` is filled **only when the upstream reports it**: OpenRouter `usage.cost`, xAI `cost_in_usd_ticks / 1e10`, Grok history `costUsdTicks / 1e10` (zero ticks → `None`) ([`hub.py#L145-L148`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L145-L148), [`backfill.py#L117`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L117)).
- `est_usd` is a **list-price estimate, Anthropic-shaped usage only**, from a hard-coded table dated "2026-11-18" ([`prices.py#L1-L26`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/prices.py#L1-L26)); it ignores batch discounts, fast mode and residency. Unknown model → `None`.
- `stats.py` totals use `COALESCE(SUM(cost_usd),0)+COALESCE(SUM(est_usd),0)` ([`stats.py#L77-L82`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L77-L82)): **missing cost is silently added as $0**, and reported and estimated costs are summed. TokenMunchers' rule is the opposite: unknown cost is absorbing and estimates are labeled (`supabase/README.md#L122-L124`, `integrations/shared/src/metadata.ts#L15-L35`, `has_unknown_cost`, `has_estimated_cost`).
- Mapping proposal, if anything is ever published: `cost_usd` non-null → `cost_basis: reported`; `est_usd` is **not** imported in the Grok pilot (it is Anthropic-only anyway); both null → `cost_usd: null, cost_basis: unknown`. Never map a missing cost to 0.

### 3.6 Proxy coverage, losses, and source attribution

**What is recorded.** Only `POST` responses ([`hub.py#L267`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L267)), after header/body pass-through, from routes `anthropic, openrouter, xai, grok, ollama, openai, chatgpt, gemini` plus `routes.json` ([`hub.py#L23-L32`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L23-L32), [`#L40-L49`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L40-L49)).

**Row-emission rules (the completeness problem)** ([`hub.py#L274-L301`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L274-L301)):

| Situation | What `_record` does | Consequence for an exporter |
| :--- | :--- | :--- |
| Parse exception | `rows = [(None, None)]` | Falls into the "no usage" branch below. |
| No usage and `status < 400` | **No row**, *except* a streamed `completions`/`responses` path, which stores an **all-zero row** | Missing calls are silent; zero rows are *placeholders for unknown usage, not zero usage*. |
| `status ≥ 400` | Row saved with `(0,0,0,0,0,None,None)` (tests assert a 401 row has `output=0`) | Failed requests appear as zero rows; `stats.py` filters `status<400` ([`stats.py#L74`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L74)). |
| Upstream dies mid-stream | `status` overwritten to **502** ([`hub.py#L255-L259`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L255-L259)), row still carries whatever usage was captured | Possibly billed tokens exist under `status=502`, which `stats.py` excludes. |
| Response > 16 MiB (`MAX_CAPTURE`) | Forwarding continues; capture stops; `over=True` only writes a stderr warning; **usage from the captured prefix can still produce a row** ([`hub.py#L36`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L36), [`#L246-L249`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L246-L249), [`#L300-L301`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L300-L301)) | The README says such a response "loses its usage row" (`README.md#L78`); the code can instead emit a **partial row**. The DB stores no `over` flag. |
| SQLite write fails | stderr only | Silent absence. |
| `Content-Encoding` other than `gzip` | not decoded; parse fails | Treated as "no usage". (`Accept-Encoding: identity` is forced upstream, so rare.) |

**There is no `usage_state`/completeness column.** `status=200` plus positive numbers is not proof of completeness. Any publishing path needs an explicit completeness indicator in a supported export (Section 7, S2), or must publish only fields it can prove, with `null` for the rest.

**What is never seen:** WebSocket traffic (426 returned so clients fall back, `hub.py#L197-L198`); tools that bypass the wrapper (editor/desktop launches do not read `~/.zshrc`, `README.md#L57`; `omp` models not declared in `models.yml` "may bypass the hub", `omp-models.yml.example`); Cursor (impossible, `README.md#L46`); **Claude Code/Codex traffic when `tokenhub.zsh` falls back because the hub is down** (`tokenhub.zsh`, `_tokenhub_up`). Most non-Claude/Codex/Grok harness rows in the README are "untested" (`README.md#L33-L46`).

**Source attribution.**
- `source` = the **route prefix**, i.e. the upstream a request was forwarded to, **not** the harness ([`hub.py#L201-L205`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L201-L205), [`#L295-L297`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L295-L297)). `anthropic` could be Claude Code, omp, or any SDK pointed at the route; `grok` could be any client using `GROK_MODELS_BASE_URL`; `routes.json` can *replace* a built-in route's upstream.
- `client` = first token of `User-Agent`, max 60 chars ([`hub.py#L290`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L290)), user/client-controlled; `'backfill'` marks history rows.
- `session` = `x-claude-code-session-id` header only ([`hub.py#L297`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/hub.py#L297)): **NULL for every non-Claude live row**, and no upstream request/message ID is stored for any live row.
- Backfill rows set `source` to the *provider* (omp: `m.provider or "omp"`, [`backfill.py#L65`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/backfill.py#L65)). An omp session using Anthropic therefore lands under `source='anthropic'`, **the same label as Claude Code**. `stats.py` then treats `source='anthropic'` as Claude Code for session counting and the `tokens_cc` swap ([`stats.py#L94-L116`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L94-L116)). The only recoverable harness provenance for history rows is the `path` key prefix (`/backfill/omp/…`, `/backfill/codex/…`, `/backfill/grok/…`; bare for Claude Code).
- **Consequence:** harness provenance cannot be derived from `source` alone. Any adapter needs an allowlist of `(source, client/path-prefix)` pairs, validated against real captures, and must refuse rows that fail it.

**Privacy-relevant content.** Stored: timestamps, route, model (sanitized to `[\w.:/@+-]`, ≤100 chars, `hub.py#L293`), status, token/cost counts, UA token, Claude session ID, URL path without query (can embed a model id for Gemini), duration. Not stored: bodies, headers, credentials (`hub.py` docstring and pass-through at `#L211-L218`; `test_hub.py` asserts the `Authorization` secret reaches upstream unchanged). The hub binds `127.0.0.1` only and rejects browser/DNS-rebinding requests (`hub.py#L193-L196`, `#L317`). This matches TokenMunchers' "metadata only" promise (`README.md` privacy section), but `path`, `client`, `session`, `ms`, `stream`, `status` are **not** in TokenMunchers' allowlist (`integrations/shared/src/metadata.ts#L3-L4` `fields`; `validation.ts#L15-L33` rejects unknown keys) and must never be exported as-is.

### 3.7 Stats compatibility

`stats.py` (and the SwiftBar plugin built on `stats.compute`) is *presentation over the local DB*, not an accounting source:

- Windows: local calendar days, `7d` = today and prior 6 days; TokenMunchers: UTC today and rolling `now − 7×86400 s` (`dashboard/app/page.tsx#L18-L22`; `supabase/migrations/20261028010000_quality_accounting.sql#L53`).
- Filter: `status < 400`; TokenMunchers' filters are different and quality-flagged.
- Cost: missing counted as 0 and reported+estimated summed (Section 3.5).
- All-time Claude tokens can be **replaced** by Claude Code's `~/.claude/stats-cache.json` totals, which "count replayed history twice" (`tokens_cc`, [`stats.py#L110-L116`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L110-L116)); sessions/active days mix DB rows and that cache ([`stats.py#L54-L63`](https://github.com/thecandylane/tokenhub/blob/7760c3c76e1fe6f4dfb900e477aeb20e4bf34064/stats.py#L54-L63), `#L94-L99`).
- Unit of count: `requests` = rows (Section 3.4 explains rows ≠ requests).
- Therefore totals, percentages, streaks and "favorite model" are **not comparable** between the two systems without an identical window, harness filter, and cost-completeness rule. Do not source an adapter from `stats.compute`, the menu plugin, or SwiftBar output, and do not put the two side by side as if they should agree.

---

## 4. TokenMunchers facts relevant to integration

This section describes the pinned review snapshot. For the current endpoint, including append-only Codex backfill, see the [Supabase contributions contract](../supabase/README.md#post-functionsv1contributions).

### 4.1 Ingestion paths and what already owns what

| Harness | Owner at review | Path | Identity | Evidence |
| :--- | :--- | :--- | :--- | :--- |
| OMP | `plugin/` | `ingest` (live events) + `history` (daily aggregates from `omp stats`) | `event_id` = UUIDv5 of `sessionKey\|timestamp\|provider\|model` (`plugin/src/adapter.ts#L84`, doc comment: reported and back-filled calls land once via conflict-ignore on `event_id`) | `docs/harness-support-review.md` predates the native collectors |
| Claude Code | `integrations/claude-code` | `contributions` | `message:<id>` > `request:<id>` > scoped sha256 (`integrations/claude-code/src/identity.ts`) | output is `null` unless reconciled with saved cost-state; cost unknown |
| Codex | `integrations/codex` | `contributions` | response ID; `legacy:<hash>` reconstructed intervals | `integrations/codex/src/parser.ts` |

In the reviewed implementation, contributions were metadata-only with explicit per-harness consent, a per-device UUID, an immutable cutoff, and pause windows. They were not posted via `ingest` or `history`.

### 4.2 The `contributions` contract (existing seam)

- Endpoint validates a fixed allowlist per observation: `harness` ∈ {`claude-code`, `codex`} ([`validation.ts#L16`](../supabase/functions/contributions/validation.ts)), `grain` ∈ {`request`, `reconstructed_interval`}, `quality` ∈ {`observed`, `reconstructed`, `partial`}; IDs ≤512, provider ≤64, model/session ≤128, counts nullable non-negative ≤1e9, `cost_usd` nullable ≤10,000; unknown keys reject the chunk; `cost_basis: unknown` iff `cost_usd` null; timestamp must be ISO `Z`.
- Server identity: `PRIMARY KEY (user_id, harness, canonical_id)` on `public.collector_contributions` ([`supabase/migrations/20261028000000_collector_contributions.sql#L44-L69`](../supabase/migrations/20261028000000_collector_contributions.sql)); harness is `CHECK`-constrained at `#L5`, `#L44`, `#L54`. A different device re-sending an owned ID returns `409 ownership_conflict` (`#L133-L135`).
- **`cutoff` classifies, it does not exclude.** `partition = case when timestamp < cutoff then 'history' else 'live'` (migration `#L175`). Both partitions are accepted and counted. So "opt-in only from now" must be enforced **client-side before upload** (Section 7). The existing adapters' cutoff handling is in `integrations/shared/src/ledger.ts` (`activate`, `record`) and `aggregate.ts` (`attributionIssue`); do not claim the server cutoff alone gates upload.
- **Pause semantics:** `Ledger.record` drops any observation whose timestamp/interval overlaps a pause (`ledger.ts#L137`) and remembers it in `excluded_ids`, so it is never uploaded later even after resume. TokenHub's second-resolution *floor* timestamps (Section 3.2) mean a call saved at `12:00:00.9` appears at `12:00:00.000Z`; `end >= pause.start` would miss it for a pause beginning at `12:00:00.5`. A TokenHub adapter must compare using the conservative interval `[ts, ts+1 s)` (*proposal*). A request that **began** before a pause started but finishes in it is also ambiguous, since `ts` is completion time.
- **Unknown is absorbing**: any null in a day's rows makes that day's total null; the dashboard shows "unknown" and does not rank (`supabase/README.md#L122-L124`; `supabase/migrations/20261028010000_quality_accounting.sql#L2-L6`, `#L35`).
- **Harness is stored but not exposed to views**: `collector_accounting_parts` selects `provider`, `model`, `is_subagent` from the JSON observation and omits `harness` (`…quality_accounting.sql#L2`). Per-harness dashboard attribution would therefore need a coordinated view/dashboard change (consistent with finding 5 of the pre-collector `docs/harness-support-review.md`).
- **Subagent null counts as main**: `collector_accounting_parts` and `recent_activity` treat a null `observation.is_subagent` as `false` (`…default_main_attribution.sql`), so unclassified rows count toward "without subagents" totals while the stored observation keeps the null.
- **Rate limits and caps** (`supabase/README.md#L95-L121`): ≤500 entries/chunk, ≤256 KiB, ≤1000 chunks, 10 of 600 hits/min per key per request, ≤4 staged snapshots, begin/chunk/commit atomic.
- **Authentication boundaries.** `tm_` API keys are *write* credentials for `ingest`/`history`/`contributions`; `history`'s `status` action returns only that device's totals. Group totals live behind member-session reads: `dashboard/app/page.tsx#L12-L29` (`getMe` then RPCs `leaderboard_since`, `leaderboard_all_time`) and `dashboard/app/u/[handle]/page.tsx#L18-L43` both gate on a member session.

---

## 5. Semantics crosswalk (TokenHub row → TokenMunchers `Observation`)

| TokenMunchers field | TokenHub source | Fit | Note |
| :--- | :--- | :--- | :--- |
| `harness` | none (route ≠ harness) | **Gap** | Needs a provenance whitelist, not `source` alone (Section 3.6). Proposed pilot value `grok-build`; contract change required. |
| `canonical_id` | none on live rows; `/backfill/<key>` on history rows; raw `id` unstable | **Gap** | Need a persisted opaque capture ID (Section 7, S2). |
| `identity_strength` | n/a | `scoped` | No upstream request/message ID is captured, so cross-install dedupe is **not** provable; ownership is single-device. |
| `grain` | row (request) or turn×model (Grok history) | **Mixed** | Live Anthropic multi-iteration rows are not 1:1 with requests; Grok history rows are turn×model aggregates. |
| `timestamp` | `ts` naive UTC seconds, completion time | Convert to `…Z` | Pause/cutoff must use a 1 s conservative window. |
| `provider` | `source` (route) | Conditional | Only for whitelisted routes; `grok` is a transport name, not a verified provider. |
| `model` | `model` | OK | Already sanitized ≤100 chars. |
| `session_id` | `session` (Claude only) | Mostly null | Fine for `grok-build`; backfill Grok rows have `sessionId` but history is out of the pilot. |
| `is_subagent` | none | **null** | Propagates to unknown "without subagents" views. Do not silently set `false`; decide with product (Section 8). |
| `input/output/cache_*` | same-named columns | OK for the four categories | Disjoint, same total formula. Subtract nothing extra; apply the `grok` live-path rules; **do not mix live and history Grok rows** (Section 3.4). |
| `call_count` | none | **Not 1 by default** | Rows ≠ requests for Anthropic iterations; for Grok live usage unknown. `1` only where one-row-one-request is established; otherwise `null`. |
| `cost_usd` / `cost_basis` | `cost_usd` (reported) | OK | `est_usd` excluded; null → `unknown`. |
| `quality` | none (no completeness column) | **Gap** | `observed` only with an explicit completeness indicator; otherwise `partial` with null for unproven fields. |
| *(not exported)* | `status`, `stream`, `path`, `client`, `ms`, `reasoning`, `est_usd` | n/a | Outside the allowlist. |

---

## 6. Options compared

### A. Zero-change coexistence

*What:* run both. TokenHub stays local and personal; TokenMunchers continues with OMP plugin + Claude/Codex collectors. No code, no data movement.

| Criterion | Assessment |
| :--- | :--- |
| Ownership | **Clear and trivially satisfied.** TokenMunchers owns what leaves the machine; TokenHub owns the local view. They never merge, so there is no double count, only *different numbers*. |
| Identity | Not needed. |
| Token semantics | Four-category token totals are compatible, but windows, status filter, cost handling and call units differ (Section 3.7), so the two UIs will legitimately disagree; document that, do not reconcile. |
| History vs live | Each system keeps its own. TokenHub has the Claude "output is final" advantage live (proxy sees the final `message_delta`), TokenMunchers' Claude transcript path has output null unless cost-state verifies it. This is a *real* coverage difference that coexistence leaves as-is. |
| Privacy | Strongest: nothing new leaves any machine. TokenHub's "nothing leaves your machine" (`README.md#L94`) stays true. |
| Cost | Zero. **Residual risk:** users running Claude Code/Codex/omp through the hub *and* the native collector see parallel but non-reconcilable counts. Mitigation is documentation (this note's ownership table), not code. |

### B. Selective one-way adapter for coverage TokenMunchers lacks

*What:* read TokenHub-captured usage for **harnesses TokenMunchers has no collector for**, publish via the existing `contributions` seam. **Excluded by policy:** Claude Code, Codex, OMP (TokenMunchers owns them), and any `routes.json` custom route or unrecognized client.

*Candidate coverage and why Grok Build first:*

| Candidate | In TokenHub | TokenMunchers today | Verdict |
| :--- | :--- | :--- | :--- |
| Grok Build | live via `/grok` + history from `~/.grok/sessions/*/*/usage.json` | **none** | **First candidate.** Cost is upstream-reported (`costUsdTicks`). Format evidence is *only* TokenHub's code (Unknown 1). |
| Gemini CLI, Aider, OpenCode, Zed | live only, all marked "untested" (`README.md#L42-L45`) | none | Defer: no verified provenance or format; OpenCode is better served by its native plugin (`docs/harness-support-review.md`, flagged pre-collector). |
| Ollama / OpenRouter direct use | live | OMP plugin already reports OMP's calls to these providers | **Overlap risk with OMP**; exclude until a per-client provenance rule exists. |
| Claude Code, Codex, OMP | live + history | native | **Excluded** (ownership). |

*Why not a generic `tokenhub` harness.* TokenHub captures traffic from several tools; it is not itself a harness. A generic label would let a TokenHub-sourced Claude row and a native Claude row coexist under separate server keys even though they might represent the same call. Keep `grok-build` as the harness and TokenHub as collector provenance.

Pilot design (**proposal**):

1. **Opt-in and cutoff:** explicit activation per tool, as for Claude/Codex (`Ledger.activate`). Only rows with `ts` ≥ the activation instant, rounded **up** to the next second, are eligible; everything earlier is dropped client-side (never uploaded as `history`). *No broad historical backfill*, including TokenHub's `/backfill/grok/…` rows.
2. **Provenance whitelist:** use `source='grok'` and a client marker verified against a real Grok Build capture to select candidates, excluding `client='backfill'`. Route and User-Agent alone are insufficient to establish origin: the route can be overridden and the client controls the header. For upload, require the export to attest the built-in upstream and an explicitly opted-in Grok Build launch path; reject ambiguous records.
3. **Completeness gate:** publish a call only if the export marks usage complete (status<400, usage parsed, not a zero placeholder, not over the capture cap). Otherwise emit nothing; do not emit zeros.
4. **Fields:** tokens as stored for the live Grok path; `cost_usd` reported-only; `is_subagent: null`; `call_count: 1` only if Grok Build is shown to make one upstream request per row (else `null`); `quality: observed` if (and only if) completeness is explicit; `identity_strength: scoped`; `canonical_id` from the persisted opaque capture ID.
5. **Privacy and pause:** reuse `Ledger`, `metadata()` allowlist and pause semantics; apply the `[ts, ts+1s)` pause comparison; never export `path`, `client`, `session`, `ms`, `status`, `stream`, `reasoning`, `est_usd`.
6. **Single owner:** activation refuses (or warns) if the same machine has another active collector whose harness could carry the same traffic.

*Two ways to read Grok data, compared:*

| | **B1. TokenHub-sourced** (export from the proxy DB) | **B2. Direct Grok Build adapter** (read `~/.grok/sessions/*/*/usage.json` like the Claude/Codex adapters) |
| :--- | :--- | :--- |
| Fit with TokenMunchers' model | Needs upstream export + ID changes | Matches the existing pattern: explicit root, ledger, local files, no proxy required |
| Live coverage | Only requests through `GROK_MODELS_BASE_URL`, hub must be up | Whatever Grok Build persists |
| History | Hub's global cutoff and `--rebuild` mutability | Whatever files retain |
| Grain | Request-level live; per-request cost | `turn × model` (`backfill.py#L105-L117`); no request count (`call_count` should be `null`) |
| Evidence | TokenHub tests (`/xai` shape only) | **Only TokenHub's `grok_rows`**; no Grok Build primary source was available (Unknown 1) |
| Verdict | Right if the goal is *reuse TokenHub* | Right if the goal is *full leaderboard coverage of Grok Build independent of a proxy*. Needs real fixtures first |

Both would need the same TokenMunchers contract change (`grok-build` harness).

| Criterion | Assessment of B |
| :--- | :--- |
| Ownership | Clear *if* the whitelist and single-owner rule hold; otherwise high double-count risk. |
| Identity | Hard: no upstream ID. Needs a TokenHub-owned persisted capture ID; raw `id` is unusable (Section 3.1). |
| Token semantics | Compatible for the four categories; Grok live/history inconsistencies make the pilot live-only. |
| History vs live | Live-only by design. |
| Privacy | New outbound path (opt-in) from a previously local-only tool; mitigated by allowlist, pause, explicit consent. TokenHub's "nothing leaves your machine" no longer holds for exported rows, so docs must say so. |

### C. Reverse display (TokenMunchers → TokenHub/SwiftBar)

*What:* surface TokenMunchers context in TokenHub's SwiftBar menu without moving accounting data.

- **Recommended minimal form:** a menu line that **opens** the dashboard URL (`https://tokenmunchers.taiisshort.com`, `README.md` top) in the browser. The dashboard already authenticates the member and reads group totals via RPCs (`dashboard/app/page.tsx#L12-L29`, `dashboard/app/u/[handle]/page.tsx#L18-L43`). **No key, no group totals, no TokenMunchers data in the TokenHub menu.** This belongs in the user's own SwiftBar plugin (it is not a change to either repo unless TokenHub's maintainer adds it).
- **Do not** embed group totals using a `tm_` ingestion key: `tm_` keys are write-scope bearer credentials; the only read they permit is a device status (`history` `status`). Do not expose Supabase credentials or the service role.
- **If native group totals are later wanted,** design a purpose-built, explicitly scoped, member-authenticated read interface (new endpoint or token class) with its own threat model; that is out of scope here.
- **Do not** show a TokenMunchers number next to a `stats.compute` number as if comparable (Section 3.7).

Criterion summary: ownership unaffected; identity N/A; semantics N/A (a link carries none); history/live N/A; privacy best of all options except coexistence (no new data leaves anywhere).

### D. Tighter integration, and why each form is rejected

| Proposal | Why not |
| :--- | :--- |
| **Merge the databases / make TokenHub write to Supabase** | TokenHub's table has no identity, ownership, user, or device concept; TokenMunchers' server is per-user Postgres with identity PK, staging, rate limits and quality flags (Section 4.2). Merging would import TokenHub's placeholder zeros, status-coded rows, mutable backfill rows, and unlabeled harness into the leaderboard. It also breaks TokenHub's local-only promise (`README.md#L94`) for every user. |
| **Shared SQLite DB** (both tools write one file) | TokenHub: WAL, per-row connections, schema it migrates ad hoc; TokenMunchers `Ledger`: `journal_mode=DELETE`, one JSON-state row (`ledger.ts` `transaction`), `BEGIN IMMEDIATE`. Two writers with different journal modes and no shared schema versioning invites lock failures that `hub.py` already swallows (Section 3.1), and any foreign row would also corrupt TokenHub's global `MIN(ts) … client!='backfill'` cutoff (`backfill.py#L127`). |
| **Dual counting** (TokenHub rows *and* native collector for the same harness) | The server cannot dedupe across namespaces: TokenHub live rows have no request/message ID, backfill keys (`/backfill/<requestId>`, `codex/<session>/<ordinal>`) do not match the native adapters' canonical IDs (`message:…`, response IDs), and TokenHub timestamps are completion-second text. A fuzzy join on (session, time, tokens) would be a fabricated identity and is forbidden by the existing design notes ("do not fabricate…", `docs/claude-codex-backfill.md`). |
| **Treat TokenHub as OMP's reporter, or as a replacement for the Claude/Codex collectors** | It would discard TokenMunchers' quality semantics (Claude output verified via cost-state, Codex response IDs), depend on the user routing every tool through a proxy, and lose history when the hub is down (`tokenhub.zsh` falls back). |
| **Run TokenHub's `stats.py`/SwiftBar as an accounting source** | Different windows, filters, cost completeness and call units (Section 3.7). |
| **Read raw TokenHub rows keyed by `id` as a maintained interface** | Unstable under `--rebuild` and rowid reuse (Section 3.1 probe). |

---

## 7. Recommended incremental slice and acceptance evidence

**Sequence (proposal):**

| Slice | Owner | Contents | Network / schema change |
| :--- | :--- | :--- | :--- |
| **S0** | docs | This note's ownership table and "numbers will differ" guidance; optional SwiftBar link (Section 6.C) | none |
| **S1** | TokenMunchers `integrations/` | **Local read-only preview** of Grok Build candidate rows against the pinned TokenHub schema. Report exclusions and uncertain fields; do not claim current rows have verified provenance, complete usage, or durable identity. Never upload. | none |
| **S2** | TokenHub upstream (proposal to maintainer) | Add (a) a persisted **opaque capture ID** (`capture_id TEXT`, uuid4 generated at save, added with the same `ALTER TABLE … ADD COLUMN` guard pattern as `est_usd`, `hub.py#L63-L64`) and an install ID; (b) a **`usage_state`** column (`complete` / `missing` / `partial` / `error`) set from the `_record` branches in Section 3.6; (c) `PRAGMA user_version` schema versioning; (d) a read-only **metadata export** (JSONL, versioned, allowlisted columns only). | TokenHub schema only |
| **S3** | TokenMunchers | Contract change to accept `grok-build`: migration `CHECK`s (`…collector_contributions.sql#L5,#L44,#L54`), `validation.ts#L16`, `integrations/shared/src/metadata.ts#L17`, `Harness` union (`observations.ts`), CLI `--source`, README/`supabase/README.md`, tests; optional harness column in `collector_accounting_parts`; then upload via existing `Ledger`/`deliver()`. | Yes, explicit user approval |

**Why S1 first:** it needs no cooperation from TokenHub's maintainer or the server and tests route/client selection, cutoff and field mapping. Completeness, verified provenance and durable identity are upload gates, not properties of a preview from raw rows.

**S1 acceptance evidence** (all must be observable, none requires network):

1. **Fixture DB** created with the *pinned* TokenHub schema (Section 3.1 SQL) containing: Grok live rows; non-Grok rows (`anthropic`, `chatgpt`, `openrouter`); `client='backfill'` rows; zero-placeholder rows; `status≥400` and `502` rows; Anthropic multi-iteration rows; and rows straddling the activation cutoff by <1 s. Preview output must include only the eligible Grok live rows.
2. **Ownership:** a Claude Code/Codex/OMP row never appears, including one under `source='anthropic'` with a Claude-like UA, and a `routes.json`-style `grok` row with a non-whitelisted client.
3. **Cutoff and pause:** rows with `ts` earlier than the activation second are excluded; a row inside a pause (using the `[ts, ts+1s)` rule) is excluded and stays excluded after resume.
4. **No placeholder zeros:** exclude zero-only and error rows from candidate totals, and tally exclusions by reason. A nonzero, successful row remains a candidate, not verified usage. Upload requires an explicit completeness signal from S2.
5. **Mapping tests** against recorded expectations: the four token categories equal the stored columns; `cost_basis` is `reported` only when `cost_usd IS NOT NULL`; null cost never becomes 0; timestamps convert naive-UTC to `…Z` with `ms=0`; `is_subagent` is `null`; `call_count` follows the rule in Section 6.B.4.
6. **Privacy:** output only normalized metadata; no raw path, User-Agent, session header, latency, status, stream flag, reasoning or estimate. Exercise the collector projection once the new harness becomes valid in S3.
7. **Identity honesty:** every observation is `scoped`; the preview prints a notice that raw row `id` is not used for identity and why.
8. **Real-world check (Unknown 1/2 closers):** at least one *real* Grok Build session routed through a real TokenHub install is captured, the stored row compared against Grok Build's own `usage.json` for the same turn, and the cache-write/reasoning relationship in Section 3.4 resolved.
9. **No regression:** no change to existing OMP/Claude/Codex outputs, to `contributions` validation, or to any migration.

**S2/S3 acceptance additions:** `capture_id` stable across `--rebuild` and restarts (rows keep the same ID); identical re-export produces identical IDs; a copied DB produces `409 ownership_conflict` rather than double count; a mixed Grok live+history DB never emits history rows; upload of a snapshot with `grok-build` rows leaves OMP/Claude/Codex totals unchanged; dashboard shows unknown (not zero) where `is_subagent`/cost is null.

---

## 8. Risks, unknowns, and open questions

**Unknowns (not established by anything I could read or run):**

1. **Grok Build's own formats.** The only evidence for `~/.grok/sessions/*/*/usage.json` (`sessionId`, `updatedAt`, `turns[].turnNumber/endedAt/modelUsage{inputTokens, outputTokens, cachedReadTokens, cacheCreationTokens, reasoningTokens, costUsdTicks}`) and for the `/grok` wire shape is TokenHub's code, which says the host was "read from the grok binary" (`hub.py#L27`). `~/.grok` and `grok` are not installed on this machine, so no fixture exists. Cache-write inclusion in `inputTokens` and whether `outputTokens` includes reasoning are unresolved (Section 3.4).
2. Grok Build's `User-Agent` first token (needed for the provenance whitelist) and whether other clients legitimately use the `/grok` route.
3. Whether Grok Build issues exactly one upstream request per proxied row (for `call_count`), whether it has subagents (for `is_subagent`), and whether `usage.json` is rewritten or cumulative.
4. Whether the TokenHub maintainer would accept `capture_id` / `usage_state` / export (I read no issue tracker or contribution guide; it is a small personal tool whose README targets macOS, `README.md#L94`).
5. Real-world frequency of the loss modes in Section 3.6 (SQLite lock timeouts, capture-cap partials, client aborts).
6. Whether TokenMunchers wants "without subagents" leaderboards to tolerate unknown (null `is_subagent`) users, or requires an explicit per-harness default.
7. Whether non-`Z` timestamps occur in any source log (Section 3.2).
8. This machine has no TokenHub DB (`~/.tokenhub` absent), so no real row distribution was examined; Claude, Codex and OMP data directories exist but were deliberately not read.

**Risks if the work proceeds**

| Risk | Mitigation |
| :--- | :--- |
| Double counting a tool both systems see | Per-tool harness labels; whitelist; single-owner activation check; no Claude/Codex/OMP import from TokenHub |
| Treating zero placeholders / partial captures as complete | Explicit `usage_state`; drop-and-count, never zero-fill |
| Unstable identity | Persisted opaque capture ID; never `id` |
| Live/history semantic drift (Grok) | Live-only pilot; resolve per Unknown 1 before any history |
| Pause leak via second-resolution timestamps | Conservative `[ts, ts+1s)` comparison |
| Silent loss of local-only promise | Opt-in, explicit docs and consent text; allowlist |
| Contract creep (`tokenhub` as a harness) | Per-tool labels; TokenHub recorded as collector provenance only |
| Misleading comparison of the two UIs | Document window/filter/cost differences (Section 3.7) |

---

## 9. Verification actually performed

- Cloned `thecandylane/tokenhub` at `7760c3c76e1f` and read all source files listed above. **I did not run TokenHub's own test suites** and did not start the proxy server; I read `test_hub.py` and `test_stats.py`.
- **Scratch probes:** exercised `hub.extract` on normal, incomplete, no-usage and multi-iteration responses and `backfill.ts_of` on UTC and offset timestamps. SQLite probes using the pinned schema confirmed duplicate acceptance, row ID reuse after deletion, and row renumbering after a backfill-style rebuild. Independent databases both assigned ID `1` to unrelated first rows.
- TokenMunchers claims are from reading committed source and docs at `1f017a6`. No TokenMunchers tests, builds, migrations or smoke checks were run, and no database or deployed endpoint was exercised.
