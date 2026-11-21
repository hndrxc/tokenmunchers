---
name: munch
description: Show tokenmunchers accounting status, preview metadata, manage sync, and control accounting collector.
user-invocable: true
---

# Tokenmunchers Accounting Collector Skill (`munch`)

This skill provides user-invoked administrative controls and visibility for the Tokenmunchers metadata-only accounting collector in Codex.

Tokenmunchers reads accounting metadata from local session history files (token counts, model names, cache statistics, estimated costs, and timestamps). It operates strictly on accounting metrics and **never** collects, persists, or uploads user prompt text, source code, assistant responses, or tool call content.

Skill summaries preserve unknown or null accounting values rather than manufacturing artificial zero values. The collector strictly retains existing stable ledger identities, cutoffs, exclusions, and accepted history across runs.

## Invocation & Autonomy Rules

- **Explicit User Invocation Only**: This skill is configured with `policy.allow_implicit_invocation: false` in `agents/openai.yaml`, ensuring Codex does not inject the skill into the model prompt context by default. Combined with this skill's explicit procedural rules, the model **MUST NEVER** autonomously invoke this skill or execute its commands without direct, explicit user request (e.g. `$tokenmunchers-codex:munch status`, `$tokenmunchers-codex:munch preview`, `$tokenmunchers-codex:munch sync`).
- **No Inferred Consent**: The model **MUST NEVER** invent or infer consent for uploading data.
- **Credential Protection**: The API key (`TOKENMUNCHERS_API_KEY`) is read **only** from the process environment. The model **MUST NEVER** ask the user to paste an API key into chat, log an API key, or pass an API key as a command-line parameter. If the key is unset, instruct the user to export it in their terminal environment outside chat:
  ```bash
  export TOKENMUNCHERS_API_KEY="your-tokenmunchers-api-key"
  ```
  *Tool Environment Note*: Codex tool execution subshells (`exec_command`) may filter environment variables matching `*KEY*` or `*TOKEN*`. If `$tokenmunchers-codex:munch sync` in chat cannot access `TOKENMUNCHERS_API_KEY`, instruct the user to execute manual sync directly in their launch terminal outside Codex or enable recurring lifecycle hooks (`$tokenmunchers-codex:munch auto-on --confirm-upload`), which execute directly via Codex hooks with terminal environment inheritance.
- **Argument Safety**: Never interpolate raw user input into `eval` or arbitrary shell pipelines. Validate each subcommand and argument before running. Always quote path arguments and options like `--state "<DIR>"` and `--endpoint "<URL>"`.
---

## Runtime Path Resolution

Codex installs plugins into a local cache directory (typically `~/.codex/plugins/cache/tokenmunchers/tokenmunchers-codex/<version>/`).

To locate and execute the collector CLI:
1. **Derive Installed Root**: Identify the absolute path of this loaded `SKILL.md` file (for example, `/path/to/cache/.../plugins/codex/skills/munch/SKILL.md`).
2. Strip the trailing `/plugins/codex/skills/munch/SKILL.md` suffix to determine `<INSTALLED_ROOT>`.
3. If the absolute path to `SKILL.md` cannot be determined from the loaded file context, **STOP** immediately and report an error to the user. **NEVER** guess, assume current working directory (`process.cwd()`), or fall back to an arbitrary repository checkout.
4. The runtime entry point is:
   ```text
   <INSTALLED_ROOT>/integrations/plugin/src/cli.ts
   ```
5. **Execution Command**: Always quote the script path and invoke with Node (requires Node >= 26):
   ```bash
   node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" <SUBCOMMAND> --source codex [OPTIONS]
   ```
6. Do not assume `${CLAUDE_PLUGIN_ROOT}` is exported in the interactive tool execution shell; use the derived `<INSTALLED_ROOT>`. Do not log or echo full internal paths into chat unnecessarily.

---

## Supported Subcommands & Procedures

### 1. `status`
Displays current collector configuration, activation state, device identity, auto-upload consent, pause status, and ledger summary.
- **Local Operation**: Does not scan or upload. Completely local and safe.
- **Accounting Fidelity**: Preserves unknown/null accounting metrics without manufacturing artificial zero values.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" status --source codex [--state "<DIR>"]
  ```

### 2. `preview`
Scans local session files into the local ledger and outputs a compact JSON summary of token counts, model breakdown, quality metrics, and ledger totals.
- **Local Operation**: Scans sessions locally but does not upload. Preserves null values where cost or token metrics are not available.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" preview --source codex [--state "<DIR>"]
  ```

### 3. `activate`
Registers and configures the explicit source root directory and optional exclusions for Codex session history.
- **Explicit Root Required**: The user **must** provide the source session directory explicitly via `--root "<PATH>"` (e.g. `~/.codex/sessions`). The model **MUST NEVER** guess or auto-select a root path.
- **Optional Exclusions**: Accepts one or more `--exclude "<PREFIX>"` arguments to ignore specific subdirectories or file prefixes.
- **Configuration Only**: Initializes root path and exclusions only. Does NOT scan or deliver. Completely local and safe.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" activate --source codex --root "<EXPLICIT_ROOT>" [--exclude "<PREFIX>" ...] [--state "<DIR>"]
  ```

### 4. `sync`
Scans local session files for new usage records and uploads pending contributions to the Tokenmunchers backend.
- **Explicit Confirmation Required**: Manual sync uploads historical metadata. The user must provide explicit approval. If the user typed `$tokenmunchers-codex:munch sync` without confirmation, ask for confirmation before executing. Once confirmed, pass `--confirm-upload`:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" sync --source codex --confirm-upload [--state "<DIR>"] [--endpoint "<URL>"]
  ```
- **Difference from Standalone CLI**: Plugin `sync` performs a local scan first, catching up any unindexed turns before delivering pending records.
- **Paused Guard**: If Codex is currently paused, manual sync will throw an error (`Source paused; resume before delivery`).
- **Prerequisite**: Requires `TOKENMUNCHERS_API_KEY` in the environment.

### 5. `auto-on`
Enables recurring background uploads on session lifecycle hooks (such as `SessionEnd`).
- **Separate Opt-In Required**: Recurring background upload is completely independent of activation. Enabling auto-on requires explicit user consent separate from activating the collector.
- **Explicit Confirmation Required**: The command strictly requires `--confirm-upload` and an activated source:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" auto-on --source codex --confirm-upload [--state "<DIR>"] [--endpoint "<URL>"]
  ```
- **Credential Timing**: `TOKENMUNCHERS_API_KEY` is NOT required at configuration time for `auto-on`; the environment key is required only when hooks actually trigger network delivery.

### 6. `auto-off`
Revokes recurring background upload consent.
- **Effect**: Disables automated uploads during session lifecycle hooks. Revokes upload consent cleanly without resetting device identity, ledger records, or accepted history. Never requires reactivation or state reset.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" auto-off --source codex [--state "<DIR>"]
  ```

### 7. `pause`
Temporarily suspends accounting collection for Codex only.
- **Effect**: While paused, lifecycle hooks skip delivery, manual `sync` throws `Source paused; resume before delivery`, and turns during paused intervals are excluded. Preserves existing ledger history and identity. Does not affect other harnesses.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" pause --source codex [--state "<DIR>"]
  ```

### 8. `resume`
Resumes accounting collection for Codex after a pause.
- **Command**:
  ```bash
  node "<INSTALLED_ROOT>/integrations/plugin/src/cli.ts" resume --source codex [--state "<DIR>"]
  ```
---

## Options Reference

- `--state <DIR>`: Custom state directory. Defaults to `~/.local/state/tokenmunchers-collector`. Note: `--state` applies only to explicit manual/test command invocations; static installed hooks omit `--state` and always use default `~/.local/state/tokenmunchers-collector`. `auto-on` configured with custom `--state` does NOT redirect installed hook state/consent.
- `--endpoint <URL>`: Custom ingest endpoint URL. Defaults to `https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions`.
- `--confirm-upload`: Required explicit confirmation flag for `sync` and `auto-on`.
