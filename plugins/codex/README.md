# Tokenmunchers Codex Plugin (`tokenmunchers-codex`)

The Tokenmunchers Codex plugin provides metadata-only accounting and visibility for AI token usage, model distribution, cache efficiency, and estimated costs in OpenAI Codex CLI sessions.

---

## Key Invariants & Privacy Architecture

- **Metadata Only**: Tokenmunchers reads accounting metadata from local session history files (token counts, model names, cache statistics, estimated costs, and timestamps). It operates strictly on accounting metrics and **never** collects, persists, or uploads user prompt text, source code, assistant responses, or tool call content.
- **Zero Inferred Upload Consent**:
  - **Installation** performs no contribution uploads (marketplace commands may perform harness-level registry queries depending on configuration, but never upload collector contributions).
  - **Activation** (`activate --root ...`) configures the explicit session root and exclusions only; does NOT scan session files or upload data.
  - **Status** (`status`) inspects existing configuration and ledger records without scanning or uploading.
  - **Preview** (`preview`) scans local session files into the local ledger without uploading.
  - **Manual Upload** (`sync`) scans local files and uploads only with explicit user confirmation (`--confirm-upload`).
  - **Recurring Upload** (`auto-on`) enables background uploads on lifecycle hooks only after explicit, separate user opt-in (`--confirm-upload`).
  - **Auto-Off** (`auto-off`) immediately revokes recurring uploads without resetting device identities or ledger records.
- **Credential Safety**: `TOKENMUNCHERS_API_KEY` is loaded exclusively from the process environment (`export TOKENMUNCHERS_API_KEY="..."`). It is **never** written to configuration files, passed as CLI flags, logged, or requested in chat.
  - *Tool Environment Policy Note*: In Codex CLI 0.154.0, shell environment evaluation follows `inherit` -> default excludes -> custom excludes -> `set` -> `include_only`, where `include_only` cannot un-exclude variables removed by an exclusion stage (see upstream source at [`config_types.rs#L206-L270`](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/protocol/src/config_types.rs#L206-L270) and [`shell_environment.rs#L98-L159`](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/protocol/src/shell_environment.rs#L98-L159)). While builtin defaults specify `inherit = "All"` and `ignore_default_excludes = true`, host or sandbox configurations that enforce credential filtering may prevent tool subshells (`exec_command`) from inheriting `TOKENMUNCHERS_API_KEY`. Rather than modifying global shell policies, users in filtered environments can execute manual sync directly in their launch terminal (`node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" sync --source codex --confirm-upload`) or rely on recurring lifecycle hooks (`auto-on`), which run directly under the parent terminal's inherited environment.
- **Void Stdin**: Session hooks receive session turns on standard input from Codex. The hook runner voids and drains `process.stdin` immediately without parsing or logging prompt/transcript content.
- **Shared Ledger & Identity**: State is maintained in `~/.local/state/tokenmunchers-collector` (configurable via `--state`). The SQLite ledger preserves identities, cutoffs, exclusions, and accepted history across standalone CLI and plugin invocations, preserving unknown/null values rather than manufacturing artificial zeros.
---

## Architecture & Packaging

- **Root-Source Packaging**: The plugin is packaged using root-source distribution (`source: { source: "local", path: "." }` in `.agents/plugins/marketplace.json`).
- **Cache Containment**: When installed, Codex copies the packaged source into its local cache (`~/.codex/plugins/cache/tokenmunchers/tokenmunchers-codex/0.1.0/`). Because the repository root contains the shared `integrations/` engine, the cached plugin has full access to the shared collector and ledger runtime without external runtime dependencies or duplicate code bundles.
- **No Original Checkout Dependency**: Once installed into the Codex cache, the plugin is completely self-contained and does not depend on the original git repository checkout.
- **No Background Daemon**: Lifecycle hooks and skills execute as lightweight, one-shot Node processes. No persistent daemon or background service is spawned.
- **Runtime Requirement**: Node.js >= 26 is mandatory (enables direct execution of TypeScript entry points without a build step).

---

## Installation & Safe Staging

### Important: Local Staging vs Direct Repository Install

When running `codex plugin add` from a local directory, Codex recursively copies the target directory into the plugin cache. If run directly against a working repository checkout (`.`), Codex will copy all files—including local `.env` files, `.git/`, and `node_modules/`.

To ensure a clean installation without sensitive or extraneous files, stage a clean plugin distribution first using `integrations/package-plugin.ts`:

```bash
# 1. Create an explicit, empty staging directory
export PLUGIN_DIR="/tmp/tokenmunchers-codex-staging"
mkdir -p "$PLUGIN_DIR"

# 2. Stage clean plugin artifacts (manifests, hooks, skills, and shared runtime)
node integrations/package-plugin.ts --output "$PLUGIN_DIR"

# 3. Register the clean directory as a local marketplace
codex plugin marketplace add "$PLUGIN_DIR" --json

# 4. Add the plugin from the marketplace
# Note: Codex CLI 0.154.0 uses 'add', NOT 'install'
codex plugin add tokenmunchers-codex@tokenmunchers --json

# Note: Retain $PLUGIN_DIR on disk. Codex requires the marketplace source path to
# remain accessible for future plugin updates and re-installation.

*(Note: Installing directly from a remote Git URL is safe for committed code; during local development and for uncommitted changes, always stage via `integrations/package-plugin.ts` as shown above).*

---

## Hook Trust Model

Codex validates hook commands against cryptographic SHA-256 hashes stored in `config.toml` (`hooks.state.<id>.trusted_hash`):

1. **Interactive Review**: On first execution or when hook definitions change, Codex interactively prompts the user in the terminal to inspect and approve the hook commands.
2. **Never Bypass Hook Trust Globally**: Users must review and approve hooks interactively. Do **not** instruct users to run with `--dangerously-bypass-hook-trust` in normal workflows. (That flag is reserved exclusively for non-interactive test harnesses).

---

## Hook Runtime & Lifecycle Events

Configured in `plugins/codex/hooks.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "node \"${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts\" hook --source codex",
            "async": true
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "node \"${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts\" hook --source codex",
            "async": true,
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

- **`SessionStart`**: Verifies collector state, performs catch-up scan of any unindexed sessions, and skips network delivery unless auto-upload is active and credentials are set.
- **`SessionEnd`**: Scans session turn usage flushed at session completion. If recurring upload (`auto-on`) is enabled and `TOKENMUNCHERS_API_KEY` is present, delivers pending metadata.
- **Transcript Flush Guarantee**: Codex flushes session transcript and turn usage to disk before `SessionEnd` hooks fire. If an abrupt crash prevents flushing final turns, the subsequent `SessionStart` or manual `$tokenmunchers-codex:munch sync` catches up.
- **Lifecycle Execution & Shutdown Timeout**: `SessionStart` runs asynchronously in the background. Although configured `async: true`, Codex 0.154.0 runs `SessionEnd` synchronously during teardown. Its `timeout: 10` setting limits the hook's shutdown wait to 10 seconds; it does not make teardown non-blocking. Interrupted or timed-out uploads remain staged and retryable in the local SQLite ledger. A subsequent eligible `SessionStart` or explicitly confirmed manual sync can resume delivery, subject to upload consent, credentials, and pause state.
- **Atomic Concurrency Lock**: Hooks acquire a generic per-state+harness SQLite process lock (`<state>/${harness}.lock.sqlite` holding `BEGIN IMMEDIATE`). If another collector process is running, the hook skips execution cleanly, and the operating system automatically releases the lock on process exit.
---

## User Skill (`$tokenmunchers-codex:munch`)

The plugin registers a native user-invocable skill at `plugins/codex/skills/munch/SKILL.md`.
- **Plugin Namespacing**: Codex automatically prefixes installed plugin skill names with the plugin identifier (`<plugin-name>:<skill-name>`). Although the local directory is `munch` and frontmatter specifies `name: munch`, the native skill loader registers it under the canonical namespace `tokenmunchers-codex:munch`.
- **Invocation**:
  - **Interactive TUI**: In interactive Codex sessions, typing `$` opens the native skill picker where users can select the registered `tokenmunchers-codex:munch` entry, or invoke directly by typing `$tokenmunchers-codex:munch [subcommand] [options]`.
  - **Headless / Non-Interactive**: In headless or non-interactive execution, invoke via `codex exec '$tokenmunchers-codex:munch [subcommand] [options]'` (for example, `codex exec '$tokenmunchers-codex:munch status'`).
- **Policy & Invocation Rule**: Configured with `policy: { allow_implicit_invocation: false }` in `plugins/codex/skills/munch/agents/openai.yaml`, so the skill is not injected into the model prompt context by default. Combined with the skill's explicit rule requiring direct user instruction, the assistant must not invoke these operations without an explicit user command (e.g. `$tokenmunchers-codex:munch`).
- **Installed Root Resolution**: The skill discovers the runtime entry point by inspecting the loaded `SKILL.md` file path, stripping `/plugins/codex/skills/munch/SKILL.md`, and invoking `<INSTALLED_ROOT>/integrations/plugin/src/cli.ts`. It does not rely on current working directory or environment variables.

### Commands

| Command | Purpose | Network Effect | Requires API Key |
| :--- | :--- | :---: | :---: |
| `$tokenmunchers-codex:munch activate --root "<DIR>" [--exclude "<PREFIX>" ...]` | Configure session root and optional exclusions. | None (config only, no scan) | No |
| `$tokenmunchers-codex:munch status` | Show current configuration, consent, and ledger totals. | None (local inspect, no scan) | No |
| `$tokenmunchers-codex:munch preview` | Scan sessions and output compact metadata summary. | None (local scan, no upload) | No |
| `$tokenmunchers-codex:munch sync --confirm-upload` | Scan sessions and deliver staged records. | Uploads metadata | Yes (in env) |
| `$tokenmunchers-codex:munch auto-on --confirm-upload` | Enable recurring background uploads on hooks. | Uploads on hooks | At delivery time |
| `$tokenmunchers-codex:munch auto-off` | Revoke recurring background upload consent. | None | No |
| `$tokenmunchers-codex:munch pause` | Temporarily pause accounting collection for Codex. | None | No |
| `$tokenmunchers-codex:munch resume` | Resume accounting collection for Codex. | None | No |

*(Note: Manual `sync` while paused throws `Source paused; resume before delivery`, whereas background hooks skip delivery cleanly).*

### Options Reference

- `--state <DIR>`: Custom state directory. Defaults to `~/.local/state/tokenmunchers-collector`. Note: `--state` applies only to explicit manual/test command invocations; static installed hooks omit `--state` and always use default `~/.local/state/tokenmunchers-collector`. `auto-on` configured with custom `--state` does NOT redirect installed hook state/consent.
- `--endpoint <URL>`: Custom ingest endpoint URL. Defaults to `https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions`.
- `--confirm-upload`: Required explicit confirmation flag for `sync` and `auto-on`.

## Safe Isolated Smoke Recipe (Unrun / Verification)

To verify installation, native skill discovery, hook registration, and CLI execution without affecting user configuration or default state:

```bash
# 1. Prepare isolated environment and sandbox directories
export SANDBOX_ROOT="$(mktemp -d /tmp/codex-sandbox-XXXXXX)"
# Isolate user HOME so hooks and unflagged commands resolve default ~/.local/state in sandbox:
export HOME="${SANDBOX_ROOT}/user-home"
export CODEX_HOME="${SANDBOX_ROOT}/home"
export STAGING_DIR="${SANDBOX_ROOT}/staging"
export STATE_DIR="${SANDBOX_ROOT}/state"
export SESSIONS_DIR="${SANDBOX_ROOT}/sessions"
mkdir -p "$HOME" "$CODEX_HOME" "$STAGING_DIR" "$STATE_DIR" "$SESSIONS_DIR"

# Guarantee no uploads during smoke test
unset TOKENMUNCHERS_API_KEY
# 2. Stage clean plugin distribution
node integrations/package-plugin.ts --output "$STAGING_DIR"

# 3. Add marketplace and plugin to isolated CODEX_HOME
codex plugin marketplace add "$STAGING_DIR" --json
codex plugin add tokenmunchers-codex@tokenmunchers --json

# 4. Verify plugin is listed in Codex
codex plugin list

# 5. Switch current working directory to sandbox root to prove zero checkout dependency
cd "$SANDBOX_ROOT"
CACHE_ROOT="${CODEX_HOME}/plugins/cache/tokenmunchers/tokenmunchers-codex/0.1.0"

# 6. Verify native hook and skill registration interactively in Codex:
# Note: Exporting HOME ensures hooks and default interactive $tokenmunchers-codex:munch commands resolve
# state to "$HOME/.local/state/tokenmunchers-collector" inside the sandbox.
#
# Option A: Headless non-interactive execution:
codex exec '$tokenmunchers-codex:munch status'
#
# Option B: Interactive Codex session:
codex
# - When Codex prompts for hook trust on first run, review the SessionStart/SessionEnd
#   hook commands and approve them interactively (never pass --dangerously-bypass-hook-trust).
# - In the Codex prompt, type '$' to open the skill picker and select 'tokenmunchers-codex:munch'.
# - Verify skill registration, status, and explicit root activation:
#     $tokenmunchers-codex:munch status
#     $tokenmunchers-codex:munch activate --root "<absolute path of SESSIONS_DIR sandbox>"
#     $tokenmunchers-codex:munch preview
#   (Notice no upload occurs; activation configures root only, preview performs local scan).
# - Exit the interactive session (/exit).
# 7. Programmatic verification: test pre-activation hook no-op (exits cleanly without error)
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" hook --source codex --state "$STATE_DIR"

# 8. Activate accounting for the isolated session root (configures root only, does not scan)
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" activate --source codex --root "$SESSIONS_DIR" --state "$STATE_DIR"

# 9. Check collector status (inspects config without scanning)
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" status --source codex --state "$STATE_DIR"

# 10. Check preview (performs local scan into ledger, outputs compact metadata JSON)
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" preview --source codex --state "$STATE_DIR"

# 11. Test pause, verify hook skips delivery while paused, and resume
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" pause --source codex --state "$STATE_DIR"
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" hook --source codex --state "$STATE_DIR"
node "${CACHE_ROOT}/integrations/plugin/src/cli.ts" resume --source codex --state "$STATE_DIR"

# 12. Clean up sandbox
rm -rf "$SANDBOX_ROOT"
```
