import { notFound } from "next/navigation";

import { ActivitySourceBadge, ActivityTagControls } from "@/components/ActivityTags";
import { Avatar } from "@/components/Avatar";
import { DailyBars } from "@/components/DailyBars";
import { LiveRefresh } from "@/components/LiveRefresh";
import { ModelBars, type ModelShare } from "@/components/ModelBars";
import { Nav } from "@/components/Nav";
import type { ActivityCall } from "@/lib/activity";
import { ago, model, spendNote, tokens, usd } from "@/lib/format";
import { getMe } from "@/lib/me";
import { addNullable, combineSpend, emptySpend, lastDays, LIVE_WINDOW_MS, normalizeSpend, startOfUtcDay, streaks, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/server";
import type { DailyRow, Totals } from "@/lib/types";

import { NotMember } from "../../NotMember";

export const dynamic = "force-dynamic";

export default async function ProfilePage({ params }: { params: Promise<{ handle: string }> }) {
  const { handle } = await params;
  const supabase = await createClient();
  const { profile: me } = await getMe(supabase);
  if (!me) return <NotMember />;

  const { data: profile } = await supabase
    .from("profiles")
    .select("id,handle,display_name,avatar_url,created_at")
    .eq("handle", handle)
    .maybeSingle();
  if (!profile) notFound();

  const now = Date.now();
  const days = lastDays(30, now);
  const todayKey = utcDay(now);
  const [dailyRes, weekRes, todayRes, allRes, activityRes, liveRes] = await Promise.all([
    supabase
      .from("usage_daily_all")
      .select("user_id,day,provider,model,is_subagent,call_count,total_tokens,cost_usd,known_cost_usd,unpriced_tokens,has_unknown_cost,has_estimated_cost,has_reconstructed")
      .eq("user_id", profile.id),
    supabase.rpc("leaderboard_since", { p_since: new Date(now - 7 * 86_400_000).toISOString() }),
    supabase.rpc("leaderboard_since", { p_since: startOfUtcDay(now) }),
    supabase.rpc("leaderboard_all_time"),
    supabase.rpc("recent_activity", { p_user_id: profile.id }),
    supabase.from("live_sessions").select("last_seen,model").eq("user_id", profile.id).order("last_seen", { ascending: false }).limit(1),
  ]);

  const pick = (rows: Totals[] | null) => rows?.find((r) => r.user_id === profile.id);
  const week = pick(weekRes.data);
  const today = pick(todayRes.data);
  const all = pick(allRes.data);
  const daily = (dailyRes.data ?? []) as DailyRow[];
  const calls = (activityRes.data ?? []) as ActivityCall[];
  const live = liveRes.data?.[0];
  const isLive = !!live && now - Date.parse(live.last_seen) < LIVE_WINDOW_MS;

  // Past days from rollups; today from exact raw totals.
  const perDay: Record<string, number | null> = {};
  for (const r of daily) if (r.day < todayKey) perDay[r.day] = addNullable(Object.hasOwn(perDay, r.day) ? perDay[r.day] : 0, r.total_tokens);
  if (today) perDay[todayKey] = today.total_tokens;
  const activeDays = [...new Set(daily.filter(r => r.total_tokens === null || r.total_tokens > 0).map(r => r.day))];
  const { current, longest } = streaks(activeDays, now);

  const mix = new Map<string, ModelShare>();
  for (const r of daily) {
    if (r.day < days[0]) continue;
    const key = `${r.provider}/${r.model}`;
    const m = mix.get(key) ?? { ...emptySpend(), provider: r.provider, model: r.model, total_tokens: 0, calls: 0 };
    mix.set(key, {
      ...m,
      ...combineSpend(m, normalizeSpend(r)),
      total_tokens: addNullable(m.total_tokens, r.total_tokens),
      calls: addNullable(m.calls, r.call_count),
    });
  }
  const mixTotal = [...mix.values()].reduce<number | null>((sum, row) => addNullable(sum, row.total_tokens), 0);
  const mixRows = [...mix.values()].sort((a, b) => (b.total_tokens ?? -1) - (a.total_tokens ?? -1)).slice(0, 8);
  const incompleteDays = Object.values(perDay).some(v => v === null);
  const bestDay = Object.entries(perDay).filter((entry): entry is [string, number] => entry[1] !== null).sort((a, b) => b[1] - a[1])[0];

  return (
    <>
      <Nav handle={me.handle} />
      <LiveRefresh userId={profile.id} />
      <div className="profile-head">
        <Avatar profile={profile} size={56} />
        <div>
          <h1>
            {profile.display_name} {isLive && <span className="live-dot pulse" aria-label="live now" />}
          </h1>
          <div className="sub">
            @{profile.handle}
            {isLive && live?.model ? ` · coding now on ${model(live.model)}` : ""}
          </div>
        </div>
      </div>

      <div className="stack">
        <section className="card tiles" aria-label="Stats">
          <Tile label="This week" value={tokens(week ? week.total_tokens : 0)} hint={usageHint(week)} />
          <Tile label="All time" value={tokens(all ? all.total_tokens : 0)} hint={usageHint(all)} />
          <Tile label="Current streak" value={`${current} ${current === 1 ? "day" : "days"}`} hint={`Longest: ${longest} ${longest === 1 ? "day" : "days"}`} />
          <Tile
            label="Biggest day"
            value={incompleteDays ? "Unknown" : bestDay ? tokens(bestDay[1]) : "–"}
            hint={incompleteDays ? "Some daily totals are unavailable" : bestDay ? new Date(`${bestDay[0]}T00:00:00Z`).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" }) : "no usage yet"}
          />
        </section>

        <div className="grid">
          <section className="card" aria-labelledby="pd-h">
            <div className="card-head">
              <h2 id="pd-h">Tokens per day</h2>
              <span className="sub">last 30 days, UTC</span>
            </div>
            <DailyBars points={days.map((day) => ({ day, value: Object.hasOwn(perDay, day) ? perDay[day] : 0 }))} />
          </section>
          <section className="card" aria-labelledby="mm-h">
            <div className="card-head">
              <h2 id="mm-h">Model mix</h2>
              <span className="sub">last 30 days</span>
            </div>
            <ModelBars rows={mixRows} totalTokens={mixTotal} />
            {daily.some(r => r.has_estimated_cost) && <p className="sub">Costs include estimates, not invoices.</p>}
            {daily.some(r => r.has_reconstructed) && <p className="sub">Includes reconstructed usage; unavailable counts remain unknown.</p>}
          </section>
        </div>

        <section className="card" aria-labelledby="rc-h">
          <div className="card-head">
            <h2 id="rc-h">Recent calls</h2>
          </div>
          <ActivityTagControls />
          {calls.length === 0 ? (
            <p className="empty">No recent calls.</p>
          ) : (
            <ul className="feed">
              {calls.map((e) => (
                <li key={e.id}>
                  <span className="line">
                    {tokens(e.total_tokens)} tokens on {model(e.model)} <span className="sub">{e.provider}</span>
                    <ActivitySourceBadge source={e.source} />
                    {e.is_subagent && <>{" "}<span className="badge">subagent</span></>}
                  </span>
                  <span className="meta">
                    {tokens(e.input_tokens)} in · {tokens(e.output_tokens)} out · {tokens(e.cache_read_tokens)} cached · {usd(e.cost_usd === null ? null : Number(e.cost_usd))} ·{" "}
                    {ago(e.ts, now)}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </>
  );
}

function usageHint(total: Totals | undefined): string {
  const spend = total ? normalizeSpend(total) : emptySpend();
  const knownCost = total ? spend.known_cost_usd : 0;
  const calls = total ? total.call_count?.toLocaleString("en-US") ?? "Unknown" : "0";
  return [
    `${calls} calls`,
    knownCost === null ? "Unknown" : `${usd(knownCost)} known spend`,
    spendNote(spend),
    total?.has_reconstructed ? "reconstructed usage" : "",
  ].filter(Boolean).join(" · ");
}

function Tile({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="tile">
      <div className="label">{label}</div>
      <div className="big">{value}</div>
      <div className="hint">{hint}</div>
    </div>
  );
}
