import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { InteractionEditReplyOptions } from "discord.js";
import { handleLeaderboardCommand, type LeaderboardInteraction } from "./command.ts";
import type { LeaderboardRow } from "./db.ts";
import { formatLeaderboard } from "./leaderboard.ts";

describe("formatLeaderboard ranking and formatting", () => {
  const sampleRows: LeaderboardRow[] = [
    {
      handle: "alice",
      display_name: "Alice A",
      total_tokens: 1_000_000n,
      subagent_tokens: 200_000n,
      cost_usd: "15.50",
    },
    {
      handle: "bob",
      display_name: "Bob B",
      total_tokens: 1_000_000n,
      subagent_tokens: 800_000n,
      cost_usd: "5.00",
    },
    {
      handle: "charlie",
      display_name: "Charlie C",
      total_tokens: 0n,
      subagent_tokens: 0n,
      cost_usd: "0",
    },
    {
      handle: "dave_zero",
      display_name: "Dave D",
      total_tokens: 0n,
      subagent_tokens: 0n,
      cost_usd: "0.00",
    },
    {
      handle: "zoe_high",
      display_name: "Zoe Z",
      total_tokens: 5_000_000n,
      subagent_tokens: 1_000_000n,
      cost_usd: "50.00",
    },
  ];

  it("ranks descending by tokens and includes subagents by default", () => {
    const result = formatLeaderboard(sampleRows, { metric: "tokens", subagents: true });
    assert.equal(result.rows[0].handle, "zoe_high");
    assert.equal(result.rows[0].rank, "1");
    assert.equal(result.rows[0].scoreDisplay, "5M");
    assert.equal(result.rows[0].subagentShareText, "20% subagent");

    // Alice and Bob tie on total tokens (1M), so Alice A comes before Bob B by display_name
    assert.equal(result.rows[1].handle, "alice");
    assert.equal(result.rows[1].rank, "2");
    assert.equal(result.rows[2].handle, "bob");
    assert.equal(result.rows[2].rank, "3");

    // Zero-score profiles get unranked dash "–"
    assert.equal(result.rows[3].rank, "–");
    assert.equal(result.rows[4].rank, "–");
  });

  it("main-only ranking reverses order of heavy subagent user vs organic user", () => {
    const users: LeaderboardRow[] = [
      {
        handle: "heavy_sub",
        display_name: "SubUser",
        total_tokens: 10_000_000n,
        subagent_tokens: 9_000_000n,
        cost_usd: "0",
      },
      {
        handle: "organic",
        display_name: "OrgUser",
        total_tokens: 2_000_000n,
        subagent_tokens: 0n,
        cost_usd: "0",
      },
    ];

    const withSub = formatLeaderboard(users, { metric: "tokens", subagents: true });
    assert.equal(withSub.rows[0].handle, "heavy_sub");
    assert.equal(withSub.rows[1].handle, "organic");

    const withoutSub = formatLeaderboard(users, { metric: "tokens", subagents: false });
    assert.equal(withoutSub.rows[0].handle, "organic");
    assert.equal(withoutSub.rows[1].handle, "heavy_sub");
  });

  it("sorts BigInt tokens accurately above Number.MAX_SAFE_INTEGER", () => {
    const largeRows: LeaderboardRow[] = [
      {
        handle: "large_a",
        display_name: "User A",
        total_tokens: 9_007_199_254_740_992n,
        subagent_tokens: 0n,
        cost_usd: "0",
      },
      {
        handle: "large_b",
        display_name: "User B",
        total_tokens: 9_007_199_254_740_993n,
        subagent_tokens: 0n,
        cost_usd: "0",
      },
    ];

    const sorted = formatLeaderboard(largeRows, { metric: "tokens" });
    assert.equal(sorted.rows[0].handle, "large_b");
    assert.equal(sorted.rows[1].handle, "large_a");
  });

  it("ranks by cost when metric is cost and does not alter cost for subagents toggle", () => {
    const resultWith = formatLeaderboard(sampleRows, { metric: "cost", subagents: true });
    const resultWithout = formatLeaderboard(sampleRows, { metric: "cost", subagents: false });

    assert.equal(resultWith.rows[0].handle, "zoe_high");
    assert.equal(resultWith.rows[0].scoreDisplay, "$50.00");
    assert.equal(resultWith.rows[1].handle, "alice");
    assert.equal(resultWith.rows[1].scoreDisplay, "$15.50");
    assert.equal(resultWith.rows[2].handle, "bob");
    assert.equal(resultWith.rows[2].scoreDisplay, "$5.00");

    // Without subagents: cost stays exactly identical
    assert.deepEqual(
      resultWith.rows.map((r) => r.scoreDisplay),
      resultWithout.rows.map((r) => r.scoreDisplay),
    );
  });

  it("escapes markdown special characters and collapses whitespace in display names", () => {
    const rows: LeaderboardRow[] = [
      {
        handle: "tricky_user",
        display_name: "*Alice*\n\n_Bold_  `Code`\r\n~Strike~ <@123>",
        total_tokens: 100n,
        subagent_tokens: 0n,
        cost_usd: "1.00",
      },
    ];
    const result = formatLeaderboard(rows);
    assert.ok(!result.rows[0].escapedDisplayName.includes("\n"));
    assert.ok(!result.rows[0].escapedDisplayName.includes("<@123>"));
    assert.ok(result.rows[0].escapedDisplayName.includes("\\*Alice\\*"));
  });

  it("paginates rows at 10 items per page with accurate rank offsets", () => {
    const twentyRows: LeaderboardRow[] = Array.from({ length: 25 }, (_, i) => ({
      handle: `user_${String(i).padStart(2, "0")}`,
      display_name: `User ${String(i).padStart(2, "0")}`,
      total_tokens: BigInt((30 - i) * 1000),
      subagent_tokens: 0n,
      cost_usd: "0",
    }));

    const page1 = formatLeaderboard(twentyRows, { page: 1 });
    assert.equal(page1.totalPages, 3);
    assert.equal(page1.rows.length, 10);
    assert.equal(page1.rows[0].handle, "user_00");
    assert.equal(page1.rows[0].rank, "1");
    assert.equal(page1.rows[9].handle, "user_09");
    assert.equal(page1.rows[9].rank, "10");

    const page2 = formatLeaderboard(twentyRows, { page: 2 });
    assert.equal(page2.rows.length, 10);
    assert.equal(page2.rows[0].handle, "user_10");
    assert.equal(page2.rows[0].rank, "11");
    assert.equal(page2.rows[9].handle, "user_19");
    assert.equal(page2.rows[9].rank, "20");

    const page3 = formatLeaderboard(twentyRows, { page: 3 });
    assert.equal(page3.rows.length, 5);
    assert.equal(page3.rows[0].handle, "user_20");
    assert.equal(page3.rows[0].rank, "21");

    const pageOutOfRange = formatLeaderboard(twentyRows, { page: 99 });
    assert.equal(pageOutOfRange.rows.length, 0);
    assert.ok(pageOutOfRange.renderedText.includes("No members on page 99"));
  });
});

describe("handleLeaderboardCommand execution seam", () => {
  it("rejects executions outside the configured guild with ephemeral denial", async () => {
    const state = {
      deferredEphemeral: false,
      replyPayload: null as InteractionEditReplyOptions | null,
      loaderCalled: false,
    };

    const mockInteraction: LeaderboardInteraction = {
      guildId: "unauthorized_guild",
      options: {
        getString: () => null,
        getBoolean: () => null,
        getInteger: () => null,
      },
      deferReply: async (options) => {
        state.deferredEphemeral = options?.ephemeral === true;
      },
      editReply: async (payload: InteractionEditReplyOptions) => {
        state.replyPayload = payload;
      },
    };

    await handleLeaderboardCommand(
      mockInteraction,
      { guildId: "target_guild" },
      async () => {
        state.loaderCalled = true;
        return [];
      },
    );

    assert.equal(state.loaderCalled, false);
    assert.equal(state.deferredEphemeral, true);
    assert.equal(state.replyPayload?.embeds, undefined);
    assert.ok(state.replyPayload?.content);
  });

  it("handles loader failure gracefully without leaking credentials", async () => {
    const loggedErrors: string[] = [];
    const originalConsoleError = console.error;
    console.error = (...args: unknown[]) => {
      loggedErrors.push(args.map(String).join(" "));
    };

    const state = {
      replyPayload: null as InteractionEditReplyOptions | null,
    };

    const mockInteraction: LeaderboardInteraction = {
      guildId: "target_guild",
      options: {
        getString: () => null,
        getBoolean: () => null,
        getInteger: () => null,
      },
      deferReply: async () => {},
      editReply: async (payload: InteractionEditReplyOptions) => {
        state.replyPayload = payload;
      },
    };

    try {
      await handleLeaderboardCommand(
        mockInteraction,
        { guildId: "target_guild" },
        async () => {
          const fakeErr = Object.assign(
            new Error("FATAL: connection to postgres://secret_admin:my_password@10.0.0.1:5432/db failed"),
            {
              code: "SELF_SIGNED_CERT_IN_CHAIN",
              query: "SELECT secret_token FROM users WHERE id = 1",
            },
          );
          throw fakeErr;
        },
      );
    } finally {
      console.error = originalConsoleError;
    }

    const allLogs = loggedErrors.join("\n");
    // Operator receives safe machine code
    assert.ok(allLogs.includes("SELF_SIGNED_CERT_IN_CHAIN"));
    // Operator log does not contain raw password, URL, or query
    assert.ok(!allLogs.includes("secret_admin"));
    assert.ok(!allLogs.includes("my_password"));
    assert.ok(!allLogs.includes("10.0.0.1"));
    assert.ok(!allLogs.includes("SELECT secret_token"));

    // User-facing reply remains generic without credentials or machine codes
    const replyStr = JSON.stringify(state.replyPayload);
    assert.ok(state.replyPayload?.content);
    assert.ok(!replyStr.includes("secret_admin"));
    assert.ok(!replyStr.includes("my_password"));
    assert.ok(!replyStr.includes("10.0.0.1"));
    assert.ok(!replyStr.includes("SELECT secret_token"));
    assert.ok(!replyStr.includes("SELF_SIGNED_CERT_IN_CHAIN"));
  });

  it("sanitizes unvalidated or malicious error codes to UNKNOWN", async () => {
    const loggedErrors: string[] = [];
    const originalConsoleError = console.error;
    console.error = (...args: unknown[]) => {
      loggedErrors.push(args.map(String).join(" "));
    };

    const state = {
      replyPayload: null as InteractionEditReplyOptions | null,
    };

    const mockInteraction: LeaderboardInteraction = {
      guildId: "target_guild",
      options: {
        getString: () => null,
        getBoolean: () => null,
        getInteger: () => null,
      },
      deferReply: async () => {},
      editReply: async (payload: InteractionEditReplyOptions) => {
        state.replyPayload = payload;
      },
    };

    try {
      await handleLeaderboardCommand(
        mockInteraction,
        { guildId: "target_guild" },
        async () => {
          const fakeErr = Object.assign(
            new Error("Generic DB failure"),
            {
              code: "malicious_code_or_leak_string",
              query: "SELECT super_secret FROM private_vault",
            },
          );
          throw fakeErr;
        },
      );
    } finally {
      console.error = originalConsoleError;
    }

    const allLogs = loggedErrors.join("\n");
    assert.ok(allLogs.includes("UNKNOWN"));
    assert.ok(!allLogs.includes("malicious_code_or_leak_string"));
    assert.ok(!allLogs.includes("SELECT super_secret"));

    const replyStr = JSON.stringify(state.replyPayload);
    assert.ok(state.replyPayload?.content);
    assert.ok(!replyStr.includes("malicious_code_or_leak_string"));
    assert.ok(!replyStr.includes("SELECT super_secret"));
  });
});

describe("quality-aware nullable accounting", () => {
  const unknown: LeaderboardRow = { handle: "unknown", display_name: "Unknown", total_tokens: null, subagent_tokens: null, cost_usd: null };
  const known: LeaderboardRow = { handle: "known", display_name: "Known", total_tokens: 120n, subagent_tokens: 20n, cost_usd: "1.25" };
  it("preserves unknown tokens and cost without ranking fake zeros", () => {
    for (const metric of ["tokens", "cost"] as const) {
      const result = formatLeaderboard([unknown, known], { metric });
      assert.equal(result.rows[0].handle, "known");
      assert.equal(result.rows[1].score, null);
      assert.equal(result.rows[1].scoreDisplay, "Unknown");
      assert.equal(result.rows[1].rank, "–");
      assert.match(result.groupTotalDisplay, /^Unknown/);
    }
  });
  it("does not invent main-only totals when subagent attribution is unavailable", () => {
    const result = formatLeaderboard([{ ...known, subagent_tokens: null }], { subagents: false });
    assert.equal(result.rows[0].score, null);
    assert.equal(result.rows[0].subagentShareText, null);
  });
});
