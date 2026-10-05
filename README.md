# tokenmunchers

A live, invite-only leaderboard of AI token usage for a group of friends.

The `omp-tokenmunchers` plugin for [Oh My Pi (OMP)](https://github.com/can1357/oh-my-pi) reports the token counts of every model call. A [web dashboard](https://tokenmunchers.taiisshort.com) ranks everyone in real time: who's online, today's and this week's totals, all-time totals, and per-model breakdowns.

- **Real time.** A finished model call shows up on the dashboard in about a second.
- **Metadata only.** Token counts, model names and timestamps. Never prompts, responses, code or file paths.
- **Complete history.** Imports your usage from before you installed the plugin, and recovers calls that were missed.
- **Private by default.** Sign-in is invite-only, and every member can pause reporting at any time.

---

## Contents

- [Getting started](#getting-started)
- [Using the plugin](#using-the-plugin)
- [Privacy](#privacy)
- [Troubleshooting](#troubleshooting)
- [How it works](#how-it-works)
- [Repository layout](#repository-layout)

---

## Getting started

This guide takes you from nothing to appearing on the leaderboard in about ten minutes.

### Before you start

- An invite. Ask the group admin to add the email address of your **GitHub or Discord** account to the allowlist.
- macOS, Linux, or Windows. On Windows, either PowerShell or WSL works.

### 1. Install Bun

OMP and its plugins run on [Bun](https://bun.sh) (version **1.3.14 or newer**). Without it, `omp plugin install` fails. If you installed OMP some other way, install Bun anyway.

**macOS / Linux / WSL**

```bash
curl -fsSL https://bun.sh/install | bash
```

**Windows (PowerShell)**

```powershell
powershell -c "irm bun.sh/install.ps1 | iex"
```

Then **open a new terminal** so Bun is on your `PATH`, and check the version:

```bash
bun --version   # must print 1.3.14 or newer
```

If you already have an older Bun, update it with `bun upgrade`.

### 2. Install Oh My Pi

Skip this step if `omp --version` already works.

```bash
bun install -g @oh-my-pi/pi-coding-agent
omp --version
```

Run `omp` once and finish its first-run setup (choose a model, sign in to your provider) before continuing. See the [OMP README](https://github.com/can1357/oh-my-pi#readme) for other install methods.

### 3. Sign in to the dashboard

Open **[tokenmunchers.taiisshort.com](https://tokenmunchers.taiisshort.com)** and sign in with the GitHub or Discord account whose email was invited. If you see "not a member", your email isn't on the allowlist yet; check with the admin.

### 4. Create an API key

On the dashboard, go to **[Settings](https://tokenmunchers.taiisshort.com/settings) → API keys**, give the key a label (for example, the machine's name) and click **Create**. **Copy the key now.** It starts with `tm_` and is shown only once. Create a separate key for each computer you use.

### 5. Install the plugin

```bash
omp plugin install omp-tokenmunchers
```

### 6. Connect the plugin

Start `omp` and run:

```
/munch login tm_your_key_here
```

You should see *"logged in. Usage from this machine now shows on the dashboard."* Make any request in OMP. Within a few seconds it should appear in the dashboard's **Activity** feed, and you'll show under **Live now**.

### 7. Import your history (optional)

To include usage from before you installed the plugin:

```
/munch backfill
```

You'll see a preview (number of calls, total tokens, date range, and projects) before anything is uploaded. To leave a project out (for example, client work), exclude it by part of its name:

```
/munch backfill --exclude acme,secret-project
```

Only daily totals per model are uploaded. Running it again replaces your previous upload, so it's safe to repeat.

**You're done.** To keep the plugin up to date, re-run `omp plugin install omp-tokenmunchers` from time to time.

---

## Using the plugin

Everything is under one command, `/munch`, with tab completion:

| Command | What it does |
| --- | --- |
| `/munch` | Shows connection status, queued events and the last error |
| `/munch login [key]` | Connects this machine with an API key |
| `/munch pause` / `/munch resume` | Stops reporting for this OMP process (for private or client work), or turns it back on |
| `/munch sync` | Re-sends recent calls that didn't get through (this also happens automatically every minute) |
| `/munch backfill` | Imports your usage from before you installed the plugin |

The full command reference and configuration options are in the [plugin README](plugin/README.md).

---

## Privacy

For each model call the plugin sends the **provider, model, token counts, cost, timestamp, session id and a subagent flag**. Nothing else.

**Never sent:** prompts, responses, code, file paths, repository or project names, tool calls or their output, or your working directory. The server checks every request against a fixed list of allowed fields and rejects any request that contains anything else.

- **Pause** at any time with `/munch pause`. Calls made while paused are never uploaded, now or later.
- **History imports** (`/munch backfill`) are opt-in, show a preview first, and send only daily totals.
- **Delete your account** from Settings to permanently remove everything you've ever sent.

See [exactly which fields are sent](plugin/README.md#data-sent) in the plugin README.

---

## Troubleshooting

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

---

## How it works

```
 OMP + plugin ──── per call ────▶ ingest  (Edge Function) ──▶ Postgres ──Realtime──▶ Dashboard
      │                                                           ▲
      └──── /munch backfill ────▶ history (Edge Function) ────────┘
```

1. When a model call finishes, the plugin sends its token counts immediately. If the network is down, the call is queued on disk and retried.
2. The `ingest` Edge Function checks the request's fields and API key and stores the call. Each call has an ID, so a repeated send is stored only once.
3. Database jobs add calls up into daily totals every 10 minutes. Per-call data is kept for about a week; daily totals are kept forever.
4. The dashboard subscribes to new calls over Supabase Realtime, so it updates without reloading.

---

## Repository layout

| Folder | What's in it | Docs |
| --- | --- | --- |
| [`plugin/`](plugin/) | The `omp-tokenmunchers` OMP plugin (TypeScript, runs on Bun) | [plugin/README.md](plugin/README.md) |
| [`dashboard/`](dashboard/) | The web dashboard (Next.js, deployed on Vercel) | [dashboard/README.md](dashboard/README.md) |
| [`supabase/`](supabase/) | Database schema, security rules, scheduled jobs and Edge Functions | [supabase/README.md](supabase/README.md) |

Running your own instance, administering members, and deploying are covered in the [Supabase](supabase/README.md) and [dashboard](dashboard/README.md) READMEs. Plugin development and releases are covered in the [plugin README](plugin/README.md).
