import { Dashboard } from "@/components/Dashboard";
import { Nav } from "@/components/Nav";
import { getMe } from "@/lib/me";
import { byUser, lastDays, startOfUtcDay, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/server";
import { EVENT_COLUMNS, type DailyRow, type LiveSession, type Profile, type UsageEvent } from "@/lib/types";

import { NotMember } from "./NotMember";

export const dynamic = "force-dynamic";

export default async function Home() {
  const supabase = await createClient();
  const { profile: me } = await getMe(supabase);
  if (!me) return <NotMember />;

  const now = Date.now();
  const days = lastDays(30, now);
  const [profiles, week, today, allTime, feed, live, daily] = await Promise.all([
    supabase.from("profiles").select("id,handle,display_name,avatar_url").order("display_name"),
    supabase.rpc("leaderboard_since", { p_since: new Date(now - 7 * 86_400_000).toISOString() }),
    supabase.rpc("leaderboard_since", { p_since: startOfUtcDay(now) }),
    supabase.rpc("leaderboard_all_time"),
    supabase.from("usage_events").select(EVENT_COLUMNS).order("ts", { ascending: false }).limit(50),
    supabase.from("live_sessions").select("*").gte("last_seen", new Date(now - 90_000).toISOString()),
    supabase
      .from("usage_daily_all")
      .select("user_id,day,provider,model,is_subagent,call_count,total_tokens,cost_usd")
      .gte("day", days[0]),
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
        daily={(daily.data ?? []) as DailyRow[]}
        days={days}
        todayKey={utcDay(now)}
        serverNow={now}
      />
    </>
  );
}
