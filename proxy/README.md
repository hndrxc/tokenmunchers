# munch — Local Inference Proxy for TokenMunchers

`munch` is the local proxy program of [tokenmunchers](../README.md). It sends usage events to the same dashboard as the plugins, so its calls appear in Activity and Live now.

`munch` is a lightweight, zero-dependency Go background proxy that captures token usage across common AI inference providers and streams metadata-only usage events directly to TokenMunchers via the existing `POST /functions/v1/ingest` API flow.

- **No TLS interception or root CA:** binds to loopback and requires a private local URL for inference requests.
- **Streaming passthrough:** SSE and NDJSON flush chunk by chunk. If the upstream body fails mid-stream, the client sees an interrupted response rather than a clean EOF. Usage extraction is bounded and may skip accounting on oversized input.
- **HTTP/SSE only:** WebSocket upgrades are rejected (HTTP 426), so transports or client features that need WebSockets do not work through the proxy.
- **Uses the existing TokenMunchers API flow:** authenticates with your normal `tm_` API key, posts strictly allowlisted `usage` events to `/functions/v1/ingest`, appears live on the dashboard and **Live now** presence automatically, and retries offline via an on-disk JSONL queue.
- **Metadata-only reporting:** inference traffic goes to the configured provider; TokenMunchers receives usage metrics and identifiers, not prompts, completions, tool output, or provider credentials.
- **Linux-first, easily portable:** pure Go standard library (`CGO_ENABLED=0`), XDG paths + `systemd --user` service on Linux, `launchd` LaunchAgent on macOS, and a PID-file daemon fallback elsewhere.

---

## Build & Install

### Install a release

From [GitHub Releases](https://github.com/hndrxc/tokenmunchers/releases), pick a `munch-v*` release and download its archive and `SHA256SUMS` into the same directory. Match your OS (`linux` or `darwin` for macOS) and CPU (`amd64` for x86-64 or `arm64` for ARM64, including Apple silicon). Go is not required.

Set `archive` to the downloaded filename, then verify the checksum before installing:

```bash
archive=munch-0.1.0-darwin-arm64.tar.gz
grep -F "  $archive" SHA256SUMS | shasum -a 256 -c &&
  mkdir -p ~/.local/bin &&
  tar -xzf "$archive" -C ~/.local/bin munch
```

Put `~/.local/bin` on your `PATH`, then run `munch --version` and `munch install`.

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

### Cut a release

From the repository root on `main`, run the release helper with the next version:

```bash
bash .github/scripts/release-munch 0.1.1
```

The helper pushes the release commit to `origin/main` first, then creates and pushes the annotated `munch-v0.1.1` tag in a separate push. Don't send `main` and the tag in one push: GitHub recorded only the branch run when we did that for `v0.1.0`.

The tag push starts the `munch` workflow. It tests `proxy/`, builds Linux and macOS archives for `amd64` and `arm64`, and publishes them with `SHA256SUMS`. If the tag is on GitHub but no tag run appears in Actions, start the same workflow with `gh workflow run munch.yml --ref munch-v0.1.1`. If only the tag push failed, retry it with `git push --no-follow-tags origin refs/tags/munch-v0.1.1`.

`munch --version` and the usage client's version both use the numeric version from the tag. Untagged source builds default to `0.1.0`. To set a version in a manual build from `proxy/`:

```bash
go build -ldflags '-X github.com/tokenmunchers/tokenmunchers/proxy/internal/config.Version=0.1.1' ./cmd/munch
```

### Upgrading from tmproxy

The command used to be called `tmproxy`. You do not need to run `tmproxy uninstall`: `munch install` rewrites the existing `tmproxy.service` unit (or the launchd agent) to point at the new binary and reuses `~/.config/tmproxy`, so your API key, routes, and local key carry over. Then:

1. Remove any hand-added `eval "$(tmproxy env --check)"` (or fish/PowerShell equivalent) line from your shell rc; `munch install` adds its own managed block.
2. Delete the old `tmproxy` binary.

The configuration and state directories, the `tmproxy.service` unit name, and the `TMPROXY_*` environment variables keep their names.

### Retired Claude Code and Codex plugins

The native Claude Code and Codex collector plugins were retired in favor of `munch`. If you previously installed them, uninstall them so they stop uploading: `claude plugin uninstall tokenmunchers@tokenmunchers` and `codex plugin remove tokenmunchers-codex@tokenmunchers`. Usage they uploaded before stays on the leaderboard.

---

## Quick Start

Create a `tm_` API key in the dashboard under **Settings → API keys**, then run:

```bash
munch install
```

`munch install` is a one-shot setup that prints a line per step:

1. **API key.** Uses the key saved in `config.json` by `munch login`. If there is none, it takes `TOKENMUNCHERS_KEY` (or `TOKENMUNCHERS_API_KEY`) from your environment and saves it to `config.json`, because the background service does not see your shell's environment. Failing that, it asks you to paste one; the prompt does not hide terminal input. Keys are stored with mode `0600` in `~/.config/tmproxy/config.json` and are never printed.
2. **Background service.** Installs and starts the OS service (`systemd --user` on Linux, `launchd` on macOS), then waits for an authenticated health check. A daemon that is already running is stopped first, so a new binary, config, and key take effect. Without a service manager it starts a plain background process and tells you it won't restart after a reboot. If the proxy doesn't answer within 10 seconds, `install` fails and points you to `munch status` (and `journalctl --user -u tmproxy` on systemd).
3. **omp-tm.** If `omp` is on your `PATH`, it asks whether to report Oh My Pi usage with the `omp-tokenmunchers` plugin instead of the proxy (the default answer is yes). See [Oh My Pi (omp-tm)](#oh-my-pi-omp-tm). A problem with `omp` or the plugin is only a warning: OMP routing stays as it was, and you can run `munch install` again.
4. **Shell profile.** Asks before changing your profile (`Add munch to <profile> so new shells route AI tools through the proxy?`, or `Update the munch block in <profile>?` if the block exists). On yes, adds a marked block (`# >>> munch >>>` … `# <<< munch <<<`) so new shells run `munch env --check` and route Claude Code, the OpenAI SDKs, Gemini, and other tools through the proxy. Codex signed in with ChatGPT is the exception: it reads its ChatGPT backend URL from its own config, not from these exported variables, so it needs the manual step in [Codex (ChatGPT sign-in)](#4-codex-chatgpt-sign-in). The profile is picked from `$SHELL`: `~/.bashrc` (`~/.bash_profile` on macOS), `.zshrc` under `$ZDOTDIR` or your home directory, or `~/.config/fish/conf.d/munch.fish`. Other shells are not edited; the summary prints the line to add yourself. Everything outside the block stays untouched, and a hand-written `munch env` line already in the profile is left alone.
5. **Summary.** Prints where munch is running, whether it starts again after a reboot, whether new shells are wired, how OMP is handled, and a reminder to open a new terminal.

Open a new terminal afterwards: shells and apps that are already running keep their old environment. Re-running `munch install` is safe; it restarts the daemon and refreshes the profile block. `munch uninstall` removes the service and the profile block.

| Flag | Effect |
| --- | --- |
| `--yes` | Don't prompt: take the recommended answer to every question, including omp-tm. With no API key available it fails instead of asking for one. |
| `--no-shell` | Don't touch shell profiles. The summary prints the line to add yourself. |
| `--config-dir`, `--state-dir`, `--listen` | Use non-default locations. The profile block runs `munch env --check` without flags, so with any of these `install` leaves your profile alone and prints the `eval` line, with the same flags, for you to add. |

### Manual setup

`install` only automates steps you can run yourself.

#### 1. Save your TokenMunchers API key

```bash
munch login
```

Paste the key at the prompt rather than putting it in the command line, where it can appear in shell history and process listings. The current prompt does not hide terminal input. Keys are stored with mode `0600` in `~/.config/tmproxy/config.json`. You can also supply `TOKENMUNCHERS_KEY` via environment variable instead of saving it on disk.

#### 2. Run as a background service

```bash
# Start a background daemon (it does not survive a reboot)
munch start

# Or install the auto-start service without touching your shell profile
munch install --no-shell

# Verify it's up
munch status
```

#### 3. Point your tools at `munch`

Add this line to `~/.bashrc` or `~/.zshrc`. `munch env` verifies the local daemon before emitting private URLs by default. It does not clear variables that are already set:

```bash
eval "$(munch env --check)"
```

Fish shell (`~/.config/fish/config.fish`):

```fish
munch env --check --shell fish | source
```

PowerShell (`$PROFILE`):

```powershell
munch env --shell powershell | Out-String | Invoke-Expression
```

Or run a single command through the proxy without changing your shell:

```bash
munch exec -- claude
munch exec -- codex
```

**Upgrading:** unauthenticated URLs such as `http://127.0.0.1:8787/openai/v1` no longer work. Re-run `munch install` (it restarts the running daemon), or restart the daemon and re-run `munch env --check`, then start a new shell. For SDKs configured manually, run `munch routes --show-local-urls` privately, copy the route URL and append any required API suffix such as `/v1`. Treat the URL as a password: don't put it in shared config, tickets, screenshots, or logs. Use `munch env --unset` to clear stale exports.

#### 4. Codex (ChatGPT sign-in)

Codex signed in with ChatGPT does not read the proxy environment variables: it takes its ChatGPT backend URL from the `chatgpt_base_url` setting in `~/.codex/config.toml` (user-level config only — Codex ignores the key in project-scoped `.codex/config.toml`). Wire it to the `chatgpt` route manually:

```bash
munch routes --show-local-urls   # print the private route URLs; treat them as secrets
```

Copy the `chatgpt` route URL (it looks like `http://127.0.0.1:8787/_tm/<secret>/chatgpt`) and set it in `~/.codex/config.toml`, appending the `/backend-api/` suffix Codex expects:

```toml
chatgpt_base_url = "http://127.0.0.1:8787/_tm/<secret>/chatgpt/backend-api/"
```

Then restart Codex (start a new `codex` process) and make a request. The proxy forwards everything after the route name verbatim, so `…/chatgpt/backend-api/…` reaches `https://chatgpt.com/backend-api/…` exactly as in Codex's default configuration.

- **Keep the URL private:** it contains the capability secret. Don't put it in shared config, tickets, screenshots, or logs.
- **What is and isn't routed:** this covers Codex's model-API traffic only. Sign-in itself still happens directly with OpenAI, and the proxy serves HTTP/SSE only — it rejects WebSocket upgrades (HTTP 426), so any Codex feature that needs a WebSocket transport will not work through it. Only calls that actually go through the proxy are recorded.
- **Using an API key instead?** Codex then talks to the `openai` route: set `openai_base_url` in `~/.codex/config.toml` to the same URL `munch env` exports as `OPENAI_BASE_URL` for other OpenAI-compatible clients.

---

## Built-in Routes

| Route | Default Upstream | Wire Format | Env Var(s) |
| --- | --- | --- | --- |
| `anthropic` | `https://api.anthropic.com` | Anthropic Messages (JSON + SSE) | `ANTHROPIC_BASE_URL` |
| `openai` | `https://api.openai.com` | OpenAI Chat + Responses (JSON + SSE) | `OPENAI_BASE_URL` (includes `/v1`) |
| `chatgpt` | `https://chatgpt.com` | ChatGPT / Codex Responses (SSE) | None emitted — set Codex's `chatgpt_base_url` manually |
| `gemini` | `https://generativelanguage.googleapis.com` | Gemini GenerateContent (JSON + SSE) | `GEMINI_API_BASE_URL`, `GOOGLE_GEMINI_BASE_URL` |
| `xai` | `https://api.x.ai` | OpenAI-compatible + `cost_in_usd_ticks` | `XAI_BASE_URL` (includes `/v1`) |
| `grok` | `https://api.x.ai` | OpenAI-compatible + `cost_in_usd_ticks` | `GROK_MODELS_BASE_URL` |
| `openrouter` | `https://openrouter.ai` | OpenAI-compatible + `usage.cost` | `OPENROUTER_BASE_URL` (includes `/api/v1`) |
| `ollama` | `http://127.0.0.1:11434` | Ollama native NDJSON + OpenAI-compatible | `OLLAMA_HOST` (not given to `omp`; see below) |

> The `chatgpt` route is used only by Codex signed in with ChatGPT, and `munch env` never sets it; configure it manually as described in [Codex (ChatGPT sign-in)](#4-codex-chatgpt-sign-in).
> **Avoiding double counting:** OMP has its own plugin (omp-tm), so `munch install` keeps OMP out of the proxy by default and the same call isn't reported twice. See below.

### Oh My Pi (omp-tm)

OMP reads `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `XAI_BASE_URL`, `OPENROUTER_BASE_URL`, and `OLLAMA_HOST` from the environment, and the environment wins over OMP's own config. `munch install` therefore asks which component should report OMP (omp-tm is the default):

- **omp-tm.** The `omp-tokenmunchers` plugin (omp-tm) already reports every OMP call natively, so if the proxy variables also reached OMP, each call would be reported twice with different event IDs. `install` installs the plugin if it is missing, gives it your API key (a key already in the plugin's config is left alone), and records `"omp_via_plugin": true` in `config.json`. With that flag, `munch env` also defines an `omp` wrapper that runs the real `omp` with every variable `munch env` exports removed: an `omp` function in sh, bash, and zsh, a `function omp` in fish, and a `function omp` in PowerShell that restores the variables afterwards. Other tools keep routing through the proxy. The wrapper exists only in new shells, and `munch env --unset` removes it. `munch exec -- omp` likewise starts OMP without those variables, including any your shell already exported, and says so on stderr.
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

| Command | Description |
| --- | --- |
| `munch login [key]` | Validate and store your `tm_` API key (`--url` sets a self-hosted ingest URL, `--listen` the proxy address) |
| `munch run` | Run in the foreground (`--listen`, `--verbose`, `--config-dir`, `--state-dir`) |
| `munch start` / `stop` / `restart` | Start, stop, or restart the background daemon |
| `munch install [--yes] [--no-shell]` | One-shot setup: API key, background service (`systemd --user` on Linux, `launchd` on macOS), health check, omp-tm offer when `omp` is on `PATH`, and a shell profile block (see [Quick Start](#quick-start)) |
| `munch uninstall` | Remove the OS auto-start service, stop the proxy, and remove the shell profile block from the bash, zsh and fish profiles it may have written. It keeps `~/.config/tmproxy` and leaves the `omp-tokenmunchers` plugin installed |
| `munch status [--json]` | Show runtime status, uptime, queue depth, delivered counts, and last error |
| `munch pause` / `resume` | Stop/resume recording usage (AI traffic keeps proxying uninterrupted) |
| `munch flush` | Force-flush queued usage events immediately |
| `munch env [--check] [--unset] [--shell ...]` | Emit shell exports for common inference providers. When `omp` is on `PATH` or `omp_via_plugin` is set it also emits an `omp` wrapper that runs OMP without `OLLAMA_HOST` (or without every export when `omp_via_plugin` is set); `--unset` removes the wrapper too |
| `munch exec -- <cmd...>` | Verify the local daemon and run a command with private proxy URLs. `omp` gets the same variables its shell wrapper leaves out (and a one-line note when omp-tm is on) |
| `munch stats [--period today\|week\|all]` | Inspect local metadata-only usage summary (UTC) |
| `munch routes [--show-local-urls]` | List upstream targets; print private local URLs only on explicit request |

---

## Custom Routes & Pricing

### Custom routes (`~/.config/tmproxy/config.json`)

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

Use `https://` for remote upstreams. Cleartext `http://` is allowed only for numeric loopback addresses. Upstream base URLs cannot contain credentials, a query, or a fragment. Requests still carry their normal query parameters and provider authentication to the configured upstream; only configure destinations you trust with that data.

### Custom model pricing (`~/.config/tmproxy/prices.json`)

When an upstream reports cost directly (OpenRouter `usage.cost`, xAI `cost_in_usd_ticks`), `munch` uses the reported cost. Otherwise it estimates `cost_usd` using its built-in per-1M-token pricing table (fallback `0` for unknown models; local routes like `ollama` always report `0`). Override or add rates in `~/.config/tmproxy/prices.json`:

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

## Privacy & Security

- The listener is loopback-only, rejects browser-originated requests, and sandboxes upstream content. Inference requires a private URL; control requests require a separate local credential. `munch exec` and, by default, `munch env` verify that the daemon knows the installation key before launching a client or emitting URLs.
- Local capabilities protect against users who cannot read your private config or process environment. They do not protect against administrators, processes running as you, or a client that logs or forwards its URL. Don't publish the port through a tunnel, container mapping, or reverse proxy, and keep loopback out of any client-side HTTP proxy. Prefer `munch exec` over persistent exports: verification is done before launch, not for every SDK request, and a client can outlive the daemon.
- Provider authentication and inference payloads pass through to the configured upstream, without the private route prefix or control headers. Upstream redirects are rejected so clients cannot leak private loopback URLs by following them. The proxy processes request and response data in memory to extract usage, but does not include that content in its telemetry.
- The ingest destination must use HTTPS (or numeric-loopback HTTP for local testing) and redirects are not followed. TokenMunchers receives only the usage event fields: pseudonymous session and event IDs, timestamps, provider and model names, token counts, cost, and client metadata. Session pseudonyms are linkable within a daemon run but change after restart; the proxy does not upload the original session header or User-Agent as usage metadata. Model and provider names are still visible, so don't put secrets in custom names.
- Newly created `queue.jsonl` and `usage.jsonl` files are mode `0600` under the state directory. They contain usage metadata; `usage.jsonl` is retained locally for `munch stats`. Keep custom config and state directories private. `munch pause` stops recording new usage; it does not delete or stop sending previously queued events.
- Upgrading does not rewrite old queue files on disk. When the proxy next sends legacy entries, it replaces their original session IDs with a shared legacy label.
