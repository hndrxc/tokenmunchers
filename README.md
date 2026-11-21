# tokenmunchers

A live, invite-only leaderboard of AI token usage for a group of friends.

Small programs on your computer report the token counts of your AI model calls, and a [web dashboard](https://tokenmunchers.taiisshort.com) ranks everyone in real time: who's online, today's and this week's totals, all-time totals, and per-model breakdowns.

## What's in tokenmunchers

| Part | What it is | Docs |
| --- | --- | --- |
| **Site** | The dashboard at [tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com): leaderboard, Live now, activity feed, profiles, and Settings → API keys. | [dashboard/](dashboard/README.md) |
| **Service** | Supabase Edge Functions (`ingest`, `history`, `contributions`) that check every field and API key before storing anything. `contributions` accepts uploads of Claude Code and Codex usage collected earlier. | [supabase/](supabase/README.md) |
| **Database** | Supabase Postgres: per-call events (kept about a week), daily totals (kept forever), imported history, and scheduled rollups. | [supabase/](supabase/README.md) |
| **Plugins** | `omp-tokenmunchers` for Oh My Pi. | [plugin/](plugin/README.md) |
| **Program** | `munch`: a small local proxy that records token usage for Claude Code, Codex, Gemini, Grok, OpenRouter, Ollama and other tools. | [proxy/](proxy/README.md) |

Pick one or more ways to report your usage. They all feed the same leaderboard.

- **Real time.** A finished model call shows up on the dashboard in about a second.
- **Metadata only.** Token counts, model names and timestamps. Never prompts, responses, code or file paths.
- **Complete history.** The OMP plugin can import usage from before you installed it, and recover calls that were missed. (`munch` records from the moment your tools are routed through it; it doesn't import history.)
- **Private by default.** Sign-in is invite-only, and every member can pause reporting at any time.

---

## Contents

- [Getting started](#getting-started)
- [Everyday use](#everyday-use)
- [Privacy](#privacy)
- [Troubleshooting](#troubleshooting)
- [How it works](#how-it-works)
- [Repository layout](#repository-layout)

---

## Getting started

This guide takes you from nothing to appearing on the leaderboard in about ten minutes.

### 1. Get an invite and sign in

Ask the group admin to add the email address of your **GitHub or Discord** account to the allowlist. Then open **[tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com)** and sign in with that account. If you see "not a member", your email isn't on the allowlist yet; check with the admin.

### 2. Create an API key

On the dashboard, go to **[Settings](https://tokenmunchers.taiisshort.com/settings) → API keys**, give the key a label (for example, the machine's name) and click **Create**. **Copy the key now.** It starts with `tm_` and is shown only once. Create a separate key for each computer you use.

### 3. Choose how to report

| If you... | Use |
| --- | --- |
| Use **OMP** (Oh My Pi) | The `omp-tokenmunchers` plugin: [Option B](#option-b-omp-plugin-only) |
| Use **Claude Code, Codex, Gemini or other tools**, or several tools at once | `munch`: [Option A](#option-a-munch) |

If OMP is installed, `munch install` offers to set up the OMP plugin for you, so OMP users who also use other tools can do everything with `munch`.

#### Option A: munch

`munch` runs in the background. Your AI tools send requests through it; it reads the token counts from the replies and sends only usage metadata to tokenmunchers. It supports Anthropic, OpenAI/ChatGPT/Codex, Gemini, xAI/Grok, OpenRouter and Ollama. The shell setup routes most tools automatically; the installer offers separate setup for Codex signed in with ChatGPT.

On Linux or macOS, download and run the installer. It installs the latest stable Munch binary in `~/.local/bin` and verifies its SHA-256 checksum. No Go or `sudo` required.

```bash
curl -fsSL https://raw.githubusercontent.com/hndrxc/tokenmunchers/main/install-munch.sh -o install-munch.sh &&
  sh install-munch.sh
```

See [install options](proxy/README.md#install-a-release) or [build from source](proxy/README.md#build-from-source).

To upgrade an installed release, run `munch update`, then `munch restart` if the background service is running. For older versions without `munch update`, re-run the download script above.

Make sure `~/.local/bin` is on your `PATH`, then run the one-step setup:

```bash
munch install
```

It goes through these steps and prints a line for each:

1. **API key.** Uses a key you've already saved, then `TOKENMUNCHERS_KEY` or `TOKENMUNCHERS_API_KEY` from your environment. If there's neither, it asks you to paste your `tm_` key.
2. **Background service.** Installs and starts it (`systemd --user` on Linux, `launchd` on macOS, or a plain background process if neither is available), then waits for a health check.
3. **OMP.** Only if `omp` is installed: asks whether to report OMP with the `omp-tokenmunchers` plugin instead of the proxy. Say yes (the default) so OMP calls aren't counted twice. It installs the plugin and gives it the same key.
4. **Shell profile.** Adds a clearly marked block to `~/.bashrc`, `~/.zshrc` or fish's `conf.d/munch.fish` so new terminals route your tools through `munch`.
5. **Codex.** If the Codex CLI is installed and signed in with ChatGPT, offers to add a Munch model provider. Restart Codex afterwards.
6. **Summary.** Shows what it did.

`munch install --yes` skips the questions (it needs a saved key or `TOKENMUNCHERS_KEY`/`TOKENMUNCHERS_API_KEY` already set). `--no-shell` leaves the shell profile alone; `--no-codex` leaves your Codex config alone. See [manual Codex setup](proxy/README.md#4-codex-chatgpt-sign-in) if the installer skips it.

**Open a new terminal** so your shell picks up the change; tools that were already running keep their old settings. Then check it works:

```bash
munch status
```

Make a request in a tool routed through `munch`. It should appear in the dashboard's **Activity** feed within a few seconds, and you'll show under **Live now**.

#### Option B: OMP plugin only

Use this if you only use OMP. Windows works here too, with either PowerShell or WSL.

**Step B1. Install Bun.** OMP and its plugins run on [Bun](https://bun.sh) (version **1.3.14 or newer**). Without it, `omp plugin install` fails. If you installed OMP some other way, install Bun anyway.

*macOS / Linux / WSL*

```bash
curl -fsSL https://bun.sh/install | bash
```

*Windows (PowerShell)*

```powershell
powershell -c "irm bun.sh/install.ps1 | iex"
```

Then **open a new terminal** so Bun is on your `PATH`, and check the version:

```bash
bun --version   # must print 1.3.14 or newer
```

If you already have an older Bun, update it with `bun upgrade`.

**Step B2. Install Oh My Pi.** Skip this if `omp --version` already works.

```bash
bun install -g @oh-my-pi/pi-coding-agent
omp --version
```

Run `omp` once and finish its first-run setup (choose a model, sign in to your provider) before continuing. See the [OMP README](https://github.com/can1357/oh-my-pi#readme) for other install methods.

**Step B3. Install the plugin.**

```bash
omp plugin install omp-tokenmunchers
```

**Step B4. Connect the plugin.** Start `omp` and run:

```
/munch login tm_your_key_here
```

You should see *"logged in. Usage from this machine now shows on the dashboard."* Make any request in OMP. Within a few seconds it should appear in the dashboard's **Activity** feed, and you'll show under **Live now**.

To keep the plugin up to date, re-run `omp plugin install omp-tokenmunchers` from time to time.

### 4. Check the dashboard

Open the [dashboard](https://tokenmunchers.taiisshort.com). Your calls appear in **Activity**, and you show under **Live now** while you're working. Today's and this week's totals update right away; the per-day charts catch up within about ten minutes.

### 5. Import your history (optional)

To include OMP usage from before you installed the plugin:

```
/munch backfill
```

You'll see a preview (number of calls, total tokens, date range, and projects) before anything is uploaded. To leave a project out (for example, client work), exclude it by part of its name:

```
/munch backfill --exclude acme,secret-project
```

Only daily totals per model are uploaded. Running it again replaces your previous upload, so it's safe to repeat.

Claude Code and Codex history import isn't available at the moment. `munch` records those tools from the moment they are routed through it.

**You're done.**

---

## Everyday use

> **`munch` vs `/munch`.** `munch` typed in a terminal is the program. `/munch` inside OMP is a plugin command. They are different things.

### OMP plugin: `/munch`

Everything is under one command, `/munch`, with tab completion:

| Command | What it does |
| --- | --- |
| `/munch` | Shows connection status, queued events and the last error |
| `/munch login [key]` | Connects this machine with an API key |
| `/munch pause` / `/munch resume` | Stops reporting for this OMP process (for private or client work), or turns it back on |
| `/munch sync` | Re-sends recent calls that didn't get through (this also happens automatically every minute) |
| `/munch backfill` | Imports your usage from before you installed the plugin |

The full command reference and configuration options are in the [plugin README](plugin/README.md).

### Program: `munch`

Type these in a terminal:

| Command | What it does |
| --- | --- |
| `munch status` | Shows whether the proxy is running, its queue and the last error |
| `munch pause` / `munch resume` | Stops or restarts recording usage. Your tools keep working either way |
| `munch stats` | Shows your recorded usage on this machine (`--period today`, `week` or `all`) |
| `munch exec -- <command>` | Runs one command through the proxy, for example `munch exec -- claude` |
| `munch env` | Prints the shell settings that route tools through the proxy |
| `munch uninstall` | Removes the background service and the shell profile block. It leaves the OMP plugin installed |

The full command list, routes and settings are in the [munch README](proxy/README.md). Its settings stay in `~/.config/tmproxy/` and its service is called `tmproxy.service`; those names are intentional.

---

## Privacy

For each model call the OMP plugin sends the **provider, model, token counts, cost, timestamp, session id and a subagent flag**. Nothing else.

**Never sent:** prompts, responses, code, file paths, repository or project names, tool calls or their output, or your working directory. The server checks every request against a fixed list of allowed fields and rejects any request that contains anything else.

- **Pause** at any time with `/munch pause` (OMP) or `munch pause` (the program). Calls made while paused are never uploaded, now or later.
- **History imports** (`/munch backfill`) are opt-in, show a preview first, and send only daily totals.
- **Delete your account** from Settings to permanently remove everything you've ever sent.
- **`munch`** sits between your tool and the provider. It reads each request and response in memory only to find the token usage, and sends tokenmunchers metadata-only events: pseudonymous session and event IDs, timestamps, provider, model, token counts, cost and client version. Your provider credentials and your prompts go only to the provider, never to tokenmunchers. `munch pause` stops recording new usage. It listens on your own computer only. See [munch privacy and security](proxy/README.md#privacy--security).

See [exactly which fields the OMP plugin sends](plugin/README.md#data-sent) in the plugin README.

---

## Troubleshooting

### OMP plugin

**`bun: command not found` right after installing Bun**
Open a new terminal. If it still isn't found, add `~/.bun/bin` to your `PATH` (on Windows, `%USERPROFILE%\.bun\bin`).

**`omp plugin install` fails, or `omp` isn't found**
Check that `bun --version` prints 1.3.14 or newer (update with `bun upgrade`), then reinstall OMP with `bun install -g @oh-my-pi/pi-coding-agent`.

**`/munch` is an unknown command**
The plugin isn't loaded. Run `omp plugin install omp-tokenmunchers` again and restart `omp`.

**"API key was rejected"**
The key was revoked or mistyped. Create a new key under Settings and run `/munch login` again.

**My calls don't show up**
- Run `/munch`. Check that the state is `reporting` (not paused) and look at the last error.
- If events are queued, they retry automatically. `/munch sync` also re-sends anything missed in the last week.
- A call only appears once its response finishes, so a long-running request appears when it completes.

**`/munch backfill` says "couldn't find OMP's stats database"**
Run `omp stats` once to build it, then try again.

**Other usage is still queued**
Backfill waits until queued calls have been delivered, so those calls don't get counted twice. Wait a minute, check `/munch`, and try again.

### munch

**`munch: command not found`**
Make sure `~/.local/bin` is on your `PATH` and open a new terminal. If the file isn't there, build it again with `cd tokenmunchers/proxy && go build -o ~/.local/bin/munch ./cmd/munch`.

**My calls don't show up with munch**
- Run `munch status` and check that it's running and not paused, and look at the last error.
- Open a new terminal after `munch install`. Terminals and tools that were already running keep their old settings; restart them too.
- Or run a single command through the proxy to test: `munch exec -- <command>`.
- If you use Codex signed in with ChatGPT, run `munch install` and accept the Codex setup, then restart Codex. If the installer skips it, see the [manual provider setup](proxy/README.md#4-codex-chatgpt-sign-in). An old plain-HTTP `chatgpt_base_url` can cause an HTTPS-origin error.

**OMP calls are counted twice**
Both the OMP plugin and `munch` are reporting them. Run `munch install` again and accept the omp-tm question, so OMP is reported by its plugin only. Or, to report OMP through `munch` instead, run `omp plugin disable omp-tokenmunchers`.

**OMP warns `model discovery at http://127.0.0.1:8787/ (from OLLAMA_HOST) failed`**
Open a new terminal after running `munch install`. The `omp` in that terminal runs without `OLLAMA_HOST`, so the warning goes away.

**Upgrading from the old `tmproxy` name**
Delete the `eval "$(tmproxy env --check)"` line you added to your shell profile, and delete the old `tmproxy` binary. Then run `munch install`. It reuses your saved key and settings.

**Upgrading from the retired Claude Code and Codex plugins**
If you installed the old collector plugins, uninstall them so they stop uploading: `claude plugin uninstall tokenmunchers@tokenmunchers` and `codex plugin remove tokenmunchers-codex@tokenmunchers`. Usage they uploaded before stays on the leaderboard.

---

## How it works

```
  OMP + plugin ──────────── per call ───┐
                                        ▼
  munch (local proxy) ── per call ──▶ ingest (Edge Function) ──▶ Postgres ──Realtime──▶ Site (dashboard)
  (Claude Code, Codex, Gemini, ...)                               ▲
                                                                  │
                                                                  │
  OMP + plugin ── /munch backfill ──▶ history (Edge Function) ────┤
                                                                  │
  Claude Code / Codex (uploaded earlier) ─▶ contributions ────────┘
```

1. When a model call finishes, the reporter sends its token counts immediately. The OMP plugin does this from inside OMP. `munch` does it by reading the usage in the response as it passes through the local proxy, which only listens on your own computer. If the network is down, the call is queued on disk and retried.
2. The **service**'s `ingest` Edge Function checks the request's fields and API key and stores the call. Each call has an ID, so a repeated send is stored only once.
3. The **database** adds calls up into daily totals every 10 minutes (scheduled Postgres jobs). Per-call data is kept for about a week; daily totals are kept forever.
4. The **site** subscribes to new calls over Supabase Realtime, so it updates without reloading.
5. `/munch backfill` sends daily totals to the `history` function. The `contributions` function accepts uploads of Claude Code and Codex usage collected earlier; the daily views still count that previously uploaded usage alongside live and OMP usage.

---

## Repository layout

| Folder | What's in it | Docs |
| --- | --- | --- |
| [`plugin/`](plugin/) | The `omp-tokenmunchers` OMP plugin (TypeScript, runs on Bun) | [plugin/README.md](plugin/README.md) |
| [`proxy/`](proxy/) | `munch`, the local proxy program (Go 1.24+, standard library only) | [proxy/README.md](proxy/README.md) |
| [`dashboard/`](dashboard/) | The web dashboard (Next.js, deployed on Vercel) | [dashboard/README.md](dashboard/README.md) |
| [`discord-bot/`](discord-bot/) | Guild slash-command bot for Discord (Node.js, discord.js) | [discord-bot/README.md](discord-bot/README.md) |
| [`supabase/`](supabase/) | Database schema, security rules, scheduled jobs and Edge Functions | [supabase/README.md](supabase/README.md) |

Running your own instance, administering members, running the Discord bot, and deploying are covered in the [Supabase](supabase/README.md), [discord-bot](discord-bot/README.md), and [dashboard](dashboard/README.md) READMEs. Plugin development and releases are covered in the [plugin README](plugin/README.md). Building and configuring `munch` is covered in the [proxy README](proxy/README.md).
