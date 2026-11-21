# Native Harness Plugin Specification: Claude Code and Codex

Checked: 2026-10-27.  
Scope: Primary-source analysis of installed Claude Code and Codex CLI binaries to establish native plugin packaging, marketplace manifests, hook runtime semantics, path interpolation, non-blocking upload triggers, and security/privacy guarantees. No application source modifications or test runs.

---

## 1. Grounded Primary Sources & Installed Versions

| Attribute | Claude Code | Codex CLI |
| :--- | :--- | :--- |
| **Installed Version** | `2.1.312` | `0.154.0` |
| **Binary Location** | `/home/carter/.local/share/claude/versions/2.1.312` | `/home/carter/.nvm/versions/node/v26.8.1/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex` |
| **Runtime Architecture** | Compiled Bun executable (`/$bunfs/root/`) | Native Rust executable with Tokio async engine |
| **Plugin Subcommands** | `claude plugin [install\|list\|marketplace\|validate\|details\|enable\|disable]` | `codex plugin [add\|list\|marketplace\|remove]` |
| **Marketplace Subcommands** | `claude plugin marketplace [add\|list\|remove\|update]` | `codex plugin marketplace [add\|list\|upgrade\|remove]` |

### Critical CLI Differences Verified by Execution:
- **Codex Install Command**: Codex CLI 0.154.0 does **not** support `codex plugin install`. The install command is `codex plugin add <plugin>@<marketplace>`.
- **Codex Marketplace Source**: Passing a file path to `codex plugin marketplace add` fails with `Error: local marketplace source must be a directory, not a file`. The source argument must point to the directory containing `.agents/plugins/marketplace.json`.

---

## 2. Packaging Architecture: Root Source (`source: "."`)

### Why Root Source is Required
When Codex installs a plugin from a marketplace entry, it copies the directory declared in `source.path` into its local cache (`~/.codex/plugins/cache/<marketplace>/<plugin>/<version>`). If a plugin is packaged from a subfolder (such as `./plugins/claude-code`), the cached copy isolates that subfolder, breaking all relative imports (`../../integrations/plugin/src/cli.ts`) with `MODULE_NOT_FOUND`.

By setting the marketplace plugin source to `"."` in both manifests, both harnesses copy/link the full repository root into the plugin environment. Shared collector engines, adapters, and `package.json` remain in-tree without code duplication.

### Raw Local Copy Hazard in Codex (Observed Verification)
Empirical testing of `codex plugin add` on a local path demonstrated that Codex performs a raw recursive directory copy into `~/.codex/plugins/cache/` without evaluating `.gitignore`. If `.git`, `.env`, local plans, or `node_modules` exist in the target directory, they are copied into the user's plugin cache.

**Safe Local Packaging Workflow**:
Because raw repository copying can leak uncommitted secrets or heavy build artifacts, local installation must stage only clean distributable assets into an explicit output directory:
```bash
PLUGIN_DIR="$(mktemp -d /tmp/tokenmunchers-plugin-XXXXXX)"
node integrations/package-plugin.ts --output "$PLUGIN_DIR"
```
The packaged directory contains both harness manifests (`.claude-plugin/` and `.agents/plugins/` + `.codex-plugin/`), platform-specific hooks/skills (`plugins/claude-code/`, `plugins/codex/`), the shared runtime CLI (`integrations/plugin/src/cli.ts`), shared engines, and root `package.json`. It excludes `.git`, `.env`, `node_modules`, planning documents, and test fixtures.
---

## 3. Marketplace Manifests

### Claude Code: `.claude-plugin/marketplace.json`
- `name` (*string*, required): Marketplace catalog identifier (e.g. `"tokenmunchers"`).
- `owner` (*object*, required): Must be an object (`owner: { "name": "Tokenmunchers" }`).
  - *Verification*: `owner.email` is **OPTIONAL**. Tested `owner: { "name": "Tokenmunchers" }` with `claude plugin validate` — passed with 0 errors. Do not invent an email.
- `plugins` (*array*, required): Array of plugin definitions.
  - `name` (*string*, required): Plugin identifier (`tokenmunchers`).
  - `source` (*string*, required): Path relative to marketplace root. `"."` is valid. Paths beginning with `../` are strictly rejected.
  - `description` (*string*, optional).

```json
{
  "name": "tokenmunchers",
  "owner": {
    "name": "Tokenmunchers"
  },
  "description": "Tokenmunchers metadata accounting collector plugins",
  "plugins": [
    {
      "name": "tokenmunchers",
      "source": ".",
      "description": "Tokenmunchers metadata-only accounting collector for Claude Code"
    }
  ]
}
```

### Codex: `.agents/plugins/marketplace.json`
- `name` (*string*, required): Catalog identifier (`"tokenmunchers"`).
- `interface` (*object*, optional): Presentation metadata (`displayName`).
- `plugins` (*array*, required): Ordered array of plugin entries.
  - `name` (*string*, required): Matches `plugin.json` (`tokenmunchers-codex`).
  - `source` (*object*, required): `{"source": "local", "path": "."}`.
  - `policy` (*object*, required):
    - `installation` (*string*): `"AVAILABLE"` (or `"INSTALLED_BY_DEFAULT"`, `"NOT_AVAILABLE"`).
    - `authentication` (*string*): `"ON_INSTALL"` (or `"ON_USE"`).
  - `category` (*string*, required): Display bucket (e.g. `"Productivity"`).

```json
{
  "name": "tokenmunchers",
  "interface": {
    "displayName": "Tokenmunchers"
  },
  "plugins": [
    {
      "name": "tokenmunchers-codex",
      "source": {
        "source": "local",
        "path": "."
      },
      "policy": {
        "installation": "AVAILABLE",
        "authentication": "ON_INSTALL"
      },
      "category": "Productivity"
    }
  ]
}
```

---

## 4. Plugin Manifests

### Claude Code: `.claude-plugin/plugin.json`
- `name` (*string*, required): `"tokenmunchers"`.
- `version` (*string*, required): Strict SemVer (`0.1.0`).
- `description` (*string*, required).
- `author` (*object*, optional): `{"name": "Tokenmunchers"}`.
- `license` (*string*, optional): `"MIT"`.
- `hooks` (*string*, optional): `"./plugins/claude-code/hooks.json"`.
- `skills` (*string*, optional): `"./plugins/claude-code/skills"`.
  *Primary source citation (Bun bundle string)*:
  > "Path to a skill directory, relative to the plugin root ("." / "./" denote the plugin root itself). Loaded in addition to the skills/ directory (except: for a marketplace entry whose source resolves to the marketplace root, declaring a specific subdirectory replaces the skills/ scan)."

```json
{
  "name": "tokenmunchers",
  "version": "0.1.0",
  "description": "Tokenmunchers metadata-only accounting collector for Claude Code",
  "author": {
    "name": "Tokenmunchers"
  },
  "license": "MIT",
  "hooks": "./plugins/claude-code/hooks.json",
  "skills": "./plugins/claude-code/skills"
}
```

### Codex: `.codex-plugin/plugin.json`
- `name` (*string*, required): `"tokenmunchers-codex"`.
- `version` (*string*, required): Strict SemVer (`0.1.0`).
- `description` (*string*, required).
- `author` (*object*, required): `{"name": "Tokenmunchers"}`.
- `license` (*string*, optional): `"MIT"`.
- `hooks` (*string*, optional): `"./plugins/codex/hooks.json"`.
- `skills` (*string*, optional): `"./plugins/codex/skills"`.
- `interface` (*object*, required): Required by workspace ingestion schema.
  - `displayName`: `"Tokenmunchers Codex"`
  - `shortDescription`: `"Metadata-only token accounting collector"`
  - `developerName`: `"Tokenmunchers"`
  - `category`: `"Productivity"`
  - `capabilities`: `["Interactive"]`

```json
{
  "name": "tokenmunchers-codex",
  "version": "0.1.0",
  "description": "Tokenmunchers metadata-only accounting collector for Codex",
  "author": {
    "name": "Tokenmunchers"
  },
  "license": "MIT",
  "hooks": "./plugins/codex/hooks.json",
  "skills": "./plugins/codex/skills",
  "interface": {
    "displayName": "Tokenmunchers Codex",
    "shortDescription": "Metadata-only token accounting collector",
    "developerName": "Tokenmunchers",
    "category": "Productivity",
    "capabilities": ["Interactive"]
  }
}
```

---

## 5. Hook Configuration & Runtime Mechanics

### Shell String vs. Exec Form
- **Claude Code**: Supports both exec form (`command` + `args: string[]`) and shell form (`command: string`).
- **Codex**: Primary source Rust schema for `HookHandlerConfig::Command` contains strictly:
  ```rust
  struct Command {
      command: String,
      command_windows: Option<String>,
      timeout: Option<u64>,
      async: bool,
      status_message: Option<String>,
      additional_context_limit: Option<usize>,
  }
  ```
  **Codex has NO `args` array in its command hook schema.**
- **Unified Portable Solution**: Use a quoted shell command string across both harnesses:
  `node "${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts" hook --source <harness>`
  Wrapping `${CLAUDE_PLUGIN_ROOT}` in double quotes ensures safe execution even if paths contain spaces.

### Background Execution vs. Synchronous Teardown (`"async": true`)
- **Claude Code**: Async command hooks detach as `async_hook_<pid>`, immediately close stdin, and return `{ backgrounded: true }` without blocking UI or turn progression.
- **Codex (`SessionStart` vs. `SessionEnd`)**:
  - `SessionStart` hooks run asynchronously in the background when `async: true`, avoiding startup stalls.
  - **Forced Synchronous Teardown on `SessionEnd`**: During session exit, the Codex runtime explicitly runs async `SessionEnd` hooks synchronously (emitting `running async SessionEnd hook synchronously in ...hooks.json`) to prevent the CLI process from terminating before teardown tasks complete.
  - **Timeout Bounding**: Because `SessionEnd` executes synchronously during process teardown, its execution must be strictly bounded with `timeout: 10` (seconds). If network synchronization times out or is interrupted, the un-uploaded staged snapshot remains safely committed in the local SQLite ledger and automatically resumes on the subsequent `SessionStart`.
  - *Invariant*: Not all async hooks are non-blocking; `SessionStart` is non-blocking backgrounded, while `SessionEnd` is forced synchronous in Codex during session exit.

### SessionEnd Ordering & Usage Metric Flush
In Codex (`core/src/turn_timing.rs` / `core/src/hook_runtime.rs`), the binary logs:
> `failed to flush transcript before SessionEnd hook: ...`

This confirms that Codex **explicitly flushes session transcripts and turn usage to disk before calling the `SessionEnd` hook**. When `SessionEnd` runs, the accounting files are complete.
### Concrete Hook Configurations

#### Claude Code (`plugins/claude-code/hooks.json`):
```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "node \"${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts\" hook --source claude-code",
            "async": true,
            "timeout": 30
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "node \"${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts\" hook --source claude-code",
            "async": true,
            "timeout": 30
          }
        ]
      }
    ]
  }
}
```

#### Codex (`plugins/codex/hooks.json`):
```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "node \"${CLAUDE_PLUGIN_ROOT}/integrations/plugin/src/cli.ts\" hook --source codex",
            "async": true,
            "timeout": 30
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

---

## 6. Skill Frontmatter & Working Directory Semantics

### Working Directory Hazard & Skill Path Resolution
When a user invokes a skill (`/tokenmunchers:munch` in Claude Code, `$tokenmunchers-codex:munch` in Codex), the model tool shell executes with the **user's active project directory as CWD**, NOT the installed plugin cache directory.

While harness documentation describes `${CLAUDE_PLUGIN_ROOT}` expansion in hook process environments and configuration fields, skill markdown instructions run inside LLM prompt context where environment expansion can vary across tools.

To guarantee robust execution regardless of active workspace CWD or prompt parsing, the skill implementation conservatively derives the absolute plugin root by resolving `../../../../integrations/plugin/src/cli.ts` from the loaded `SKILL.md` location (or stripping `/plugins/<harness>/skills/munch/SKILL.md` from the file path).
### Frontmatter Conventions
- **Claude Code**:
  ```markdown
  ---
  name: munch
  description: Show tokenmunchers accounting status and preview
  user-invocable: true
  ---
  ```
- **Codex**:
  In Codex, `SKILL.md` frontmatter validator rejects `disable-model-invocation: true` (`must be false`).
  Instead, Codex defines user-only explicit invocation in `agents/openai.yaml` within the skill directory (`plugins/codex/skills/munch/agents/openai.yaml`):
  ```yaml
  interface:
    display_name: "Munch"
    short_description: "Show tokenmunchers accounting status and preview"

  policy:
    allow_implicit_invocation: false
  ```
  *Citation from Codex binary (`ext/skills/src/host_roots.rs`)*:
  > "policy.allow_implicit_invocation: When false, the skill is not injected into the model context by default, but can still be invoked explicitly via `$skill`. Defaults to true."
  *Live Execution Evidence*: Codex `skills/list` registers the plugin skill under the canonical namespaced identifier `tokenmunchers-codex:munch` (`enabled: true`, `pluginId: "tokenmunchers-codex@tokenmunchers"`). Native execution confirms that `$tokenmunchers-codex:munch status` loads the full cached SKILL instructions, whereas an unnamespaced `$munch` does not resolve.
  The accompanying `plugins/codex/skills/munch/SKILL.md` frontmatter stays standard:
  ```markdown
  ---
  name: munch
  description: Show tokenmunchers accounting status and preview
  user-invocable: true
  ---
  ```

---

## 7. Privacy, Concurrency, and Trust Models

1. **Metadata-Only Privacy**: Hook stdin delivers raw session turns, prompts, and tool outputs. Hook trigger scripts (`hook` subcommand) **must treat stdin as void**, immediately draining or closing it without reading, parsing, or storing prompt or transcript text.
2. **Concurrency Locking**: Background `async: true` hooks can fire in rapid bursts. Hook triggers acquire a SQLite transaction lock on `${harness}.lock.sqlite` in the state directory across collector work, preventing races without fragile PID files or stale age heuristics.
3. **Codex Trust Flag**: Codex validates hook SHA-256 hashes against `config.toml` (`hooks.state.<id>.trusted_hash`). Non-interactive CLI runs require:
   `codex --dangerously-bypass-hook-trust ...`
4. **Environment Isolation & Credential Handling**: In sandboxed tool shells invoked during skill runs, environment policies may filter sensitive keys (such as `TOKENMUNCHERS_API_KEY`). The `$tokenmunchers-codex:munch` and `/tokenmunchers:munch` skills expose the runtime commands (`status`, `preview`, `activate`, `sync`, `auto-on`, `auto-off`, `pause`, `resume`) under user confirmation. When tool sandboxes filter API credentials, manual uploads requiring `TOKENMUNCHERS_API_KEY` use direct terminal invocation with explicit user consent (`node integrations/plugin/src/cli.ts sync --source <harness> --confirm-upload`), rather than attempting to bypass shell environment filters or inject raw API keys into the assistant's prompt context.
---

## 8. Executable Verification Commands

### Step 1: Stage Clean Plugin Root
```bash
PLUGIN_DIR="$(mktemp -d /tmp/tokenmunchers-plugin-XXXXXX)"
node integrations/package-plugin.ts --output "$PLUGIN_DIR"
```
### Step 2: Claude Code 2.1.312
```bash
claude plugin validate "$PLUGIN_DIR"
claude plugin marketplace add "$PLUGIN_DIR"
claude plugin install tokenmunchers@tokenmunchers -s local
claude plugin details tokenmunchers@tokenmunchers
claude plugin uninstall tokenmunchers -s local
claude plugin marketplace remove tokenmunchers
```
### Step 3: Codex CLI 0.154.0
```bash
codex plugin marketplace add "$PLUGIN_DIR" --json
codex plugin list
codex plugin add tokenmunchers-codex@tokenmunchers --json
codex --dangerously-bypass-hook-trust exec "echo test"
codex plugin remove tokenmunchers-codex@tokenmunchers
codex plugin marketplace remove tokenmunchers
```

### Step 4: Cleanup Staging Directory
```bash
rm -rf "$PLUGIN_DIR"
```
