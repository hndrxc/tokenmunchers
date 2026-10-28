import { readFileSync } from "node:fs";
import { rootCertificates } from "node:tls";
import postgres from "postgres";

const supabaseCa = readFileSync(
  new URL("../certs/supabase-prod-ca.crt", import.meta.url),
  "utf8",
);

export type LeaderboardPeriod = "today" | "week" | "all";
export type LeaderboardMetric = "tokens" | "cost";

export interface LeaderboardRow {
  handle: string;
  display_name: string | null;
  total_tokens: number | string | bigint | null;
  subagent_tokens: number | string | bigint | null;
  cost_usd: number | string | null;
}

export type LeaderboardLoader = (period: LeaderboardPeriod) => Promise<LeaderboardRow[]>;

export interface DatabaseAdapter {
  fetchLeaderboard: LeaderboardLoader;
  close: () => Promise<void>;
}

export function createDatabase(connectionString: string): DatabaseAdapter {
  const sql = postgres(connectionString, {
    max: 5,
    idle_timeout: 20,
    connect_timeout: 10,
    ssl: {
      ca: [...rootCertificates, supabaseCa],
      rejectUnauthorized: true,
    },
    connection: {
      default_transaction_read_only: true,
    },
    prepare: true,
  });

  return {
    async fetchLeaderboard(period: LeaderboardPeriod): Promise<LeaderboardRow[]> {
      const validPeriod: LeaderboardPeriod =
        period === "today" || period === "all" ? period : "week";

      const rows = await sql<LeaderboardRow[]>`
        SELECT handle, display_name, total_tokens, subagent_tokens, cost_usd
        FROM public.discord_leaderboard(${validPeriod});
      `;
      return rows;
    },
    async close(): Promise<void> {
      await sql.end({ timeout: 5 });
    },
  };
}
