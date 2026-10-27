import { SlashCommandBuilder } from "discord.js";
import type {
  InteractionDeferReplyOptions,
  InteractionEditReplyOptions,
} from "discord.js";
import type { LeaderboardLoader, LeaderboardMetric, LeaderboardPeriod } from "./db.ts";
import { buildLeaderboardEmbed } from "./leaderboard.ts";

export interface LeaderboardInteraction {
  guildId: string | null;
  options: {
    getString(name: string): string | null;
    getBoolean(name: string): boolean | null;
    getInteger(name: string): number | null;
  };
  deferReply(options?: InteractionDeferReplyOptions): Promise<unknown>;
  editReply(options: InteractionEditReplyOptions): Promise<unknown>;
}

export const leaderboardCommandData = new SlashCommandBuilder()
  .setName("leaderboard")
  .setDescription("Show the tokenmunchers leaderboard")
  .setDMPermission(false)
  .addStringOption((option) =>
    option
      .setName("period")
      .setDescription("Time period")
      .setRequired(false)
      .addChoices(
        { name: "today", value: "today" },
        { name: "week", value: "week" },
        { name: "all", value: "all" },
      ),
  )
  .addStringOption((option) =>
    option
      .setName("metric")
      .setDescription("Ranking metric")
      .setRequired(false)
      .addChoices(
        { name: "tokens", value: "tokens" },
        { name: "cost", value: "cost" },
      ),
  )
  .addBooleanOption((option) =>
    option
      .setName("subagents")
      .setDescription("Include subagents (true) or main session only (false)")
      .setRequired(false),
  )
  .addIntegerOption((option) =>
    option
      .setName("page")
      .setDescription("Page number (10 members per page)")
      .setRequired(false)
      .setMinValue(1),
  );

const SAFE_CODE_PATTERN = /^(?:[0-9A-Z]{5}|[A-Z][A-Z0-9_]{1,63})$/;

function extractSafeErrorCode(err: unknown): string {
  if (err && typeof err === "object" && "code" in err) {
    const rawCode = err.code;
    if (typeof rawCode === "string") {
      const trimmed = rawCode.trim();
      if (SAFE_CODE_PATTERN.test(trimmed)) {
        return trimmed;
      }
    }
  }
  return "UNKNOWN";
}

function getDiagnosticHint(code: string): string | undefined {
  switch (code) {
    case "SELF_SIGNED_CERT_IN_CHAIN":
    case "UNABLE_TO_VERIFY_LEAF_SIGNATURE":
    case "DEPTH_ZERO_SELF_SIGNED_CERT":
      return "Database TLS certificate verification failed; configure trusted database CA certificate.";
    case "42883":
      return "Database function or wrapper missing; verify schema or migrations.";
    case "42501":
      return "Database permission denied; verify database role privileges.";
    case "28P01":
      return "Database authentication failed; verify database credentials.";
    case "ENOTFOUND":
    case "EAI_AGAIN":
      return "Database host lookup failed; verify DNS hostname resolution.";
    case "ECONNREFUSED":
    case "ETIMEDOUT":
      return "Database connection refused or timed out; verify host availability.";
    default:
      return undefined;
  }
}

export async function handleLeaderboardCommand(
  interaction: LeaderboardInteraction,
  config: { guildId: string; dashboardUrl?: string },
  loader: LeaderboardLoader,
): Promise<void> {
  // Guard: Bot accepts guild-only commands only in DISCORD_GUILD_ID
  if (!interaction.guildId || interaction.guildId !== config.guildId) {
    await interaction.deferReply({ ephemeral: true }).catch(() => {});
    await interaction.editReply({
      content: "This command is only authorized for use within the configured server.",
      allowedMentions: { parse: [] },
    });
    return;
  }

  await interaction.deferReply();

  try {
    const rawPeriod = interaction.options.getString("period");
    const period: LeaderboardPeriod =
      rawPeriod === "today" || rawPeriod === "all" ? rawPeriod : "week";

    const rawMetric = interaction.options.getString("metric");
    const metric: LeaderboardMetric = rawMetric === "cost" ? "cost" : "tokens";

    const rawSubagents = interaction.options.getBoolean("subagents");
    const subagents = rawSubagents ?? true;

    const rawPage = interaction.options.getInteger("page");
    const page = Math.max(1, rawPage ?? 1);

    const rows = await loader(period);
    const embed = buildLeaderboardEmbed(
      rows,
      { period, metric, subagents, page },
      config.dashboardUrl,
    );

    await interaction.editReply({
      embeds: [embed],
      allowedMentions: { parse: [] },
    });
  } catch (err: unknown) {
    const code = extractSafeErrorCode(err);
    const hint = getDiagnosticHint(code);
    if (hint) {
      console.error(`Leaderboard command failed [${code}]: ${hint}`);
    } else {
      console.error(`Leaderboard command failed [${code}].`);
    }
    await interaction.editReply({
      content: "Unable to load leaderboard at this time.",
      embeds: [],
      allowedMentions: { parse: [] },
    });
  }
}
