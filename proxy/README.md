# munch — Local Inference Proxy for TokenMunchers

`munch` is the local proxy program of [tokenmunchers](../README.md). It sends usage events to the same dashboard as the plugins, so its calls appear in Activity and Live now.

`munch` is a lightweight Go background proxy that captures token usage across common AI inference providers and streams metadata-only usage events directly to TokenMunchers via the existing `POST /functions/v1/ingest` API flow.

For first-party desktop subscription usage without routing inference through Munch, see the opt-in [desktop accounting pilot](#desktop-accounting-pilot). It uses a separate reporting path from the proxy described below.

- **No TLS interception or root CA:** binds to loopback and requires a private local URL for inference requests.
- **Streaming passthrough:** SSE and NDJSON flush chunk by chunk. If the upstream body fails mid-stream, the client sees an interrupted response rather than a clean EOF. Usage extraction is bounded and may skip accounting on oversized input.
- **HTTP/SSE only:** WebSocket upgrades are rejected (HTTP 426), so transports or client features that need WebSockets do not work through the proxy.
- **Uses the existing TokenMunchers API flow:** authenticates with your normal `tm_` API key, posts strictly allowlisted `usage` events to `/functions/v1/ingest`, appears live on the dashboard and **Live now** presence automatically, and retries offline via an on-disk JSONL queue.
- **Metadata-only reporting:** inference traffic goes to the configured provider; TokenMunchers receives usage metrics and identifiers, not prompts, completions, tool output, or provider credentials.
- **Native binaries:** standalone Go executable (`CGO_ENABLED=0`), `systemd --user` on Linux, a `launchd` LaunchAgent on macOS, and a detached per-login background process on Windows.

---

## Quick Start

### Windows PowerShell

Use PowerShell 5.1 or newer on Windows 10 or 11 (`amd64` or `arm64`). No Go or administrator account is needed. Create a `tm_` key on the [dashboard](https://tokenmunchers.taiisshort.com/settings) under **Settings → API keys**. Download the installer to a file and review it before running it:

```powershell
Invoke-WebRequest -UseBasicParsing `
  -Uri 'https://raw.githubusercontent.com/hndrxc/tokenmunchers/main/install-munch.ps1' `
  -OutFile '.\install-munch.ps1'
```

Run `.\install-munch.ps1` after review. If `RemoteSigned` blocks the download, you can unblock just the reviewed file with `Unblock-File -LiteralPath '.\install-munch.ps1'`. If policy still blocks it, use the [manual ZIP install](#manual-windows-install) instead of changing policy.

The installer checks `SHA256SUMS`, installs `munch.exe` to `%LOCALAPPDATA%\Programs\Munch`, adds that directory to your user `PATH`, and runs `munch install`. Paste the key when Munch asks; your input is visible. Use `.\install-munch.ps1 -DownloadOnly` to install the executable and update `PATH` without running CLI setup, then run `munch install` when ready. If `munch` isn't found, open a new terminal or run `& "$env:LOCALAPPDATA\Programs\Munch\munch.exe"` with the desired arguments.

Munch starts a detached process in your login session. It does not set up an automatic startup task or service, edit PowerShell profiles, or change execution policy. Run `munch start` after signing in again if it's not running. To check it and route a single command without changing your terminal's environment:

```powershell
munch status
munch exec -- claude
```

With Claude Code installed, make a real request and check the dashboard's **Activity** feed. Alternatively, run `munch env --shell powershell | Out-String | Invoke-Expression` in the PowerShell terminal where you'll launch your tools; it applies only there and to processes started from it. Restart tools that were already open.

- **Codex CLI with ChatGPT sign-in:** accept the optional setup in `munch install` and restart Codex. Environment variables alone don't route this mode. See [manual Codex setup](#4-codex-chatgpt-sign-in) if needed.
- **OMP:** accept the optional plugin setup to report through the plugin, or decline it and disable `omp-tokenmunchers` if you prefer the proxy. See [OMP routing](#oh-my-pi-omp-tm) to avoid double counting.

To upgrade, stop Munch and [rerun the installer](#update-an-installed-release). For config, queue, and log paths see [Locations](#locations); to remove it without losing your key see [Uninstall on Windows](#uninstall-on-windows).

### Linux and macOS

You need Linux or macOS on `amd64` or `arm64`, and a `tm_` API key from the dashboard under **Settings → API keys**.

```bash
# 1. Install the latest release as ~/.local/bin/munch (checked against SHA256SUMS)
curl -fsSL https://raw.githubusercontent.com/hndrxc/tokenmunchers/main/install-munch.sh -o install-munch.sh &&
  sh install-munch.sh

# 2. Only if `munch` is not found: put ~/.local/bin on PATH for this shell
export PATH="$HOME/.local/bin:$PATH"

# 3. Save your key, start the background service, and route your tools through it
munch install
```

If Munch needs a key, paste it at the prompt; your input is visible. Munch starts the service and offers to configure your shell, OMP, and Codex CLI as applicable. See [what Munch changes](#what-munch-changes) for exactly what it writes and what uninstall leaves behind.

Open a new terminal and restart any tools that were already running. Run `munch status`, then make a request in a routed tool and look for it in the dashboard’s **Activity** feed. For later upgrades, see [Update an installed release](#update-an-installed-release).

| Flag | Effect |
| --- | --- |
| `--yes` | Don't prompt: answer yes to every question, including omp-tm. With no saved or exported API key it fails instead of asking for one. |
| `--no-shell` | Don't touch shell profiles. The summary prints the line to add yourself. |
| `--no-codex` | Don't inspect or modify Codex settings. |
| `--config-dir`, `--state-dir`, `--listen` | Use non-default locations. The service is set up with them, but the profile block runs `munch env --check` without flags, so `install` leaves your profile alone and prints the `eval` line, with the same flags, for you to add. |

### Manual setup

`install` only automates steps you can run yourself.

#### 1. Save your TokenMunchers API key

```bash
munch login
```

Paste the key at the prompt rather than putting it in the command line, where it can appear in shell history and process listings. The prompt does not hide terminal input. `munch login` checks only the key's format; it does not contact TokenMunchers. You can also supply `TOKENMUNCHERS_KEY` for a foreground `munch run` instead of saving it, but the background service only sees the saved key.

#### 2. Run as a background service

```bash
# Start the installed service, or start a daemon for this session
munch start

# Or install the auto-start service without editing shell or Codex settings
munch install --no-shell --no-codex

# Verify it's up
munch status
```

#### 3. Point your tools at `munch`

Add this line to `~/.bashrc` or `~/.zshrc`. `munch env` prints nothing unless the local daemon proves it is yours, so a stopped daemon leaves any variables already set untouched:

```bash
eval "$(munch env --check)"
```

Fish shell (`~/.config/fish/config.fish`):

```fish
munch env --check --shell fish | source
```

PowerShell (run in the current terminal; adding this to `$PROFILE` is optional and is not done by Munch):

```powershell
munch env --shell powershell | Out-String | Invoke-Expression
```

Or run a single command through the proxy without changing your shell:

```bash
munch exec -- claude
munch exec -- codex
```

For SDKs configured by hand, run `munch routes --show-local-urls` privately, copy the route URL and append any required API suffix such as `/v1`. Treat the URL as a password: don't put it in shared config, tickets, screenshots, or logs.

#### 4. Codex (ChatGPT sign-in)

`munch install` offers to set this up for the CLI. Use these manual steps if it cannot configure your setup.

Codex signed in with ChatGPT does not read the proxy environment variables, and Codex 0.171+ rejects a plain-HTTP `chatgpt_base_url` ("workspace backend must use an HTTPS origin without credentials"). Instead, add a custom model provider in the user-level `config.toml` (`$CODEX_HOME/config.toml`, default `~/.codex/config.toml`; Codex ignores project-scoped `.codex/config.toml` for this). If you set `chatgpt_base_url` to a plain-HTTP `munch` URL earlier, remove that line: leave `chatgpt_base_url` unset so Codex keeps using its default backend for account discovery.

```bash
munch routes --show-local-urls   # print the private route URLs; treat them as secrets
```

Copy the `chatgpt` route URL (it looks like `http://127.0.0.1:8787/_tm/<secret>/chatgpt`) and add, at the top level of the file (before any `[table]`), the provider selection, then the provider table with `/backend-api/codex` appended:

```toml
model_provider = "munch_chatgpt"

[model_providers.munch_chatgpt]
name = "Munch"
base_url = "http://127.0.0.1:8787/_tm/<secret>/chatgpt/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
```

Then restart Codex (start a new `codex` process) and make a request. `requires_openai_auth = true` reuses your existing ChatGPT sign-in. The proxy forwards everything after the route name verbatim, so `…/chatgpt/backend-api/codex/…` reaches `https://chatgpt.com/backend-api/codex/…`. This was tested with Codex 0.171.0. A later `munch install` recognizes exactly this provider: it only updates its `base_url` if the private URL changed, and `munch uninstall` never removes it.

- **Keep the URL private:** it contains the capability secret. Don't put it in shared config, tickets, screenshots, or logs.
- **What is and isn't routed:** this sends Codex's model responses through `munch` with your existing ChatGPT sign-in; other first-party ChatGPT functionality is not claimed to be routed, and the default backend is still used for account discovery. Sign-in itself still happens directly with OpenAI, and the proxy serves HTTP/SSE only — it rejects WebSocket upgrades (HTTP 426). Only calls that actually go through the proxy are recorded.
- **Using an API key instead?** Codex then talks to the `openai` route: set `openai_base_url` in `~/.codex/config.toml` to the same URL `munch env` exports as `OPENAI_BASE_URL` for other OpenAI-compatible clients.

---

## Desktop accounting pilot

The pilot is off by default. It observes eligible local desktop usage without changing provider sign-in, subscription billing, or model routing. It does not cover cloud sessions, Cowork, or ordinary Claude or ChatGPT chat, and this pilot does not claim macOS runtime support.

### Build, opt in, and check status

Install a [release](#install-a-release), or build manually. For a native Windows build, run this in `proxy/` with Go 1.24+:

```powershell
go build -o munch.exe ./cmd/munch
```

The commands below use `munch` on `PATH`; in PowerShell, use `.\munch.exe` if running the binary from the current directory. On any platform, use the existing `munch login` to save your TokenMunchers key. Opt in to either collector independently:

```text
munch desktop claude on
munch desktop codex on
```

If you already run a Munch service or background daemon, use `munch restart` to load the new settings. Otherwise, run `munch run` in a terminal and leave it running. To reload settings in foreground mode, stop it and run `munch run` again. Use `munch desktop` for collector status or `munch status` for daemon status, including how many desktop requests TokenMunchers has accepted since you turned each enabled source on and how many await delivery. If you use custom config or state directories, use the same directories for these commands and the daemon.

### Claude Desktop: local Code only

1. Run `munch desktop claude-env` privately. It prints a JSON map of environment variable names to string values, including a private local telemetry endpoint. Treat that endpoint capability like a password; don't share it in tickets, screenshots, logs, or config checked into version control.
2. Merge that map into the `env` object in your Claude user `settings.json` (`CLAUDE_CONFIG_DIR` if set, otherwise `~/.claude`), or add the same name/value pairs to the saved environment for Desktop Local. Keep unrelated settings and environment entries. Do not overlay existing telemetry configuration, header helpers, or a managed exporter. If they're already configured, leave them alone rather than replacing them for the pilot.
3. Restart Claude Desktop and start a **new local Code session**. Munch accepts eligible first-party Claude Desktop request telemetry, not ordinary chat or remote Code sessions.

### ChatGPT Desktop: Codex on “This computer” only

Select **This computer** in Codex in ChatGPT Desktop and keep its native `openai` model provider. Munch follows confirmed, uncompressed local Desktop rollouts and rescans retained active and archived rollouts across daemon downtime, counting only eligible usage from opt-in onward. It does not read legacy or compressed rollouts. If you use a custom `CODEX_HOME`, set it to the absolute path of the Desktop Codex home in the daemon's environment; setting it only in an interactive terminal does not change an installed service's environment. The default is `~/.codex`. There is no proxy URL to set and no CLI provider configuration to change. Do not also route or wrap the same desktop app usage for proxy accounting; the two reporting paths are separate.

### What is counted and sent

- One call per exact provider ID: a Claude request ID or a Codex response ID. Usage is live-only from opt-in activation; Munch does not backfill earlier history or infer calls from Codex `token_count` snapshots. If a safe model name is unavailable, it is reported as `unknown`.
- Counts use the actual reported tokens. Claude reports ordinary input separately from cache reads and writes. Codex input includes cached input; Munch subtracts cached reads and writes from ordinary input so the cache tokens aren't counted twice. Reasoning tokens are not added on top of output. Cost is unknown, not a price estimate or subscription charge.
- Munch sends only accounting metadata, including the actual provider request or response ID, through the TokenMunchers `/contributions` endpoint using your TokenMunchers key. It does not upload raw telemetry or rollout records, prompts, replies, tool content, session IDs, or provider credentials. Once committed, eligible calls appear in the dashboard’s **Activity** as **Claude Code** or **Codex** on the next poll (roughly every 30 seconds). Activity reads the existing contribution ledger; it does not create proxy usage events, a second ledger, or **Live now** presence.
- Desktop collection state and pending contributions are stored separately under `<state>/desktop/`. Pending contributions are durable across daemon restarts and retried if delivery fails. `munch pause` stops new accounting without affecting the apps; contributions already queued can still be delivered. `munch resume` resumes accounting. Only one daemon can own a desktop state directory at a time; a second daemon reports the local lock conflict. Separately, if TokenMunchers reports that an ID in a batch belongs to another account, Munch reports the delivery error and retains the original batch rather than taking ownership or dropping it.

### Change the key or destination

The desktop state directory is bound to the TokenMunchers key and contributions endpoint it first used. Changing either stops desktop reporting rather than sending pending data under a different identity. To deliver that data, restore the original key and endpoint. To start with a new key or destination, stop Munch, deliberately move `<state>/desktop/` aside, then turn each source you intend to keep using **off and back on** before restarting Munch. This resets each source's activation time so it doesn't replay usage from the previous identity; running `on` alone for an already enabled source does not reset it. Pending data does not transfer to the new directory. Don't delete the old directory unless you intend to discard its pending data.

### Disable or uninstall

Use `munch desktop claude off` and/or `munch desktop codex off`, then restart Munch (or stop and rerun the foreground process). When disabling Claude or uninstalling Munch, remove **only** the telemetry variables you added for this pilot from user settings or the Desktop Local saved environment. Restart Claude Desktop and use a new local Code session so it no longer has the old environment. There are no Codex provider settings from the pilot to undo. Disabling or uninstalling is not deletion of the desktop state directory or previously uploaded accounting.

---

## What Munch changes

Everything the installer script and `munch` write, start, or edit, and what stays behind. Nothing needs administrator privileges or installs a certificate, and Munch never reads or stores provider credentials. Only the Windows installer updates your user `PATH`.

### Locations

Flags win over environment variables, which win over `config.json` and the defaults.

| What | Linux default | macOS default | Overrides |
| --- | --- | --- | --- |
| Binary | `~/.local/bin/munch` | same | `MUNCH_INSTALL_DIR` (installer script) |
| Config directory | `$XDG_CONFIG_HOME/tmproxy`, else `~/.config/tmproxy` | `~/Library/Application Support/tmproxy` | `TMPROXY_CONFIG_DIR`, `--config-dir` |
| State directory | `$XDG_STATE_HOME/tmproxy`, else `~/.local/state/tmproxy` | `~/Library/Caches/tmproxy` | `TMPROXY_STATE_DIR`, `--state-dir` |
| Service | `tmproxy.service` in `$XDG_CONFIG_HOME/systemd/user/` (else `~/.config/systemd/user/`) | launchd agent `com.tokenmunchers.tmproxy` in `~/Library/LaunchAgents/com.tokenmunchers.tmproxy.plist` | none |
| Listen address | `127.0.0.1:8787` | same | `listen_addr` (`munch login --listen`), `TMPROXY_LISTEN`, `--listen`; numeric loopback only |
| Ingest URL | hosted TokenMunchers `ingest` | same | `ingest_url` (`munch login --url`), `TOKENMUNCHERS_URL`; HTTPS or numeric-loopback HTTP |

On Windows, the default install directory is `%LOCALAPPDATA%\Programs\Munch`, the config directory is `%APPDATA%\tmproxy`, and the state directory is `%LOCALAPPDATA%\tmproxy` (`tmproxy.log` and `tmproxy.pid` are there). The config and state overrides above apply on Windows too. There is no installed Windows service. Config and state defaults on other systems use Go's user config and cache directories plus `tmproxy`. The `tmproxy` names are kept from the program's former name.

| File | Contents and writers |
| --- | --- |
| `<config>/config.json` (`0600`, directory `0700`) | `api_key`, `ingest_url`, `listen_addr` (`munch login`); `api_key` when none is saved and `omp_via_plugin` (`munch install`); `paused` (`munch pause`/`resume`); desktop collector opt-ins (`munch desktop`); `routes` (you) |
| `<config>/local-auth.key` (`0600`) | Random 256-bit local secret, created on the daemon's first start or by `install`. The private `/_tm/<secret>` URL prefix, the control token, and the health proof derive from it; deleting it changes every private URL, so re-run `munch install` and open new shells. |
| `<config>/prices.json` | Only if you create it ([pricing](#custom-model-pricing-pricesjson)) |
| `<state>/queue.jsonl` (`0600`) | Events awaiting delivery; renamed to `queue.jsonl.<pid>.<n>.flushing` while sending, deleted once delivered |
| `<state>/usage.jsonl` (`0600`) | One metadata line per recorded call, for `munch stats`; never trimmed or deleted by Munch |
| `<state>/tmproxy.log`, `<state>/tmproxy.pid` | Daemon output under launchd and the fallback (systemd uses `journalctl --user -u tmproxy`); PID file for the fallback only, removed on stop |
| `<state>/desktop/` | State and durable pending contributions for the [desktop accounting pilot](#desktop-accounting-pilot), separate from the proxy queue |

### Installer script and `munch login`

`install-munch.sh` writes only `munch` (mode `755`) to `${MUNCH_INSTALL_DIR:-$HOME/.local/bin}`. It takes the newest published stable `munch-vX.Y.Z` release, verifies the archive against `SHA256SUMS` in a temporary directory under `$TMPDIR` (default `/tmp`), and moves the binary into place with one rename; temporary files are removed on exit. It does not touch `PATH`, profiles, config, or services, and does not run `munch`.

`install-munch.ps1` installs the matching native Windows executable from the newest stable release after checking the ZIP against `SHA256SUMS`, adds its directory to the user `PATH`, and runs `munch install` unless `-DownloadOnly` was specified.

`munch login` saves `api_key` after a format check (no network call), plus `ingest_url` with `--url` and `listen_addr` with `--listen`. A running daemon keeps its old settings until `munch restart`.

### `munch install`

Steps run in this order; if the key or service step fails, `install` stops before the OMP, shell, and Codex steps.

1. **API key.** Uses the key in `config.json`; otherwise saves `TOKENMUNCHERS_KEY` (then `TOKENMUNCHERS_API_KEY`) there, because the service does not see your shell environment; otherwise asks. With `--yes` or an empty answer and no key, it fails without changes.
2. **Service.** Creates `local-auth.key` if missing and stops a running daemon so the new binary, config, and key take effect. On Linux with a `systemd --user` session it writes the unit and runs `systemctl --user daemon-reload` and `enable --now tmproxy.service`; on macOS it writes the plist and runs `launchctl unload` then `launchctl load -w`. On Linux without a user session it still writes the unit (nothing runs it) and starts the fallback; other systems get the fallback only. The unit and plist run `<absolute munch path> run --config-dir … --state-dir … --listen …`, so re-run `install` after moving the binary or changing those. It then waits up to 10 seconds for the daemon to prove it holds `local-auth.key`, or fails pointing at `munch status` and the service log.
3. **Oh My Pi** (only if `omp` is on `PATH`). **Yes:** runs `omp plugin install omp-tokenmunchers` if `omp plugin list --json` doesn't show it, writes your key as `apiKey` (and a non-default `ingestUrl`) to `<OMP agent dir>/tokenmunchers/config.json` unless it already has a key, and sets `omp_via_plugin: true`. The agent dir is `~/.omp/agent`, adjusted by `PI_CONFIG_DIR`, `OMP_PROFILE`/`PI_PROFILE` (`profiles/<name>/agent`), or `PI_CODING_AGENT_DIR`. **No:** sets `omp_via_plugin` to false and warns if the plugin is installed. A failing `omp` or plugin config is only a warning; routing stays as it was.
4. **Shell profile** (bash, zsh, or fish from `$SHELL`). In `~/.bashrc` (`~/.bash_profile` on macOS), `$ZDOTDIR/.zshrc` (else `~/.zshrc`), or `~/.config/fish/conf.d/munch.fish` (under `$XDG_CONFIG_HOME` if set), it adds or replaces only a block between `# >>> munch >>>` and `# <<< munch <<<` that runs `<absolute munch path> env --check` if that binary exists. The rest of the file stays byte-for-byte; symlinked profiles are edited at their target. Your own `munch env` line counts as already set up. Other shells, `--no-shell`, and the location flags change nothing and get the line printed.
5. **Codex** (only if `codex` is on `PATH`; never with `--no-codex`). It reads `codex login status` only for whether you are signed in with ChatGPT; that output is never printed or stored, and Codex's credential files are never opened. It skips Codex if not signed in with ChatGPT, if your `chatgpt` route has a custom upstream, or if `$CODEX_HOME` (default `~/.codex`) is not a directory. After you confirm, it rewrites `$CODEX_HOME/config.toml` atomically with mode `0600`, creating it if needed: a marked `model_provider = "munch_chatgpt"` block at the top, the marked [provider table](#4-codex-chatgpt-sign-in) at the end, and an old plain-HTTP Munch `chatgpt_base_url` removed. It leaves the file alone and says why if another provider is selected (also by the active profile), `chatgpt_base_url` points at a custom backend, a `munch_chatgpt` provider differs from the documented one, Munch's blocks were edited, or the file is read-only, invalid TOML, or can't take the blocks without changing other settings. Running Codex processes keep their old config.

### Daemon, delivery, and pause

The service runs `munch run`, which listens only on loopback. For each proxied call with tokens it appends a line to `usage.jsonl` and `queue.jsonl` and delivers the queue to the ingest URL in batches of up to 100. Network errors, HTTP `401`, and server errors keep events queued, retried every 30 seconds with exponential backoff (`munch flush` retries now). Events rejected with `400`/`413`, or older than 6.5 days, are dropped. Events keep their IDs, so resends are de-duplicated. On shutdown it tries one last delivery for up to 5 seconds.

`munch pause` saves `"paused": true`, through the daemon or directly, so it survives restarts: traffic keeps flowing, nothing new is recorded or queued, and queued events are still sent. `munch resume` undoes it.

The daemon reads `config.json`, `prices.json`, and both keys only at startup. A foreground `munch run` and the other commands also honor `TOKENMUNCHERS_KEY`/`TOKENMUNCHERS_API_KEY`, `TOKENMUNCHERS_URL`, and `TMPROXY_LISTEN`; the service does not see them.

### Restarts and the fallback

| Backend | Starts after reboot | Restarted if it exits | `munch stop` | Output |
| --- | --- | --- | --- | --- |
| `systemd-user` | Yes, with your user session | On failure, after 3 seconds | `systemctl --user stop`; stays enabled for the next session | `journalctl --user -u tmproxy` |
| `launchd` | Yes, at login | Always (`KeepAlive`) | `launchctl unload`; the plist loads again at next login | `tmproxy.log` |
| `pidfile` fallback | No | No | Authenticated shutdown request | `tmproxy.log` |

`munch start` uses the installed unit or plist when available. Otherwise it starts the fallback, writes `tmproxy.pid`, and waits up to 3 seconds. `munch restart` stops and starts the daemon to load a new binary or settings. The PID-file fallback uses authenticated shutdown rather than signaling a PID from disk.

### `munch update`

Release builds only (source builds report `dev`), on Linux or macOS, `amd64` or `arm64`. It takes the highest stable `munch-vX.Y.Z` from fixed GitHub HTTPS URLs (nothing configures the source) and never downgrades or reinstalls. It stages next to the binary the running `munch` resolves to (following symlinks; that directory must be writable), checks the archive against `SHA256SUMS` and the binary header against your platform, and swaps it in with one rename that keeps the permission bits. Config, keys, service, profiles, Codex, and OMP are untouched, and the daemon runs the old version until `munch restart`. Native Windows does not support `munch update`.

### `munch env`, `munch exec`, and read-only commands

`munch env` prints shell code. By default (`--check`) it emits nothing unless the daemon proves it holds `local-auth.key`; `--check=false` can create the key if missing. `munch exec` passes the same variables to one child after the health check. See [Built-in Routes](#built-in-routes) for the exports and OMP wrapper. `status`, `stats`, and plain `routes` are read-only. `routes --show-local-urls` can create the key and prints the private URLs. `flush` attempts delivery of queued events.

### `munch uninstall`

| Removed | Reported, but kept | Kept |
| --- | --- | --- |
| The service: `systemctl --user disable --now` and unit deleted, or `launchctl unload -w` and plist deleted; then any fallback daemon that proves the key is stopped and `tmproxy.pid` deleted | `munch env` lines you added yourself | The `munch` binary |
| The marked block in the bash, zsh, and fish profiles above, whatever your current shell (an emptied `munch.fish` is deleted) | Munch settings you wrote or edited in Codex's `config.toml`, including the manual provider; revert them, because Codex can't connect once the proxy is gone | The config directory: API key, routes, `local-auth.key`, `prices.json` |
| Unedited Munch blocks in `$CODEX_HOME/config.toml` and `~/.codex/config.toml`; the provider table stays while anything else still selects `munch_chatgpt` | The `omp-tokenmunchers` plugin and its config, which keep reporting OMP | The state directory: proxy queued events (sent if you reinstall within 6.5 days), `usage.jsonl`, `tmproxy.log`, and `<state>/desktop/` (including pending contributions); and a Codex `config.toml` left empty |

Open shells keep their variables until you open a new terminal or run `eval "$(munch env --unset)"`, and running Codex processes use the proxy until restarted. To remove everything (Linux paths; on macOS use `~/Library/Application Support/tmproxy` and `~/Library/Caches/tmproxy`):

```bash
munch flush                                       # send queued events while the daemon still runs
munch uninstall
omp plugin uninstall omp-tokenmunchers            # only if you no longer want omp-tm
rm -rf ~/.config/tmproxy ~/.local/state/tmproxy   # API key, local key, queue, usage log
rm ~/.local/bin/munch
```

#### Uninstall on Windows

Run `munch uninstall` before deleting the executable. This stops the daemon and removes the unedited Codex settings Munch added. Revert any manually configured Munch URLs and restart your tools. Close terminals where you applied routing, or clear it before removing the executable:

```powershell
munch env --unset --shell powershell | Out-String | Invoke-Expression
```

Delete `%LOCALAPPDATA%\Programs\Munch\munch.exe` and remove just `%LOCALAPPDATA%\Programs\Munch` from your **user** `Path` in **Edit environment variables for your account**. If you installed manually elsewhere, use that location instead. Leave `%APPDATA%\tmproxy` and `%LOCALAPPDATA%\tmproxy` in place to keep your key, settings, pending events, and logs. Delete those directories only if you intend to discard their contents. The OMP plugin and its own key are separate; uninstall the plugin only if you want OMP to stop reporting too.

### No double counting and secrets

- **OMP:** with omp-tm, the `omp` wrapper and `munch exec -- omp` hide every proxy variable so only the plugin reports it; without omp-tm, disable the plugin. See [Oh My Pi (omp-tm)](#oh-my-pi-omp-tm).
- **Codex proxy accounting:** ChatGPT sign-in ignores the proxy environment variables, so only the selected Munch provider routes it. [Uninstall the retired plugins](#retired-claude-code-and-codex-plugins), which would report the same calls. Do not use proxy accounting and the desktop pilot for the same app usage.
- **API key:** stored in `config.json`, copied to omp-tm only if you choose it, sent to TokenMunchers for proxy ingest or desktop contributions, and never printed (`login` and `status` say only whether one is configured).
- **Private proxy URLs** from `local-auth.key` appear in `munch env` output, `munch routes --show-local-urls`, shell and `exec` environments, and Codex's `config.toml`. The Claude desktop telemetry endpoint is a separate private capability. Provider credentials pass through to the upstream only. See [Privacy & Security](#privacy--security).

---

## Build & Install

### Install a release

The [Linux and macOS installer](#linux-and-macos) runs on `amd64` and `arm64`. It needs `curl`, `python3`, `tar`, `mktemp`, `awk`, and either `sha256sum` or `shasum`, but not Go or `sudo`. To install somewhere else, run `MUNCH_INSTALL_DIR=/path/to/bin sh install-munch.sh`, then put that directory on your `PATH` and run `munch --version` and `munch install`. For the native Windows installer, see [Windows PowerShell](#windows-powershell).

For a manual Unix install, download the archive for your OS (`linux` or `darwin`) and architecture (`amd64` or `arm64`) and `SHA256SUMS` from [GitHub Releases](https://github.com/hndrxc/tokenmunchers/releases). Check the archive against the matching entry before extracting `munch` into a directory on your `PATH`.

#### Manual Windows install

From the **same stable** `munch-vX.Y.Z` [release](https://github.com/hndrxc/tokenmunchers/releases), download `SHA256SUMS` and `munch-X.Y.Z-windows-amd64.zip` (or `munch-X.Y.Z-windows-arm64.zip` on ARM64). In the download directory, inspect `SHA256SUMS` and compare the ZIP's hash to the entry for its **exact filename**:

```powershell
$zip = 'munch-X.Y.Z-windows-amd64.zip'  # substitute the actual version and architecture
Get-Content -LiteralPath '.\SHA256SUMS' | Select-String -SimpleMatch $zip
Get-FileHash -LiteralPath $zip -Algorithm SHA256
```

Don't extract or run anything if the entry is missing or the hashes differ (letter case doesn't matter). If they match, extract the ZIP, which contains only `munch.exe`, to your user install directory:

```powershell
$dir = Join-Path $env:LOCALAPPDATA 'Programs\Munch'
Expand-Archive -LiteralPath $zip -DestinationPath $dir
& (Join-Path $dir 'munch.exe') install
```

Add the path shown by `$dir` to your user `Path` if you want to run `munch` by name in new terminals. For a manual upgrade, stop the running daemon before replacing the old `munch.exe`, then run `munch install` again.

### Update an installed release

On Windows, run `munch stop` to release the running executable before replacing it. Download, review, and rerun the [PowerShell installer](#windows-powershell); it reuses your saved key and starts the daemon again. If you use `-DownloadOnly`, run `munch start` yourself afterward. The commands below are for Linux and macOS.

```bash
munch update    # verify and install the newest stable release in place
munch restart   # switch a running daemon to it
```

What `update` checks and leaves alone is described in [`munch update`](#munch-update). If your old release has no `update` command, re-run the installer script.

### Build from source

Requires Go 1.24+.

```bash
cd proxy
mkdir -p ~/.local/bin
go build -o ~/.local/bin/munch ./cmd/munch
```

Cross-compile examples:

```bash
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -o munch-linux-amd64 ./cmd/munch
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o munch-darwin-arm64 ./cmd/munch
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o munch.exe ./cmd/munch
```

Untagged source builds report version `dev` and cannot use `munch update`. To set a version in a manual build from `proxy/`:

```bash
go build -ldflags '-X github.com/tokenmunchers/tokenmunchers/proxy/internal/config.Version=0.1.3' ./cmd/munch
```

### Cut a release

From the repository root on `main`, run the release helper with the next version:

```bash
bash .github/scripts/release-munch 0.1.3
```

The helper pushes the release commit to `origin/main` first, then creates and pushes the annotated `munch-v0.1.3` tag in a separate push. Don't send `main` and the tag in one push: GitHub recorded only the branch run when we did that for `v0.1.0`.

The tag push starts the `munch` workflow. It tests `proxy/` and publishes four Unix `.tar.gz` archives (Linux and macOS, each on `amd64` and `arm64`), two Windows `.zip` archives (`amd64` and `arm64`, each containing only `munch.exe`), and one `SHA256SUMS` covering all six archives. If the tag is on GitHub but no tag run appears in Actions, start the same workflow with `gh workflow run munch.yml --ref munch-v0.1.3`. If only the tag push failed, retry it with `git push --no-follow-tags origin refs/tags/munch-v0.1.3`.

`munch --version` and the usage client's version both use the numeric version from the tag.

### Upgrading from tmproxy

The command used to be called `tmproxy`. You do not need to run `tmproxy uninstall`: `munch install` rewrites the existing `tmproxy.service` unit (or the launchd agent) to point at the new binary and reuses the same config directory, so your API key, routes, and local key carry over. Then:

1. Remove any hand-added `eval "$(tmproxy env --check)"` (or fish/PowerShell equivalent) line from your shell rc; `munch install` adds its managed block only on supported Unix shells. On Windows, [route tools from PowerShell](#windows-powershell).
2. Delete the old `tmproxy` binary.

### Retired Claude Code and Codex plugins

The native Claude Code and Codex collector plugins were retired in favor of `munch`. If you previously installed them, uninstall them so they stop uploading: `claude plugin uninstall tokenmunchers@tokenmunchers` and `codex plugin remove tokenmunchers-codex@tokenmunchers`. Usage they uploaded before stays on the leaderboard.

---

## Built-in Routes

| Route | Default Upstream | Wire Format | Env Var(s) |
| --- | --- | --- | --- |
| `anthropic` | `https://api.anthropic.com` | Anthropic Messages (JSON + SSE) | `ANTHROPIC_BASE_URL` |
| `openai` | `https://api.openai.com` | OpenAI Chat + Responses (JSON + SSE) | `OPENAI_BASE_URL` (includes `/v1`) |
| `chatgpt` | `https://chatgpt.com` | ChatGPT / Codex Responses (SSE) | None emitted — `munch install` offers to configure the Codex provider |
| `gemini` | `https://generativelanguage.googleapis.com` | Gemini GenerateContent (JSON + SSE) | `GEMINI_API_BASE_URL`, `GOOGLE_GEMINI_BASE_URL` |
| `xai` | `https://api.x.ai` | OpenAI-compatible + `cost_in_usd_ticks` | `XAI_BASE_URL` (includes `/v1`) |
| `grok` | `https://api.x.ai` | OpenAI-compatible + `cost_in_usd_ticks` | `GROK_MODELS_BASE_URL` |
| `openrouter` | `https://openrouter.ai` | OpenAI-compatible + `usage.cost` | `OPENROUTER_BASE_URL` (includes `/api/v1`) |
| `ollama` | `http://127.0.0.1:11434` | Ollama native NDJSON + OpenAI-compatible | `OLLAMA_HOST` (not given to `omp`; see below) |

> The `chatgpt` route is used only by Codex signed in with ChatGPT. `munch env` never sets it; use `munch install` or [configure it manually](#4-codex-chatgpt-sign-in).

### Oh My Pi (omp-tm)

OMP reads `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `XAI_BASE_URL`, `OPENROUTER_BASE_URL`, and `OLLAMA_HOST` from the environment, and the environment wins over OMP's own config. `munch install` therefore asks which component should report OMP (omp-tm is the default):

- **omp-tm.** The `omp-tokenmunchers` plugin (omp-tm) already reports every OMP call natively, so if the proxy variables also reached OMP, each call would be reported twice with different event IDs. `install` sets up the plugin as described in [`munch install`](#munch-install) and records `"omp_via_plugin": true`. With that flag, `munch env` also defines an `omp` wrapper that runs the real `omp` with every variable `munch env` exports removed: an `omp` function in sh, bash, and zsh, a `function omp` in fish, and a `function omp` in PowerShell that restores the variables afterwards. Other tools keep routing through the proxy. The wrapper exists only in new shells, and `munch env --unset` removes it.
- **The proxy.** If you decline, `omp_via_plugin` stays off and OMP routes through the proxy like any other tool, except for `OLLAMA_HOST` (below). Disable the plugin so calls aren't counted twice: `omp plugin disable omp-tokenmunchers`.

#### Why OMP never gets `OLLAMA_HOST`

`munch env` exports `OLLAMA_HOST` with a private route (`http://127.0.0.1:8787/_tm/<secret>/ollama`), which the `ollama` CLI and Ollama SDKs keep intact. OMP does not: it reduces `OLLAMA_HOST` to `scheme://host:port`, so the private route is lost and its model discovery hits the proxy root and fails. Any `OLLAMA_HOST` override also makes OMP's Ollama discovery mandatory, so OMP then warns at every startup that discovery failed.

So `munch` leaves `OLLAMA_HOST` out for `omp` in every mode. OMP's own Ollama discovery stays optional and quiet, and it talks to your local Ollama directly. Concretely:

- If `omp` is on your `PATH` (or `omp_via_plugin` is set), `munch env` defines the `omp` wrapper. Without `omp_via_plugin` it removes only `OLLAMA_HOST`; with it, every proxy variable (as above).
- `munch exec -- omp` removes the same variables from OMP's environment, both the ones `munch` would add and any already exported by your shell.
- Everything else, such as the `ollama` CLI, keeps `OLLAMA_HOST` and stays proxied.

Because the wrapper removes these variables, configure custom OMP endpoints (for example a remote Ollama host) in OMP's own config, not through them. `munch uninstall` leaves the plugin installed; remove it with `omp plugin uninstall omp-tokenmunchers`.

---

## Commands

Every command except `update` and `--version` accepts `--config-dir` and `--state-dir`; all except `stats` also accept `--listen`.

| Command | Description |
| --- | --- |
| `munch login [key]` | Check the format of your `tm_` API key and save it (`--url` sets a self-hosted ingest URL, `--listen` the proxy address) |
| `munch run` | Run in the foreground (`--verbose` logs captured metadata per call) |
| `munch start` / `stop` / `restart` | Start, stop, or restart the background daemon ([details](#restarts-and-the-fallback)) |
| `munch install [--yes] [--no-shell] [--no-codex]` | Set up the proxy and offer to configure OMP, the shell profile, and Codex (see [Quick Start](#quick-start)) |
| `munch update` | Verify and install a newer stable Munch binary in place; restart a running daemon separately |
| `munch uninstall` | Stop and remove the service and remove managed shell and Codex setup; keeps the binary, config and state directories, and the `omp-tokenmunchers` plugin ([details](#munch-uninstall)) |
| `munch status [--json]` | Show runtime status, uptime, queue depth, delivered counts, last error, and desktop pending status |
| `munch desktop` | Show desktop collector status |
| `munch desktop claude on/off` or `munch desktop codex on/off` | Enable or disable a desktop collector; restart the daemon to apply |
| `munch desktop claude-env` | Print the private Claude Desktop telemetry environment as a JSON string map |
| `munch pause` / `resume` | Stop/resume recording usage (AI traffic keeps proxying uninterrupted); persists across restarts |
| `munch flush` | Force-flush queued usage events immediately |
| `munch env [--check] [--unset] [--shell ...]` | Emit shell exports for common inference providers. When `omp` is on `PATH` or `omp_via_plugin` is set it also emits an `omp` wrapper that runs OMP without `OLLAMA_HOST` (or without every export when `omp_via_plugin` is set); `--unset` removes the wrapper too |
| `munch exec -- <cmd...>` | Verify the local daemon and run a command with private proxy URLs. `omp` gets the same variables its shell wrapper leaves out (and a one-line note when omp-tm is on) |
| `munch stats [--period today\|week\|all]` | Inspect local metadata-only usage summary (UTC) |
| `munch routes [--show-local-urls]` | List upstream targets; print private local URLs only on explicit request |
| `munch --version` | Print the version, OS, and architecture |

---

## Custom Routes & Pricing

Both files live in the [config directory](#locations). Run `munch restart` after editing them.

### Custom routes (`config.json`)

You can add or override routes under `"routes"`:

```json
{
  "listen_addr": "127.0.0.1:8787",
  "routes": {
    "vllm": {
      "upstream": "http://127.0.0.1:8000",
      "provider": "vllm",
      "format": "openai",
      "local": true
    }
  }
}
```

Use `https://` for remote upstreams. Cleartext `http://` is allowed only for numeric loopback addresses. Upstream base URLs cannot contain credentials, a query, or a fragment. Requests still carry their normal query parameters and provider authentication to the configured upstream; only configure destinations you trust with that data. `munch install` does not configure Codex while the `chatgpt` route points anywhere but `https://chatgpt.com`.

### Custom model pricing (`prices.json`)

When an upstream reports cost directly (OpenRouter `usage.cost`, xAI `cost_in_usd_ticks`), `munch` uses the reported cost. Otherwise it estimates `cost_usd` using its built-in per-1M-token pricing table (fallback `0` for unknown models; local routes like `ollama` always report `0`). Override or add rates in `prices.json`:

```json
{
  "my-custom-model": {
    "input": 2.5,
    "output": 10.0,
    "cache_read": 0.25,
    "cache_write": 3.0
  }
}
```

---

## Troubleshooting

- **`install` says munch did not answer within 10 seconds.** Run `munch status`, then read `journalctl --user -u tmproxy` (systemd) or `tmproxy.log` in the state directory. If another program holds port 8787, pick another loopback port with `munch login --listen 127.0.0.1:<port>` and run `munch install` again.
- **Usage doesn't appear on the dashboard.** `munch status` shows whether reporting is paused, the queue depth, and the last delivery error; `munch flush` retries at once. Tools started before `munch install` don't go through the proxy: open a new terminal or restart them.
- **Old plain URLs stopped working.** Unauthenticated URLs such as `http://127.0.0.1:8787/openai/v1` no longer work. Re-run `munch install` (it restarts the daemon), or restart the daemon and re-run `munch env --check`, then start a new shell. Use `munch env --unset` to clear stale exports, and [step 3](#3-point-your-tools-at-munch) for SDKs configured by hand.
- **Codex: "workspace backend must use an HTTPS origin".** An old plain-HTTP `chatgpt_base_url` is still in Codex's `config.toml`. `munch install` offers to remove it, or delete the line yourself.
- **Codex skipped by `install`, or Codex cannot connect.** The `Codex:` line of `install` says why it left the config alone; fix that or use the [manual setup](#4-codex-chatgpt-sign-in). After `munch uninstall`, or after `local-auth.key` changed, revert any Munch settings Codex still has, or run `munch install` again.
- **`munch update` refuses to run.** It needs a release build on Linux or macOS (`amd64`/`arm64`) and write access to the binary's directory; otherwise re-run the installer script.

---

## Privacy & Security

- The listener is loopback-only, rejects browser-originated requests, and sandboxes upstream content. Inference requires a private URL; control requests require a separate local credential. `munch exec` and, by default, `munch env` verify that the daemon knows the installation key before launching a client or emitting URLs.
- Local capabilities protect against users who cannot read your private config or process environment. They do not protect against administrators, processes running as you, or a client that logs or forwards its URL. Don't publish the port through a tunnel, container mapping, or reverse proxy, and keep loopback out of any client-side HTTP proxy. Prefer `munch exec` over persistent exports: verification is done before launch, not for every SDK request, and a client can outlive the daemon.
- Provider authentication and inference payloads pass through to the configured upstream, without the private route prefix or control headers. Upstream redirects are rejected so clients cannot leak private loopback URLs by following them. The proxy processes request and response data in memory to extract usage, but does not include that content in its telemetry.
- The ingest destination must use HTTPS (or numeric-loopback HTTP for local testing) and redirects are not followed. For proxy ingest, TokenMunchers receives only usage event fields: pseudonymous session and event IDs, timestamps, provider and model names, token counts, cost, and client metadata. Session pseudonyms are linkable within a daemon run but change after restart; the proxy does not upload the original session header or User-Agent as usage metadata. Model and provider names are still visible, so don't put secrets in custom names. Desktop contributions use [separate metadata-only reporting](#what-is-counted-and-sent).
- `queue.jsonl` and `usage.jsonl` hold usage metadata only and are created `0600`; `usage.jsonl` is kept for `munch stats` until you delete it. Keep custom config and state directories private. Every file Munch writes is listed in [What Munch changes](#what-munch-changes).
- Upgrading does not rewrite old queue files on disk. When the proxy next sends legacy entries, it replaces their original session IDs with a shared legacy label.
