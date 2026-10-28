import { EmbedBuilder } from "discord.js";
import type { LeaderboardMetric, LeaderboardPeriod, LeaderboardRow } from "./db.ts";
import { escapeDiscordMarkdown, formatTokens, formatUsd } from "./format.ts";

export interface LeaderboardOptions {
  period?: LeaderboardPeriod;
  metric?: LeaderboardMetric;
  subagents?: boolean;
  page?: number;
}

export interface FormattedLeaderboardRow {
  handle: string;
  displayName: string;
  escapedDisplayName: string;
  rank: string;
  score: bigint | number | null;
  scoreDisplay: string;
  subagentShareText: string | null;
  line: string;
}

export interface FormattedLeaderboard {
  period: LeaderboardPeriod;
  periodLabel: string;
  metric: LeaderboardMetric;
  includeSubagents: boolean;
  page: number;
  totalPages: number;
  totalMembers: number;
  groupTotalDisplay: string;
  notes: string[];
  rows: FormattedLeaderboardRow[];
  renderedText: string;
}

const PAGE_SIZE = 10;

export function getPeriodLabel(period: LeaderboardPeriod): string {
  switch (period) {
    case "today":
      return "Today (UTC)";
    case "all":
      return "All Time";
    case "week":
    default:
      return "Past 7 Days";
  }
}

export function formatLeaderboard(
  rows: LeaderboardRow[],
  options: LeaderboardOptions = {},
): FormattedLeaderboard {
  const period: LeaderboardPeriod = options.period ?? "week";
  const metric: LeaderboardMetric = options.metric ?? "tokens";
  const includeSubagents = options.subagents ?? true;
  const requestedPage = Math.max(1, options.page ?? 1);

  // Compute scores and display names for sorting
  type InternalEntry = {
    handle: string;
    displayName: string;
    score: bigint | number | null;
    costScore: number | null;
    tokenScore: bigint | null;
    subagentTokens: bigint | null;
    totalTokens: bigint | null;
    isZero: boolean;
  };

  const entries: InternalEntry[] = rows.map((r) => {
    const rawName = r.display_name?.trim();
    const displayName = rawName && rawName.length > 0 ? rawName : r.handle;

    const cost = r.cost_usd === null ? null : Number(r.cost_usd);
    const total = r.total_tokens === null ? null : BigInt(r.total_tokens);
    const sub = r.subagent_tokens === null ? null : BigInt(r.subagent_tokens);
    const effectiveTokens = includeSubagents ? total : total === null || sub === null ? null : total - sub;

    if (metric === "cost") {
      return {
        handle: r.handle,
        displayName,
        score: cost,
        costScore: cost,
        tokenScore: 0n,
        subagentTokens: 0n,
        totalTokens: 0n,
        isZero: cost === 0,
      };
    }

    return {
      handle: r.handle,
      displayName,
      score: effectiveTokens,
      costScore: 0,
      tokenScore: effectiveTokens,
      subagentTokens: sub,
      totalTokens: total,
      isZero: effectiveTokens === 0n,
    };
  });

  // Sort: Descending score then display_name.localeCompare
  entries.sort((a, b) => {
    if (a.score === null || b.score === null) {
      return a.score === b.score ? a.displayName.localeCompare(b.displayName) : a.score === null ? 1 : -1;
    }
    if (metric === "cost") {
      const diff = (b.score as number) - (a.score as number);
      if (diff !== 0) return diff;
    } else {
      const diff = (b.score as bigint) - (a.score as bigint);
      if (diff !== 0n) return diff > 0n ? 1 : -1;
    }
    return a.displayName.localeCompare(b.displayName);
  });

  const totalMembers = entries.length;
  const totalPages = Math.max(1, Math.ceil(totalMembers / PAGE_SIZE));
  const page = requestedPage;

  // Calculate group total across all entries
  let groupTotalDisplay: string;
  if (metric === "cost") {
    const totalCost = entries.reduce<number | null>((sum, e) => sum === null || e.costScore === null ? null : sum + e.costScore, 0);
    groupTotalDisplay = totalCost === null ? "Unknown cost" : formatUsd(totalCost);
  } else {
    const totalTokens = entries.reduce<bigint | null>((sum, e) => sum === null || e.tokenScore === null ? null : sum + e.tokenScore, 0n);
    groupTotalDisplay = totalTokens === null ? "Unknown tokens" : `${formatTokens(totalTokens)} tokens`;
  }

  // Notes
  const notes: string[] = [];
  if (metric === "cost") {
    notes.push("Cost totals include subagents and use reported or imported USD estimates.");
  } else if (!includeSubagents) {
    notes.push("Main session tokens only (subagent tokens excluded).");
  }
  if (entries.some(e => e.score === null)) notes.push("Unavailable totals are unknown, not zero, and are not ranked.");
  notes.push("Collector totals can include reconstructed usage; costs are not invoices.");

  // Slice page entries first, then format only the requested page
  const startIndex = (page - 1) * PAGE_SIZE;
  const pageEntries = entries.slice(startIndex, startIndex + PAGE_SIZE);

  const pageRows: FormattedLeaderboardRow[] = pageEntries.map((entry, idx) => {
    const rank = entry.isZero || entry.score === null ? "–" : String(startIndex + idx + 1);
    const truncatedName = entry.displayName.length > 40 ? entry.displayName.slice(0, 39) + "…" : entry.displayName;
    const escapedDisplayName = escapeDiscordMarkdown(truncatedName);

    let scoreDisplay: string;
    let subagentShareText: string | null = null;

    if (metric === "cost") {
      scoreDisplay = entry.costScore === null ? "Unknown" : formatUsd(entry.costScore);
    } else {
      scoreDisplay = entry.tokenScore === null ? "Unknown" : formatTokens(entry.tokenScore);
      if (includeSubagents && entry.subagentTokens !== null && entry.totalTokens !== null && entry.subagentTokens > 0n && entry.totalTokens > 0n) {
        const pct = Math.round((Number(entry.subagentTokens) / Number(entry.totalTokens)) * 100);
        if (pct > 0) {
          subagentShareText = `${pct}% subagent`;
        }
      }
    }

    const rankPrefix = rank === "–" ? "**–**" : `**${rank}.**`;
    const detailPart = subagentShareText ? ` *(${subagentShareText})*` : "";
    const unitPart = metric === "tokens" ? " tokens" : "";
    const line = `${rankPrefix} **${escapedDisplayName}** — ${scoreDisplay}${unitPart}${detailPart}`;

    return {
      handle: entry.handle,
      displayName: entry.displayName,
      escapedDisplayName,
      rank,
      score: entry.score,
      scoreDisplay,
      subagentShareText,
      line,
    };
  });
  let renderedText: string;
  if (totalMembers === 0) {
    renderedText = "*No members found.*";
  } else if (pageRows.length === 0) {
    renderedText = `*(No members on page ${page})*`;
  } else {
    renderedText = pageRows.map((r) => r.line).join("\n");
  }

  return {
    period,
    periodLabel: getPeriodLabel(period),
    metric,
    includeSubagents,
    page,
    totalPages,
    totalMembers,
    groupTotalDisplay,
    notes,
    rows: pageRows,
    renderedText,
  };
}

export function buildLeaderboardEmbed(
  rows: LeaderboardRow[],
  options: LeaderboardOptions = {},
  dashboardUrl?: string,
): EmbedBuilder {
  const formatted = formatLeaderboard(rows, options);

  const title = `Leaderboard • ${formatted.periodLabel}`;
  const header = `**${formatted.groupTotalDisplay}** across the group (${formatted.periodLabel.toLowerCase()})\n\n`;

  let description = `${header}${formatted.renderedText}`;
  if (formatted.notes.length > 0) {
    description += `\n\n*${formatted.notes.join(" ")}*`;
  }

  // Discord description hard limit is 4096
  if (description.length > 4000) {
    description = description.slice(0, 3996) + "…";
  }

  const embed = new EmbedBuilder()
    .setTitle(title)
    .setDescription(description)
    .setColor(0x5865f2)
    .setFooter({
      text: `Page ${formatted.page} of ${formatted.totalPages} • ${formatted.totalMembers} member${formatted.totalMembers === 1 ? "" : "s"}`,
    })
    .setTimestamp();

  if (dashboardUrl) {
    embed.setURL(dashboardUrl);
  }

  return embed;
}
