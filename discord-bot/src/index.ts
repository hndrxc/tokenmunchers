import { Client, Events, GatewayIntentBits } from "discord.js";
import { handleLeaderboardCommand } from "./command.ts";
import { loadConfig } from "./config.ts";
import { createDatabase } from "./db.ts";

export { handleLeaderboardCommand } from "./command.ts";
export { buildLeaderboardEmbed, formatLeaderboard } from "./leaderboard.ts";
export { createDatabase } from "./db.ts";
export { loadConfig } from "./config.ts";
export type { LeaderboardLoader, LeaderboardMetric, LeaderboardPeriod, LeaderboardRow } from "./db.ts";
export type { FormattedLeaderboard, LeaderboardOptions } from "./leaderboard.ts";

async function main(): Promise<void> {
  const config = loadConfig();
  const db = createDatabase(config.databaseUrl);

  const client = new Client({
    intents: [GatewayIntentBits.Guilds],
    allowedMentions: { parse: [] },
  });

  client.once(Events.ClientReady, (readyClient) => {
    console.log(`Tokenmunchers Discord Bot logged in as ${readyClient.user.tag}`);
    console.log(`Listening for /leaderboard in guild ${config.guildId}`);
  });

  client.on(Events.InteractionCreate, async (interaction) => {
    if (!interaction.isChatInputCommand()) return;
    if (interaction.commandName !== "leaderboard") return;

    try {
      await handleLeaderboardCommand(interaction, config, db.fetchLeaderboard);
    } catch {
      console.error("Unhandled interaction execution failure.");
    }
  });

  let stopping = false;
  const shutdown = async (signal: string) => {
    if (stopping) return;
    stopping = true;
    console.log(`Received ${signal}, closing bot and database connections...`);
    try {
      await client.destroy();
      await db.close();
      console.log("Shutdown complete.");
    } catch (err) {
      console.error("Error during shutdown:", err);
    } finally {
      process.exit(0);
    }
  };

  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));

  try {
    await client.login(config.discordToken);
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    console.error(`Failed to login to Discord: ${message}`);
    await db.close();
    process.exit(1);
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
