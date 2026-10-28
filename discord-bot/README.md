# tokenmunchers Discord Bot

A standalone Discord bot providing a private `/leaderboard` slash command for [tokenmunchers](../README.md), matching the web dashboard's metrics and periods.

---

## Overview & Privacy Model

- **Guild-only visibility:** Tokenmunchers statistics are private. The bot accepts commands exclusively within the server configured via `DISCORD_GUILD_ID`. Slash commands are registered at guild scope, and execution from any other guild or direct messages is strictly denied (`setDMPermission(false)`).
- **Trusted-guild data visibility warning:** Anyone in the Discord server permitted to use the `/leaderboard` command can see aggregated statistics for **all** registered Tokenmunchers members. The bot does not verify whether the Discord user invoking the command is an invited Tokenmunchers site member, nor does it sync Discord users with site accounts. Only add the bot to a private, trusted Discord server whose members are authorized to see group stats, and use Discord's Integration settings to restrict command access if needed.
- **Read-only access:** The bot connects to PostgreSQL with a dedicated least-privilege role that can only invoke the `public.discord_leaderboard(p_period)` wrapper function. The bot cannot read raw event tables, read API keys or emails, or modify database state.
- **No per-user commands or writes:** The bot does not synchronize Discord member accounts with site profiles, store Discord user IDs, or accept user data reporting.
- **Persistent Gateway runtime:** The bot uses `discord.js` v14 to maintain a real-time WebSocket connection to the Discord Gateway. It requires long-running process hosting (e.g., systemd, Docker, VPS), and cannot run on serverless platforms like Next.js API routes or Vercel functions.
- **Minimal Gateway intents:** The client connects with `GatewayIntentBits.Guilds` only. No privileged intents (Message Content, Server Members, or Presences) are requested or required.

---

## Prerequisites

- **Node.js >= 22.18.0** (native TypeScript execution via type stripping)
- **npm**
- **Supabase / PostgreSQL database** with all migrations through `20261028010000_quality_accounting.sql` applied
- **Discord Developer account** with permissions to manage applications and invite bots to your target server

---

## 1. Discord Application Setup

1. Open the [Discord Developer Portal](https://discord.com/developers/applications) and click **New Application**.
2. Under **General Information**, copy the **Application ID** (`DISCORD_APPLICATION_ID`).
3. Under **Bot**:
   - Set the bot's username and avatar.
   - **Uncheck "Public Bot":** Keep the bot private so only the application owner can add it to servers.
   - **Privileged Gateway Intents:** Leave all privileged intents (**Presence Intent**, **Server Members Intent**, **Message Content Intent**) toggled **OFF**. The bot does not need them.
   - Click **Reset Token** to generate a new bot token, copy it (`DISCORD_TOKEN`), and store it securely. Never commit this token to version control.
4. Under **OAuth2 → URL Generator**:
   - **Scopes:** Select `bot` and `applications.commands`.
   - **Bot Permissions:** Select:
     - `View Channels`
     - `Send Messages`
     - `Embed Links`
   - Copy the generated URL and open it in a browser to invite the bot **only to your chosen trusted server**.
5. **Channel and Command Permissions:**
   - Server members can use slash commands by default.
   - Server administrators can restrict command access to specific channels or roles under **Server Settings → Integrations → Bots and Apps → tokenmunchers**.

---

## 2. Database Provisioning & Least Privilege

The bot accesses PostgreSQL directly through a dedicated database user granted only the `tokenmunchers_discord_reader` role.

### Security Architecture

- **Role sandbox:** Migration `20261026000000_discord_leaderboard.sql` creates the `tokenmunchers_discord_reader` role as `NOLOGIN`. It has `USAGE` on schema `public` and `EXECUTE` on `public.discord_leaderboard(text)`.
- **Zero raw table grants:** The role has **no** `SELECT`, `INSERT`, `UPDATE`, or `DELETE` permissions on tables (`usage_events`, `usage_daily`, `profiles`, `api_keys`, `allowlist`).
- **Definer wrapper:** `public.discord_leaderboard(p_period)` runs as `SECURITY DEFINER` with `SET search_path = ''`. Execution permissions are revoked from `PUBLIC`, `anon`, and `authenticated`, and granted strictly to `tokenmunchers_discord_reader`.
- **Safe output:** The wrapper returns only aggregated statistics (`handle`, `display_name`, `total_tokens`, `subagent_tokens`, `cost_usd`). It never exposes auth IDs, emails, or per-call timestamps.
- **Profile coverage:** Outer-joins `public.profiles` so all registered members appear. Only profiles with no usage receive zeros; unavailable accounting stays `null`.
- **Collector compatibility:** Canonical Claude Code/Codex contributions enter the same period totals once. Unavailable tokens/cost display **Unknown**, are unranked, and propagate to the group total rather than becoming fake zeros. Main-only totals stay unknown when subagent attribution is missing. Notes explain that collector usage may be reconstructed and USD can be reported or estimated, not invoices. The bot's role gains no raw/staging table privileges.

### Provisioning the Login User

Run the following SQL in your Supabase SQL Editor or Postgres administrative console:

```sql
CREATE ROLE tokenmunchers_bot WITH LOGIN PASSWORD 'your-strong-random-password' IN ROLE tokenmunchers_discord_reader;
```

**Security Rules:**
- **Never** grant `SUPERUSER`, `BYPASSRLS`, `CREATEDB`, or `CREATEROLE` to this role.
- **Never** use the Supabase `service_role` key or database superuser account for the bot.

### Connection String Formatting

```
DATABASE_URL=postgresql://tokenmunchers_bot:<escaped_password>@<db_host>:5432/postgres?sslmode=verify-full
```

- **Password escaping:** If the generated password contains special characters (`@`, `:`, `/`, `?`, `#`, etc.), URL-encode them (e.g. `@` becomes `%40`, `:` becomes `%3A`).
- **Verified TLS:** Always include `sslmode=verify-full`. Do not omit `sslmode` or use `sslmode=require`, as omitting it allows unencrypted plaintext connections and `require` may skip certificate verification. While `sslmode=verify-full` is standard in the connection URL, explicit driver TLS options verify the certificate chain and hostname in code.
- **Direct connection only:** Connect directly to PostgreSQL on port 5432 using the plain username `tokenmunchers_bot`. Do not use Supabase transaction pooling (port 6543) because the bot client enables prepared statements (`prepare: true`), which transaction poolers do not support.

### TLS Certificate Authority & Trust

Remote Supabase PostgreSQL instances use SSL certificates issued by Supabase's production root Certificate Authority (`Subject: Supabase Root 2021 CA`, expiring `2031-04-26 10:56:53 UTC`). Because Node.js does not include Supabase's private root CA in its default Mozilla trust store, standard Node TLS connections fail with `SELF_SIGNED_CERT_IN_CHAIN`.

- **Bundled official trust anchor:** The bot bundles the official Supabase production root CA at `certs/supabase-prod-ca.crt` and configures the PostgreSQL driver with `ssl: { ca: [...rootCertificates, supabaseCa], rejectUnauthorized: true }`. This preserves Node's built-in root certificates while adding Supabase's CA, enforcing both certificate chain validation and hostname verification.
- **Primary source references:**
  - Official Root CA download: [https://supabase-downloads.s3-ap-southeast-1.amazonaws.com/prod/ssl/prod-ca-2021.crt](https://supabase-downloads.s3-ap-southeast-1.amazonaws.com/prod/ssl/prod-ca-2021.crt)
  - Supabase Dashboard origin source: [custom-content.json#L63](https://github.com/supabase/supabase/blob/master/apps/studio/hooks/custom-content/custom-content.json#L63) and [SSLConfiguration.tsx#L114-L121](https://github.com/supabase/supabase/blob/master/apps/studio/components/interfaces/Settings/Database/SSLConfiguration.tsx#L114-L121)
- **Operator maintenance & CA rotation:**
  - The bundled CA certificate is valid through **2031-04-26**.
  - If Supabase rotates its root CA or prior to expiration, replace `certs/supabase-prod-ca.crt` strictly from the official Supabase source link and restart the bot daemon.
- **Security anti-patterns to avoid:**
  - **Never** disable TLS verification (`rejectUnauthorized: false`).
  - **Never** use `sslmode=require` to bypass TLS certificate chain verification.
  - **Never** set `NODE_TLS_REJECT_UNAUTHORIZED=0`.
  - **Do not** set `NODE_EXTRA_CA_CERTS` inside `.env`. Node's `--env-file` flag does not apply `NODE_EXTRA_CA_CERTS` before Node's internal TLS context initializes; bundling the CA directly in code solves this reliably.
---

## 3. Installation & Configuration

1. Clone or navigate to the `discord-bot` folder:
   ```bash
   cd discord-bot
   ```

2. Install dependencies:
   ```bash
   npm ci
   ```

3. Copy the configuration template:
   ```bash
   cp .env.example .env
   ```

4. Configure `.env`:
   ```dotenv
   # Discord Application & Bot Credentials
   DISCORD_TOKEN=your_bot_token_here
   DISCORD_APPLICATION_ID=your_application_id_here
   DISCORD_GUILD_ID=your_guild_id_here

   # PostgreSQL connection string
   DATABASE_URL=postgresql://tokenmunchers_bot:password@db.example.supabase.co:5432/postgres?sslmode=verify-full

   # Optional: link the embed title to the web dashboard (must be http:// or https://)
   DASHBOARD_URL=https://tokenmunchers.taiisshort.com
   ```

### Configuration Options

| Variable | Required | Description |
| --- | --- | --- |
| `DISCORD_TOKEN` | Yes | Discord Bot Token from the Developer Portal. |
| `DISCORD_APPLICATION_ID` | Yes | Discord Application (Client) ID. |
| `DISCORD_GUILD_ID` | Yes | Server ID where the bot operates. |
| `DATABASE_URL` | Yes | PostgreSQL connection string for the `tokenmunchers_bot` user. |
| `DASHBOARD_URL` | No | Web dashboard URL to link in embed titles. Validated for `http:` or `https:`. |

---

## 4. Commands & Scripts

| Script | Command | Purpose |
| --- | --- | --- |
| `npm run register` | `node --env-file=.env src/register.ts` | Registers the `/leaderboard` slash command in `DISCORD_GUILD_ID` via Discord REST API. |
| `npm start` | `node --env-file=.env src/index.ts` | Starts the bot daemon using Node native TypeScript execution. |
| `npm run typecheck` | `tsc --noEmit` | Runs TypeScript type checking without emitting files. |
| `npm test` | `node --test 'src/**/*.test.ts'` | Runs unit and interaction tests. |

### Registering Slash Commands

Before starting the bot for the first time, register the slash command with Discord:

```bash
npm run register
```

> **Note:** Slash command registration is scoped strictly to `DISCORD_GUILD_ID`. Guild command updates take effect immediately on Discord. You must re-run `npm run register` whenever command options or descriptions change. You **do not** need to re-run slash command registration after backend code, TLS, or database driver updates.

### Running the Bot

```bash
npm start
```

For production deployments, run the bot as a managed persistent service (e.g. using `systemd`, Docker, or a process manager). Whenever bot code, configuration, or certificates are updated, restart the running process or daemon so the changes take effect.

---

## 5. Slash Command Reference (`/leaderboard`)

```
/leaderboard [period: today | week | all] [metric: tokens | cost] [subagents: True | False] [page: integer >= 1]
```

### Options and Defaults

| Option | Type | Choices / Bounds | Default | Description |
| --- | --- | --- | --- | --- |
| `period` | String | `today`, `week`, `all` | `week` | Time period: `today` (UTC midnight to now), `week` (rolling past 7 days), or `all` (all-time). |
| `metric` | String | `tokens`, `cost` | `tokens` | Ranking metric: total tokens or estimated USD cost. |
| `subagents` | Boolean | `True`, `False` | `True` | Whether to include subagent calls (`True`) or main session only (`False`). |
| `page` | Integer | Min: `1` | `1` | Page number for paginated results (10 members per page). |

### Output Semantics & Rules

- **Group summary:** Embed header shows total tokens or cost across the entire group for the selected period.
- **Ranking order:** Members are ordered in descending order by the active metric score. Ties are broken alphabetically by display name (`display_name.localeCompare`).
- **Unranked zero-score members:** All registered profiles are listed. Members with zero usage in the selected period are displayed with an unranked dash (`–`) instead of a numerical rank.
- **Subagents toggle behavior:**
  - When `subagents: False` and `metric: tokens`, subagent tokens are subtracted from each member's total (`total_tokens - subagent_tokens`, clamped at 0).
  - When `subagents: True` and `metric: tokens`, members with subagent usage display their subagent share (e.g. `(42% subagent)`).
  - **Cost totals:** Cost figures represent overall expenditures recorded by OMP and history imports. Cost totals always include subagents regardless of the toggle setting; an informational note clarifies this in cost embeds.
- **Discord formatting safety:**
  - Display names are escaped against Discord Markdown syntax (`*`, `_`, `~`, `` ` ``, `|`, `<`, `>`, `:`, `@`, `#`).
  - Mentions are suppressed (`allowedMentions: { parse: [] }`) to prevent unintended pings.
  - Embed descriptions are bounded below Discord's 4,096 character limit.
- **Pagination:** Each page displays up to 10 members. The footer indicates `Page X of Y • Z members`.
- **Sanitized error handling:** Database or internal execution errors reply with a generic user-facing message (`Unable to load leaderboard at this time.`) without exposing SQL queries, error traces, or credentials in Discord. Mentions are suppressed (`allowedMentions: { parse: [] }`) and embeds are cleared. Diagnostic information is logged strictly to the process `stderr` stream (`console.error`) with a sanitized machine code (e.g. `[SELF_SIGNED_CERT_IN_CHAIN]`, `[42883]`, `[28P01]`) and static hint, never exposing connection strings, passwords, or query details.
- **Graceful shutdown:** Intercepts `SIGINT` and `SIGTERM` to close the Discord Gateway connection and drain the PostgreSQL connection pool.

---

## 6. Examples

- **Default leaderboard:**
  ```
  /leaderboard
  ```
  *Shows the past 7 days, ranked by total tokens (including subagents), page 1.*

- **Today's costs:**
  ```
  /leaderboard period:today metric:cost
  ```
  *Shows today's estimated costs since 00:00 UTC.*

- **All-time main session usage:**
  ```
  /leaderboard period:all subagents:False
  ```
  *Shows all-time rankings counting only main sessions (subagent tokens subtracted).*

- **Paginated view:**
  ```
  /leaderboard page:2
  ```
  *Shows members 11–20 for the past 7 days.*

---

## 7. Operator Troubleshooting

### Where Diagnostic Logs Appear

Database query failures or connection errors log sanitized machine error codes and static diagnostic hints strictly to process `stderr` (e.g. `Leaderboard command failed [SELF_SIGNED_CERT_IN_CHAIN]: Database TLS certificate verification failed; configure trusted database CA certificate.`). Discord users only receive the generic message `Unable to load leaderboard at this time.` to prevent credential leakage.

To inspect the `stderr` logs:
- **systemd:** `journalctl -u tokenmunchers-bot -e`
- **Docker:** `docker logs <container_name_or_id>`
- **Direct CLI:** Terminal output of the running `npm start` process

### Diagnostic Codes & Symptoms

| Symptom / Error Code | Cause | Solution |
| --- | --- | --- |
| `[SELF_SIGNED_CERT_IN_CHAIN]`, `[UNABLE_TO_VERIFY_LEAF_SIGNATURE]`, `[DEPTH_ZERO_SELF_SIGNED_CERT]` | Database TLS verification failed against Supabase CA | Ensure `discord-bot/certs/supabase-prod-ca.crt` is present and intact from the official Supabase source. Restart the bot process (`npm start`). Never disable TLS or use `NODE_TLS_REJECT_UNAUTHORIZED=0`. |
| `[42883]` | Undefined function `discord_leaderboard` | Database migration `20261026000000_discord_leaderboard.sql` has not been applied. Run `supabase db push` or apply the migration via SQL Editor. |
| `[42501]` | Insufficient privilege on `discord_leaderboard` | Dedicated login role was not created `IN ROLE tokenmunchers_discord_reader` or role grants are missing. Verify role inheritance in Supabase. |
| `[28P01]` | Authentication failed | Incorrect database username or password in `DATABASE_URL`. If the password contains special characters (`@`, `:`, `/`), URL-encode them. |
| `[ENOTFOUND]`, `[EAI_AGAIN]` | Hostname DNS resolution failed | Check the database host domain in `DATABASE_URL`. Verify server network connectivity and DNS resolution. |
| `[ECONNREFUSED]`, `[ETIMEDOUT]` | Connection refused or timed out | Database host is unavailable, port 5432 is blocked by firewall, or transaction pooler port (6543) was specified instead of direct port 5432. |
| `/leaderboard` does not appear in Discord | Slash command not registered or wrong server | Run `npm run register`. Verify `DISCORD_GUILD_ID` matches your server ID. |
| Bot replies: *"This command is only authorized for use within the configured server."* | Command received outside configured guild | The bot only operates inside `DISCORD_GUILD_ID`. Check that your server ID in `.env` matches the server. |
| Bot fails to reply or says *"The application did not respond"* | Missing channel permissions | Ensure the bot has `View Channel`, `Send Messages`, and `Embed Links` permissions in that channel. |
| `Missing required environment variables` on start | Missing or empty keys in `.env` | Copy `.env.example` to `.env` and verify all required variables are set. |
| `Invalid DASHBOARD_URL` on start | Malformed URL scheme | `DASHBOARD_URL` must start with `http://` or `https://`. |
| Gateway disconnects / timeouts | Host sleep or serverless environment | The bot requires a persistent process. Host on a server, VPS, or container daemon rather than serverless request functions. |
