# Tokenmunchers Claude Code Plugin

Native Claude Code plugin for Tokenmunchers metadata-only token accounting, session tracking, and contribution sync.

---

## Overview

The Tokenmunchers Claude Code plugin provides:
- **Zero-Turn Leakage**: Strictly metadata-only accounting (token counts, cache statistics, session timestamps). Stdin from Claude Code hooks is completely ignored, ensuring prompt and completion privacy.
- **Native User Skill**: `/tokenmunchers:munch` (or `/munch`) slash command with `disable-model-invocation: true`, locking the skill to `user-invocable-only` so the model cannot autonomously invoke administrative or upload actions.
- **Bifurcated Explicit Consent**: History snapshot upload (`sync`) and recurring background delivery (`auto-on`) require separate, explicit human approval.
- **Shared Ledger Identities**: Reuses the standard ledger state (`~/.local/state/tokenmunchers-collector`), preserving cutoff dates, exclusion rules, and device identities across standalone CLI and plugin runs.
- **Node >= 26 Execution**: Leverages native Node.js TypeScript execution for the shared plugin runtime without intermediate compilation.

---

## Installation

### Safe Local Installation (Development & Testing)

Directly pointing Claude Code to a raw git checkout (`marketplace add .`) risks copying uncommitted development files, secrets (`.env`), or heavy build directories (`node_modules`) into the plugin cache.

To install locally, first stage a clean plugin tree into an explicit temporary directory using the packaging helper, then add that clean root:

```bash
# 1. Stage a clean, production-ready plugin root (requires Node >= 26)
node integrations/package-plugin.ts --output /tmp/tokenmunchers-clean

# 2. Add the staged clean directory as a local marketplace
claude plugin marketplace add /tmp/tokenmunchers-clean

# 3. Install the plugin
claude plugin install tokenmunchers@tokenmunchers

# 4. Restart Claude Code
# Restart your terminal session or Claude Code process so manifests and skills are registered.
```

### Remote Git Installation (Committed Release)

Once changes are committed and pushed to the repository:

```bash
# Add the GitHub repository as a marketplace
claude plugin marketplace add hndrxc/tokenmunchers

# Install the plugin
claude plugin install tokenmunchers@tokenmunchers

# Restart Claude Code
```

### Installed Cache Architecture & Zero Checkout Dependency

When installed, Claude Code copies the declared root source into its plugin cache (`~/.claude/plugins/cache/tokenmunchers/tokenmunchers/<version>`).
- **No Original Checkout Dependency**: The plugin runs self-contained from the installed cache (`${CLAUDE_PLUGIN_ROOT}`). The original git checkout is never required at runtime.
- **Direct Runtime Execution**: Hook triggers and the slash command execute the shared runtime at `<cache_root>/integrations/plugin/src/cli.ts` via Node >= 26.

---

## The `/tokenmunchers:munch` Skill

The plugin installs `/tokenmunchers:munch` (aliased to `/munch` when unambiguous), configured with `disable-model-invocation: true` and `user-invocable: true`.

This locks the skill to `user-invocable-only` in Claude Code's skill registry, preventing the model from autonomously invoking the `/tokenmunchers:munch` slash command; all accounting changes and uploads must be directly triggered by the human user.

### Working Directory & Path Resolution

When executing tools inside a skill invocation, Claude Code runs commands in the user's active project workspace (`CWD`), **not** in the plugin cache directory.

The skill instructions resolve the plugin cache root from the skill base directory supplied in the skill prompt preamble (`Base directory for this skill: <baseDir>`), resolving 4 parent directory levels up (`<baseDir>/../../../..`) using POSIX single-quoted escaping (`'<SHELL_ESCAPED_BASE_DIR>'` with embedded apostrophes escaped as `'"'"'`) to locate `<plugin_root>/integrations/plugin/src/cli.ts`. It does not rely on markdown `${CLAUDE_PLUGIN_ROOT}` expansion or relative paths from `CWD`.

---

## Command Reference & Examples

All commands run through the shared runtime CLI:
`node <plugin_root>/integrations/plugin/src/cli.ts <COMMAND> --source claude-code [OPTIONS]`

Optional options:
- `--state <dir>`: Custom state directory (default: `~/.local/state/tokenmunchers-collector`).
  - *Hook State Note*: Installed native lifecycle hooks use a static command without `--state`, always reading/writing `~/.local/state/tokenmunchers-collector`. The `--state` option applies only to that specific manual or test CLI invocation and does not persistently redirect installed hooks; running `auto-on --state CUSTOM` enables recurring consent in `CUSTOM` only, which default installed hooks do not use.
- `--endpoint <url>`: Contribution endpoint override (default: official contributions URL). Only passed upon explicit user request; never inferred.
### 1. `status`
Inspect current accounting configuration, active session history root, cutoff timestamp, exclusions, and pause state.
```bash
/tokenmunchers:munch status
```
- Safe, read-only, local only. Reads existing ledger state without scanning new files or making network requests.

### 2. `preview`
Run a fresh local metadata scan of Claude session history and display token and session totals without uploading.
```bash
/tokenmunchers:munch preview
```
- Local only, NO network calls. Scans session files and records new observations in the local SQLite ledger, then returns a compact metadata summary.

### 3. `activate`
Initialize local accounting for Claude Code with an explicit history directory.
```bash
/tokenmunchers:munch activate --root "$HOME/.claude/projects"
# With repeated exclusions:
/tokenmunchers:munch activate --root "$HOME/.claude/projects" --exclude "private-client" --exclude "sandbox"
```
- Requires an explicit `--root`. Does not perform implicit directory guessing.
- **Idempotency & Cutoff**: First invocation establishes the cutoff timestamp, root directory, and device identity. A subsequent invocation with identical root and exclusions returns the existing state, preserving the original cutoff timestamp and device identity. Calling activate with a changed root or changed exclusions throws an error (configuration is fixed once activated).
- Local only; does not upload anything.

### 4. `sync`
Scan session logs and upload a staged snapshot to the official contribution endpoint.
```bash
/tokenmunchers:munch sync
```
- Scans the source root into the ledger first, then delivers the staged snapshot.
- **Explicit Consent Guard**: Snapshot upload requires explicit user confirmation (`--confirm-upload`). If invoked without prior confirmation, the skill displays a preview of what will be uploaded and asks for approval before passing `--confirm-upload`.
- Returns delivery output containing snapshot ID (`snapshot_id`), accepted session count (`accepted`), resumed request count, and rate limit wait counts.
- Requires `TOKENMUNCHERS_API_KEY` exported in the environment.

### 5. `auto-on`
Enable recurring background upload delivery triggered by Claude Code lifecycle hooks (`SessionStart`, `SessionEnd`, `Stop`).
```bash
/tokenmunchers:munch auto-on
```
- **Separate Recurring Consent**: Requires separate explicit human consent before passing `--confirm-upload`.
- When enabled and `TOKENMUNCHERS_API_KEY` is present, background hooks will automatically upload session metadata snapshots.

### 6. `auto-off`
Revoke recurring background upload delivery.
```bash
/tokenmunchers:munch auto-off
```
- Background hooks remain active for local-only scanning and accounting, but network delivery is revoked.
- Preserves activation root, exclusions, cutoff timestamp, and local ledger history.

### 7. `pause` and `resume`
Temporarily suspend or resume accounting for Claude Code.
```bash
/tokenmunchers:munch pause
/tokenmunchers:munch resume
```
- **Per-Claude Scope**: Pausing affects only `claude-code`; other harnesses (such as Codex) continue tracking normally.
- **Permanent Exclusion**: Timestamps occurring within the paused interval are permanently excluded from accounting even after resuming.

---

## Privacy, Consent, & State Model

| Concept | Behavior |
| :--- | :--- |
| **Default Posture** | Zero scans and zero network uploads before explicit activation. |
| **Activation** | Strictly local ledger initialization; establishes cutoff and device identity; no network traffic. |
| **Status** | Truly read-only local inspection of existing ledger state. |
| **Preview** | Local collector scan persisting new observations in SQLite; local-only, no network traffic. |
| **Manual Sync Consent** | Uploading historical snapshots requires explicit user confirmation. |
| **Recurring Consent** | Automated background delivery (`auto-on`) requires separate explicit recurring consent. |
| **Auto-Off Revocation** | Disables recurring network calls immediately without altering ledger state or activation rules. |
| **Unknown vs Zero Unchanged** | Missing metrics and unobserved properties remain unknown/null rather than fabricated as zero; zero unchanged indicates an activated source observed zero new records on scan. |
| **Accepted-History Retention** | Retains acknowledged ledger records across sessions; accepted history is preserved in state and not deleted. |
| **Shared Ledger** | Uses `~/.local/state/tokenmunchers-collector` (manual override via `--state DIR`). Installed native hooks always use the default state directory; custom `--state` does not redirect hooks. |
| **Credentials** | Uses ONLY exported `TOKENMUNCHERS_API_KEY`. Never persist or paste API keys into chat. |

---

## Manual Verification Recipe (Optional)

To verify the plugin in an isolated environment without modifying user configuration or shared state:

```bash
# 1. Stage clean plugin directory
node integrations/package-plugin.ts --output /tmp/tm-claude-smoke

# 2. Add local marketplace and install
claude plugin marketplace add /tmp/tm-claude-smoke
claude plugin install tokenmunchers@tokenmunchers

# 3. In Claude Code, verify slash command discovery:
# /tokenmunchers:munch status --state /tmp/tm-test-state

# 4. Verify shared runtime directly with isolated test state and dummy history dir
mkdir -p /tmp/tm-test-dummy-history
node /tmp/tm-claude-smoke/integrations/plugin/src/cli.ts activate --source claude-code --root /tmp/tm-test-dummy-history --state /tmp/tm-test-state
node /tmp/tm-claude-smoke/integrations/plugin/src/cli.ts status --source claude-code --state /tmp/tm-test-state
node /tmp/tm-claude-smoke/integrations/plugin/src/cli.ts preview --source claude-code --state /tmp/tm-test-state

# 5. Uninstall plugin and clean up marketplace registration
claude plugin uninstall tokenmunchers
claude plugin marketplace remove tokenmunchers
```
