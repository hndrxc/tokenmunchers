export interface Config {
  discordToken: string;
  applicationId: string;
  guildId: string;
  databaseUrl: string;
  dashboardUrl?: string;
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const missing: string[] = [];

  const discordToken = env.DISCORD_TOKEN?.trim();
  if (!discordToken) missing.push("DISCORD_TOKEN");

  const applicationId = env.DISCORD_APPLICATION_ID?.trim();
  if (!applicationId) missing.push("DISCORD_APPLICATION_ID");

  const guildId = env.DISCORD_GUILD_ID?.trim();
  if (!guildId) missing.push("DISCORD_GUILD_ID");

  const databaseUrl = env.DATABASE_URL?.trim();
  if (!databaseUrl) missing.push("DATABASE_URL");

  if (missing.length > 0) {
    throw new Error(
      `Missing required environment variable${missing.length > 1 ? "s" : ""}: ${missing.join(", ")}. Please configure them in your .env file.`,
    );
  }

  let dashboardUrl: string | undefined;
  if (env.DASHBOARD_URL?.trim()) {
    const raw = env.DASHBOARD_URL.trim();
    try {
      const parsed = new URL(raw);
      if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
        throw new Error("DASHBOARD_URL must use http: or https: protocol.");
      }
      dashboardUrl = parsed.toString();
    } catch (err) {
      if (err instanceof Error && err.message.includes("protocol")) {
        throw err;
      }
      throw new Error(`Invalid DASHBOARD_URL: must be a valid URL (received "${raw}").`);
    }
  }

  return {
    discordToken: discordToken!,
    applicationId: applicationId!,
    guildId: guildId!,
    databaseUrl: databaseUrl!,
    dashboardUrl,
  };
}
