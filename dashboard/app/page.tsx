import type { SupabaseClient } from "@supabase/supabase-js";

import { Dashboard } from "@/components/Dashboard";
import { Nav } from "@/components/Nav";
import { getMe } from "@/lib/me";
import { byUser, lastDays, startOfUtcDay, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/server";
import { EVENT_COLUMNS, type DailyRow, type LiveSession, type Profile, type UsageEvent } from "@/lib/types";

import { NotMember } from "./NotMember";

export const dynamic = "force-dynamic";

const DAILY_PAGE = 1000;

/**
 * Full per-user/day rollup history before today, paged past the standard
 * PostgREST 1000-row response cap under a deterministic grouping-key order.
 * Today's bucket is intentionally excluded: live totals are exact, while the
 * rollup view lags up to 10 minutes.
 */
async function loadDailyHistory(supabase: SupabaseClient, todayKey: string): Promise<DailyRow[]> {
  const rows: DailyRow[] = [];
  for (let offset = 0; ; offset += DAILY_PAGE) {
    const { data, error } = await supabase
      .from("usage_daily_all")
      .select("user_id,day,provider,model,is_subagent,call_count,total_tokens,cost_usd")
      .lt("day", todayKey)
      .order("day")
      .order("user_id")
      .order("provider")
      .order("model")
      .order("is_subagent")
      .range(offset, offset + DAILY_PAGE - 1);
    if (error) throw new Error(`Failed to load daily usage history: ${error.message}`);
    const page = (data ?? []) as DailyRow[];
    rows.push(...page);
    if (page.length < DAILY_PAGE) return rows;
  }
}

export default async function Home() {
  const supabase = await createClient();
  const { profile: me } = await getMe(supabase);
  if (!me) return <NotMember />;

  const now = Date.now();
  const days = lastDays(30, now);
  const todayKey = utcDay(now);
  const [profiles, week, today, allTime, feed, live, daily] = await Promise.all([
    supabase.from("profiles").select("id,handle,display_name,avatar_url").order("display_name"),
    supabase.rpc("leaderboard_since", { p_since: `${days.at(-7)!}T00:00:00.000Z` }),
    supabase.rpc("leaderboard_since", { p_since: startOfUtcDay(now) }),
    supabase.rpc("leaderboard_all_time"),
    supabase.from("usage_events").select(EVENT_COLUMNS).order("ts", { ascending: false }).limit(50),
    supabase.from("live_sessions").select("*").gte("last_seen", new Date(now - 90_000).toISOString()),
    loadDailyHistory(supabase, todayKey),
  ]);

  return (
    <>
      <Nav handle={me.handle} />
      <Dashboard
        profiles={(profiles.data ?? []) as Profile[]}
        week={byUser(week.data)}
        today={byUser(today.data)}
        allTime={byUser(allTime.data)}
        feed={(feed.data ?? []) as UsageEvent[]}
        live={(live.data ?? []) as LiveSession[]}
        daily={daily}
        days={days}
        todayKey={todayKey}
        serverNow={now}
      />
    </>
  );
}
