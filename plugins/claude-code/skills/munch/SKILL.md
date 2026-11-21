---
name: munch
description: Manage Tokenmunchers metadata accounting, activation, preview, status, manual sync, recurring auto-sync, and pause/resume for Claude Code.
user-invocable: true
disable-model-invocation: true
argument-hint: "<status|preview|activate|sync|auto-on|auto-off|pause|resume> [options]"
allowed-tools:
  - Bash
---

# /tokenmunchers:munch

Native administrative slash command skill for Tokenmunchers metadata accounting in Claude Code.

## Safety & Invariant Principles

1. **User Invocable Only (`disable-model-invocation: true`)**:
   - The frontmatter `disable-model-invocation: true` sets this skill to `user-invocable-only` in Claude Code's skill registry, preventing the model from autonomously invoking the `/tokenmunchers:munch` slash command. The assistant MUST NOT autonomously initiate administrative accounting workflows or upload actions without explicit user request.
2. **Metadata Privacy**:
   - Tokenmunchers collects token counts, cache statistics, and timestamps only.
   - Prompts, completions, code snippets, chat turns, and file contents are strictly ignored and never stored or transmitted.
   - Hook stdin from Claude Code is completely ignored and drained without parsing.
3. **Safe Default (No Implicit Network Traffic)**:
   - Installation does NO scans and NO network uploads before explicit activation.
   - Activation (`activate`) is strictly local and never makes network requests.
   - `status` is a read-only local operation that never makes network requests.
   - `preview` performs a local scan and records observations in the local SQLite ledger without making network requests.
4. **Bifurcated Consent for Uploads**:
   - Manual snapshot upload (`sync`) requires explicit user confirmation (`--confirm-upload`). The assistant MUST NEVER autonomously supply `--confirm-upload` without direct user approval in the current turn.
   - Recurring background delivery (`auto-on`) requires separate, explicit user recurring consent (`--confirm-upload`). Enabling `sync` does not imply `auto-on`, and enabling `auto-on` does not bypass approval for manual syncs.
5. **Credential Isolation**:
   - Authentication uses ONLY the externally exported environment variable `TOKENMUNCHERS_API_KEY`.
   - NEVER request the user paste their API key in chat.
   - NEVER echo, print, or expose the API key or dump environment variables in chat or shell output.
6. **No Raw Shell Interpolation & Safe Quoting**:
   - DO NOT pipe or `eval` `$ARGUMENTS`.
   - Parse user requests as discrete operation selections and validate/quote every path and flag argument independently.
   - Do NOT substitute raw path placeholders into double-quoted shell scripts if paths may contain spaces or shell metacharacters. Use robust per-argument passing.

---

## Locating the Shared Runtime CLI

When Claude Code loads this skill, it injects a preamble indicating the skill's base directory:
`Base directory for this skill: <baseDir>`
where `<baseDir>` is the absolute path to `.../plugins/claude-code/skills/munch` inside the installed plugin cache.

**Important**:
- The shell tool execution directory (`CWD`) is the user's current project workspace, NOT the plugin cache or repository root.
- Do NOT run `node ./integrations/plugin/src/cli.ts` (relative to CWD).
- Do NOT assume `${CLAUDE_PLUGIN_ROOT}` is expanded inside the markdown body of a skill.
- Derive the plugin root path by resolving 4 directory levels up from the skill base directory:
  `<baseDir>/../../../..`
  The shared runtime CLI is located at:
  `<plugin_root>/integrations/plugin/src/cli.ts`

To derive the path safely in Bash without shell metacharacter vulnerabilities (such as `$(...)`, backticks, spaces, or quotes in the installation directory), never substitute a raw `<baseDir>` inside double quotes. Wrap the literal base directory path in POSIX single quotes, escaping any embedded single quotes via the `'"'"'` sequence (e.g. `'<SHELL_ESCAPED_BASE_DIR>'`), and pass it as a distinct positional argument:
```bash
PLUGIN_CLI="$(node -e 'const path = require("node:path"); console.log(path.resolve(process.argv[1], "../../../..", "integrations/plugin/src/cli.ts"))' -- '<SHELL_ESCAPED_BASE_DIR>')"
```

Node requirement: **Node >= 26** (natively executes TypeScript files without pre-compilation).

---

## Operations & Command Handling

Translate user intent into one of the following safe commands. Always execute with `--source claude-code`.
Optional flags `--state DIR` and `--endpoint URL` may be passed to any command when requested.

### 1. Status (`status`)
Inspect current accounting configuration, active session history root, cutoff timestamp, exclusions, and pause state.
```bash
node "$PLUGIN_CLI" status --source claude-code [--state DIR]
```
- Safe, read-only, local only. Reads existing ledger state without scanning new files or making network requests.

### 2. Preview (`preview`)
Perform a fresh local scan of Claude session history and display token and session totals without uploading.
```bash
node "$PLUGIN_CLI" preview --source claude-code [--state DIR]
```
- Local only, NO network calls. Runs a local collector scan and records new observations in the local SQLite ledger, then returns a compact metadata summary.

### 3. Activate (`activate`)
Initialize local accounting for Claude Code with an explicit session history directory.
```bash
node "$PLUGIN_CLI" activate --source claude-code --root "<EXPLICIT_ROOT>" [--exclude "<PREFIX>"]... [--state DIR]
```
- `--root <path>`: Required explicit path to Claude session logs (e.g. `"$HOME/.claude/projects"`). Must be safely quoted.
- `--exclude <prefix>`: Optional repeated path prefixes to exclude from accounting (e.g. `--exclude "private-project"`).
- **Idempotency & Cutoff**: First invocation establishes the cutoff timestamp, root directory, and device identity. A subsequent invocation with identical root and exclusions returns the existing state, preserving the cutoff timestamp and device identity. Calling activate with a changed root or changed exclusions throws an error (configuration is fixed once activated).
- Local only; does NOT upload anything.

### 4. Manual Sync (`sync`)
Scan local session metadata and upload a staged snapshot to the official contribution endpoint.
```bash
node "$PLUGIN_CLI" sync --source claude-code --confirm-upload [--state DIR] [--endpoint URL]
```
- Scans the source root into the ledger first, then delivers the staged snapshot.
- **Explicit Consent Guard**: If the user did not explicitly approve uploading session history in their current turn, DO NOT pass `--confirm-upload`. Instead:
  1. Run `preview` first.
  2. Display the preview summary (tokens, sessions, quality).
  3. Inform the user of what will be uploaded and ask for explicit confirmation before running with `--confirm-upload`.
- Requires `TOKENMUNCHERS_API_KEY` exported in the user's environment.
- Returns delivery output containing `{ accepted, snapshot_id, resumed_at_request, rate_limit_waits }`.

### 5. Recurring Auto-Sync On (`auto-on`)
Enable recurring background upload delivery triggered by Claude Code lifecycle hooks (`SessionStart`, `SessionEnd`, `Stop`).
```bash
node "$PLUGIN_CLI" auto-on --source claude-code --confirm-upload [--state DIR] [--endpoint URL]
```
- **Explicit Consent Guard**: Requires separate explicit consent for recurring background network transmissions.
- Only pass `--confirm-upload` when the user has provided explicit recurring consent.

### 6. Recurring Auto-Sync Off (`auto-off`)
Revoke recurring background upload delivery.
```bash
node "$PLUGIN_CLI" auto-off --source claude-code [--state DIR]
```
- Disables recurring background uploads on hooks.
- Preserves activation root, exclusions, cutoff timestamp, and existing local ledger history. Hooks remain active for local-only scanning.

### 7. Pause Tracking (`pause`)
Temporarily pause token accounting for Claude Code.
```bash
node "$PLUGIN_CLI" pause --source claude-code [--state DIR]
```
- Pausing is source-scoped: pauses `claude-code` without affecting other harnesses (e.g. Codex).
- Timestamps occurring within the paused interval are permanently excluded from accounting, even after resuming.

### 8. Resume Tracking (`resume`)
Resume token accounting for Claude Code after a pause.
```bash
node "$PLUGIN_CLI" resume --source claude-code [--state DIR]
```
- Resumes tracking from the current timestamp. Periods during the pause remain permanently excluded.

---

## Endpoint & State Overrides

- Default state directory is `~/.local/state/tokenmunchers-collector`. Pass `--state <dir>` only when explicitly requested.
  - **Hook State Invariant**: Installed native lifecycle hooks use a static command without `--state`, always operating against the default directory (`~/.local/state/tokenmunchers-collector`). The `--state` option applies only to that explicit manual or test command invocation and does NOT persistently redirect installed hooks; running `auto-on --state CUSTOM` enables recurring consent in that custom state directory only, which default installed hooks do not use.
- Default endpoint is the official Supabase contributions URL. Pass `--endpoint <url>` only when explicitly requested by the user; never infer or invent alternative endpoints.

---

## Output Guidelines

- Present command output in a clean, human-readable summary:
  - Status/Preview: Show harness (`claude-code`), status (`active`, `paused`), session count, total tokens, cache stats, and unknown metric tallies.
  - Sync: Show delivery status, snapshot ID (`snapshot_id`), and accepted session count (`accepted`).
- Missing metrics and unobserved properties remain unknown/null; never fabricate zeros.
- Never output full raw JSON dumps unless specifically requested by the user.
- If an operation fails with a missing `TOKENMUNCHERS_API_KEY`, remind the user to export it in their environment (`export TOKENMUNCHERS_API_KEY="..."`); never invite the user to paste it into the conversation.
