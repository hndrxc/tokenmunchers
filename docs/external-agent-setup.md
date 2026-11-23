# Reporting external CLI usage from OMP wrapper agents

Tokenmunchers' OMP plugin observes OMP model calls, but it cannot see requests made by a subagent that launches another model CLI. Since **omp-tokenmunchers 0.3.0**, a wrapper can append metadata-only usage events to `external-usage.jsonl`; the plugin picks them up on session start and roughly every minute.

This is an **optional integration example**, not an official OMP agent type or a required part of the plugin. The JSONL contract works with any external CLI that exposes reliable per-call token counts. The working example here uses the Antigravity CLI (`agy`), with a builder and a read-only scout. Adapt model names, flags, authentication and permissions to the CLI version you actually have.

## 1. Prerequisites

- OMP installed and working, running on Bun 1.3.14 or later.
- `omp-tokenmunchers` **0.3.0 or later** installed, configured with `/munch login`, and reporting (`/munch status`).
- An external CLI installed, authenticated, and runnable noninteractively. The AGY example expects `agy -p PROMPT --mode=MODE --model MODEL --output-format json --print-timeout DURATION`. **Check `agy --help`**: external CLI options and output schemas may change.
- Bash, Python 3, and a working `python3` command in the shell running the wrapper. On Windows, Git Bash or WSL can be used if `agy` and Python are available in that environment.

The example does **not** install, authenticate, or bypass any provider CLI. Follow your provider's terms and organizational policies.

## 2. How the pieces fit

```text
OMP manager
  ├── code-builder subagent (may edit)
  │     └── agy-run.sh builder  < prompt on stdin
  └── code-scout subagent (read-only)
        └── agy-run.sh scout    < prompt on stdin
                   │
                   ├── AGY CLI → JSON response with usage
                   └── external-usage.jsonl (local, metadata only)
                              ↓
                   Tokenmunchers OMP plugin
                              ↓
                         dashboard
```

The manager's own OMP calls are recorded normally. Only the external CLI calls need a JSONL producer. Avoid also reporting the same external call through another collector.

## 3. Install the example files

Copy these files from the repository:

- [`examples/external-usage/agy-run.sh`](../examples/external-usage/agy-run.sh) to `~/.omp/agent/bin/agy-run.sh`.
- [`examples/external-usage/record-usage.py`](../examples/external-usage/record-usage.py) to `~/.omp/agent/bin/record-usage.py`.
- [`examples/external-usage/code-builder.md`](../examples/external-usage/code-builder.md) and [`code-scout.md`](../examples/external-usage/code-scout.md) to the OMP agent-definition directory you use (typically `~/.omp/agent/agents/`; confirm against your OMP version).

On Unix, run `chmod 700 ~/.omp/agent/bin/agy-run.sh`. On Windows, ensure Git Bash or WSL resolves the home directory to the same OMP agent directory used by the plugin. The example's recorder resolves `TOKENMUNCHERS_AGENT_DIR`, then `PI_CODING_AGENT_DIR`, then `~/.omp/agent`; set `TOKENMUNCHERS_AGENT_DIR` explicitly if your setup differs.

Review the example's `AGY_BUILDER_MODEL` and `AGY_SCOUT_MODEL` defaults; they are **illustrative requested model names**, not independently verified engine identities. Set these variables to model IDs actually supported by your installed CLI. The wrapper also uses `accept-edits` for builder and `plan` for scout; verify these modes have the intended permissions in your AGY installation.

The example is deliberately small: it does **not** reproduce a production wrapper's rate limits, concurrency locks, cooldowns, command allow-list, or 15/30-minute enforcement. Keep those protections if adapting an existing production wrapper. Do not replace a hardened wrapper just to add logging.

## 4. The stdin contract

The wrapper accepts **exactly one** positional argument: `builder` or `scout`. The prompt must be supplied on **stdin**. The quoted heredoc prevents shell interpolation:

```bash
bash ~/.omp/agent/bin/agy-run.sh builder <<'AGY_PROMPT'
Implement the assigned change. Only edit the named files.
AGY_PROMPT
```

```bash
bash ~/.omp/agent/bin/agy-run.sh scout <<'AGY_PROMPT'
Find the relevant functions and return paths and line numbers.
AGY_PROMPT
```

**Do not** use `bash ~/.omp/agent/bin/agy-run.sh builder "the prompt"`: the extra argument is refused. Do not call `agy` directly from these agents, or their usage may not be logged.

The builder/scout Markdown examples contain this contract. They are **templates**: merge their invocation rules into your existing definitions instead of overwriting model, tool, timeout, or safety settings.

## 5. How the usage record is produced

The shell wrapper reads stdin, runs AGY once, writes its original JSON output to stdout, and invokes `record-usage.py` on a temporary copy of that JSON. The recorder:

1. Parses a successful JSON response with a `usage` object and nonzero tokens.
2. Uses the CLI's `conversation_id` when available; otherwise a fresh UUID for that invocation. **A CLI-provided conversation ID is suitable only if unique per billable call**. If the CLI reuses it across turns, change the producer to use a per-call ID to avoid collapsing distinct events.
3. Appends one JSON line to `<agent dir>/tokenmunchers/external-usage.jsonl`, including `PI_SESSION_ID` and `PI_SESSION_FILE` if present.
4. Never writes prompts, responses, file paths, credentials, or raw CLI JSON into the usage event. `session_file` is a local-only correlation hint; the plugin does not upload it.

The wrapper keeps the AGY exit status. If recording fails, the example prints a warning to stderr but leaves the CLI result intact; **that call may be missing from Tokenmunchers**. Inspect permissions and the resolved agent directory if this happens. Do not report a guessed token count. A successful `agy` invocation without parseable usage also cannot be counted.

`thinking_tokens` in the observed AGY response are a subset of output usage: **do not add them to output a second time**. Token fields and cache semantics differ across CLIs; validate their meanings before adapting the recorder. The example uses `input_tokens`, `output_tokens`, and `cache_read_tokens` as returned by AGY. If your CLI's `input_tokens` already includes cached tokens, adjust to the plugin's disjoint-count convention to avoid double counting. The example omits cost rather than guessing it; the plugin defaults missing `cost_usd` to zero.

The event schema and deduplication rules are documented in [Usage from other tools](../plugin/README.md#usage-from-other-tools).

## 6. Verify end to end

From a new OMP session with the plugin enabled, invoke **one** harmless test through the actual subagent:

```bash
bash ~/.omp/agent/bin/agy-run.sh builder <<'AGY_PROMPT'
Reply with OMP_BUILDER_OK. Do not use tools or access files.
AGY_PROMPT
```

Check that the wrapper returns `status: SUCCESS` with a nonempty `response` and a `usage` object. Run `/munch status`; the `external usage waiting` count may be nonzero briefly. Allow approximately a minute for draining and delivery, then check the Tokenmunchers dashboard for a **new** external model event. Subagent attribution depends on `PI_SESSION_FILE` and the plugin's local session association; an ordinary shell invocation outside OMP may not be tagged as a subagent.

The same procedure can be repeated for `scout` using `agy-run.sh scout` if needed. Each live CLI test may consume substantial tokens, even for a short response.

For a no-provider-cost recorder check, run `python3 -m unittest discover -s examples/external-usage -p 'test_*.py'` from the repository root. This tests event generation with synthetic JSON and a temporary agent directory; it does not call AGY or the Tokenmunchers server.

## 7. Troubleshooting

| Symptom | Check |
| --- | --- |
| `extra arguments are not allowed` | Pass the prompt via stdin; only `builder` or `scout` is positional. |
| AGY returns a response, but no dashboard event | Confirm JSON has `usage` with nonzero tokens, recorder warnings, the correct agent directory, plugin 0.3.0+, and `/munch status`. |
| `external usage waiting: 0` but no event | Zero can mean the file was already drained, never written, or rejected; inspect warnings, plugin state and dashboard rather than treating zero as proof of delivery. |
| Unexpected missing or duplicate totals | Check unique per-call `id`, whether another reporter captured the call, and inclusive versus exclusive cache token accounting. |
| Wrong model label | The example logs the configured/requested model; AGY's actual selected engine may differ. Verify against provider metadata if available. |
| No subagent badge | Ensure the wrapper inherits `PI_SESSION_FILE` from the OMP subagent and the plugin can associate it locally. |
| Windows path mismatch | Check which `HOME` Git Bash/WSL uses; set `TOKENMUNCHERS_AGENT_DIR` to the directory OMP uses. |
| No events after `/munch pause` | Reporting may be intentionally paused. Resume only when appropriate. |
| Recorder warns about missing `usage` | Your CLI JSON schema may differ; adapt the parser to documented fields, not guessed values. |

**Privacy:** the plugin sends usage metadata only. The example's temporary JSON file may contain the CLI response; it is created with restrictive permissions where supported and removed when the wrapper exits. Do not run it in a shared writable directory or retain the raw JSON in logs.
