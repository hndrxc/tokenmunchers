import { REST, Routes } from "discord.js";
import { leaderboardCommandData } from "./command.ts";
import { loadConfig } from "./config.ts";

async function main(): Promise<void> {
  const config = loadConfig();

  const rest = new REST({ version: "10" }).setToken(config.discordToken);

  console.log(
    `Registering /leaderboard command to guild ${config.guildId} (app: ${config.applicationId})...`,
  );

  try {
    const result = (await rest.put(
      Routes.applicationGuildCommands(config.applicationId, config.guildId),
      { body: [leaderboardCommandData.toJSON()] },
    )) as unknown[];

    console.log(
      `Successfully registered ${result.length} guild command(s) in guild ${config.guildId}.`,
    );
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    console.error(`Failed to register slash command: ${message}`);
    process.exit(1);
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}

export { main as registerCommands };
