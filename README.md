# tokenmunchers

A live, invite-only leaderboard of AI token usage for a group of friends.

The [dashboard](https://tokenmunchers.taiisshort.com) shows live activity, daily and weekly totals, and per-model breakdowns.

## What's in tokenmunchers

| Part | What it is | Docs |
| --- | --- | --- |
| **Site** | Leaderboard, Activity, profiles, and API key settings. | [dashboard/](dashboard/README.md) |
| **Service** | `ingest` receives live plugin and proxy calls, `history` receives OMP imports, and `contributions` receives live desktop usage and Codex backfill. | [supabase/](supabase/README.md) |
| **Database** | Postgres stores usage and rolls it up into daily totals. | [supabase/](supabase/README.md) |
| **Plugin** | `omp-tokenmunchers` reports Oh My Pi usage and imports OMP history. | [plugin/](plugin/README.md) |
| **Program** | `munch` proxies tools such as Claude Code, Codex, and Gemini, collects local desktop usage, and imports retained Codex and Claude Code history. | [proxy/](proxy/README.md) |

## Contents

- [Getting started](#getting-started)
- [Windows PowerShell](#windows-powershell)
- [Everyday use](#everyday-use)
- [Privacy](#privacy)
- [Troubleshooting](#troubleshooting)
- [How it works](#how-it-works)
- [Repository layout](#repository-layout)

## Getting started

Choose the [OMP plugin](#option-b-omp-plugin-only) if you only use Oh My Pi. Use [munch](#option-a-munch) for other tools or a mix; if it finds OMP, setup can install the plugin for you.

### 1. Get an invite and sign in

Ask the group admin to allowlist the email address of your GitHub or Discord account, then sign in to the [dashboard](https://tokenmunchers.taiisshort.com) with that account. If you see “not a member,” check with the admin.

### 2. Create an API key

In [Settings](https://tokenmunchers.taiisshort.com/settings) → API keys, create a key for this computer and copy it. The `tm_` key is shown only once. Use a separate key per computer.

### 3. Choose how to report

#### Option A: munch

On Windows, use the [PowerShell installer](#windows-powershell). On Linux or macOS, install the binary:

```bash
curl -fsSL https://tokenmunchers.taiisshort.com/install.sh | sh
```

Make sure `~/.local/bin` is on your `PATH`. Run `munch install` and enter your `tm_` key. Setup starts the background service and offers to configure detected tools. Open a new terminal and restart the tools you configured. See [install details](proxy/README.md#install-details) for review-first and binary-only options.

#### Windows PowerShell

In PowerShell 5.1 or newer on Windows 10 or 11 (`amd64` or `arm64`), run:

```powershell
irm https://tokenmunchers.taiisshort.com/install.ps1 | iex
```

Enter your `tm_` key and choose which detected tools to configure. Munch doesn't edit your PowerShell profile: to route CLI tools in a given terminal, run the routing command printed by setup there, or launch one tool with `munch exec -- <command>`. If setup configured Codex with ChatGPT sign-in, start a new Codex process. If it enabled Claude Desktop collection, restart Claude Desktop and start a new local Code session.

Munch stops when you sign out of Windows; run `munch start` after signing in. See [install details](proxy/README.md#install-details) for review-first and binary-only options.

#### Option B: OMP plugin only

Install [Bun](https://bun.sh) 1.3.14 or newer on Linux, macOS, WSL, or native Windows. Open a new terminal and check `bun --version`; use `bun upgrade` if needed.

If `omp --version` doesn't work, install Oh My Pi with `bun install -g @oh-my-pi/pi-coding-agent`, then run `omp` once to finish provider setup.

```sh
omp plugin install omp-tokenmunchers
```

Start OMP and enter `/munch login tm_your_key_here`. See the [plugin README](plugin/README.md) for setup and configuration.

### 4. Check the dashboard

Run `munch status` in your terminal or `/munch` inside OMP. Make a request in a configured tool, then look for it in the dashboard's **Activity** feed after the response finishes.

### 5. Import your history (optional)

- **OMP:** Run `/munch backfill` inside OMP. It previews the import and uploads only daily totals per model. Use `--exclude` to skip projects; repeating an upload replaces the previous one. See the [plugin README](plugin/README.md).
- **Codex:** Run `munch backfill` to preview all retained usage. To upload, independently verify a pre-proxy UTC window, pass `--from` and `--before`, and confirm with `yes`. Desktop-collected usage is excluded, and the server keeps one copy per Codex response ID. [Codex backfill](proxy/README.md#codex-history-backfill) counts toward leaderboard totals but doesn't appear in Activity.
- **Claude Code:** Run `munch backfill --harness claude-code` to preview all retained CLI and local Desktop Code usage with verified final counts. Upload requires the Claude backfill server update and the same verified pre-proxy window and confirmation. See [Claude Code backfill](proxy/README.md#claude-code-history-backfill).

## Everyday use

Use `/munch` inside OMP and `munch` in a terminal.

### OMP plugin: `/munch`

| Command | What it does |
| --- | --- |
| `/munch` | Shows connection status, queued events and the last error |
| `/munch login [key]` | Connects this machine with an API key |
| `/munch pause` / `/munch resume` | Stops reporting for this OMP process (for private or client work), or turns it back on |
| `/munch sync` | Re-sends recent calls that didn't get through (this also happens automatically every minute) |
| `/munch backfill` | Imports your usage from before you installed the plugin |

See the [plugin README](plugin/README.md) for all commands and configuration.

### Program: `munch`

| Command | What it does |
| --- | --- |
| `munch status` | Check whether the proxy is running and see queued events and the last error |
| `munch pause` / `munch resume` | Pause or resume usage recording without stopping your tools |
| `munch stats` | View usage recorded on this machine |
| `munch exec -- <command>` | Run one command through the proxy, for example `munch exec -- claude` |
| `munch desktop claude\|codex on\|off` | Toggle collection for Claude Desktop local Code sessions or Codex desktop sessions on “This computer” |
| `munch backfill [--harness codex\|claude-code]` | Preview or import retained Codex or Claude Code history (Codex by default) |
| `munch uninstall` | Remove the background service and Munch-managed shell, Codex, and Claude `settings.json` telemetry settings. Keeps the binary, saved key and settings, and OMP plugin. |

See the [munch README](proxy/README.md) for all commands and [what Munch changes](proxy/README.md#what-munch-changes).

### Updating

- **Linux/macOS:** Run `munch update`, then `munch restart`.
- **Native Windows:** `munch update` isn't supported. Run `munch stop`, then rerun the [PowerShell installer](#windows-powershell), or use its [`-DownloadOnly` option](proxy/README.md#install-details) and run `munch start`.
- **OMP plugin:** Rerun `omp plugin install omp-tokenmunchers`.

## Privacy

Reporters send usage metadata, not prompts, responses, code, or file paths. See the [OMP fields](plugin/README.md#data-sent) and [munch privacy and security notes](proxy/README.md#privacy--security).

- Pause reporting with `/munch pause` in OMP or `munch pause` in your terminal. If you use both reporters, pause both.
- History imports are opt-in and previewed locally before upload.
- To remove your account and uploaded usage permanently, use **Delete account** in Settings.

## Troubleshooting

- **Command not found:** Open a new terminal after installing Bun, OMP, or Munch. Check that the install directory is on `PATH` (`~/.local/bin` for Munch on Linux/macOS or `%LOCALAPPDATA%\Programs\Munch` on Windows by default).
- **`/munch` is unknown:** Run `omp plugin install omp-tokenmunchers` and restart OMP.
- **API key rejected:** Create a new key in Settings. For OMP, run `/munch login` with the new key. For the proxy, use `munch login` and then `munch restart`; see the [munch README](proxy/README.md#desktop-accounting-pilot) before changing keys if desktop collection is enabled.
- **Calls missing:** Check `/munch` or `munch status` for a pause, queued events, or errors. Restart configured tools. See [Windows CLI routing](#windows-powershell) or [Codex with ChatGPT sign-in](proxy/README.md#4-codex-chatgpt-sign-in) if applicable.
- **OMP calls counted twice:** Use one reporter for OMP. Rerun `munch install` and accept OMP plugin setup, or disable the plugin with `omp plugin disable omp-tokenmunchers` if using the proxy for OMP.
- **Still using retired Claude Code or Codex plugins:** Uninstall them with `claude plugin uninstall tokenmunchers@tokenmunchers` or `codex plugin remove tokenmunchers-codex@tokenmunchers`. Their earlier uploads stay on the leaderboard.

See the [plugin README](plugin/README.md) and [munch README](proxy/README.md) for more help.

## How it works

```text
OMP plugin and local proxy (live calls)     → ingest        ┐
OMP plugin (/munch backfill)                 → history       ├→ Postgres → dashboard
Munch desktop collection and munch backfill → contributions ┘
```

1. The OMP plugin and proxy send usage when calls finish. Desktop collection reads telemetry from local Claude Code sessions in Claude Desktop and follows local Codex rollouts.
2. Edge Functions validate and store usage. Postgres rolls it up into daily totals, including earlier contributions from the retired collectors.
3. The dashboard subscribes to live activity and displays totals, including imported history.

## Repository layout

| Folder | What's in it | Docs |
| --- | --- | --- |
| [`plugin/`](plugin/) | The `omp-tokenmunchers` OMP plugin (TypeScript, runs on Bun) | [plugin/README.md](plugin/README.md) |
| [`proxy/`](proxy/) | `munch`: local proxy, desktop collector, and Codex and Claude Code history import (Go 1.24) | [proxy/README.md](proxy/README.md) |
| [`dashboard/`](dashboard/) | The web dashboard (Next.js, deployed on Vercel) | [dashboard/README.md](dashboard/README.md) |
| [`discord-bot/`](discord-bot/) | Guild slash-command bot for Discord (Node.js, discord.js) | [discord-bot/README.md](discord-bot/README.md) |
| [`supabase/`](supabase/) | Database schema, security rules, scheduled jobs and Edge Functions | [supabase/README.md](supabase/README.md) |
