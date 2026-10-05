import { notFound } from "next/navigation";

import { Avatar } from "@/components/Avatar";
import { DailyBars } from "@/components/DailyBars";
import { LiveRefresh } from "@/components/LiveRefresh";
import { ModelBars, type ModelShare } from "@/components/ModelBars";
import { Nav } from "@/components/Nav";
import { ago, model, tokens, usd } from "@/lib/format";
import { getMe } from "@/lib/me";
import { lastDays, LIVE_WINDOW_MS, startOfUtcDay, streaks, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/server";
import { EVENT_COLUMNS, type DailyRow, type Totals, type UsageEvent } from "@/lib/types";

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
  const [dailyRes, weekRes, todayRes, allRes, eventsRes, liveRes] = await Promise.all([
    supabase
      .from("usage_daily")
      .select("user_id,day,provider,model,is_subagent,call_count,total_tokens,cost_usd")
      .eq("user_id", profile.id),
    supabase.rpc("leaderboard_since", { p_since: new Date(now - 7 * 86_400_000).toISOString() }),
    supabase.rpc("leaderboard_since", { p_since: startOfUtcDay(now) }),
    supabase.rpc("leaderboard_all_time"),
    supabase.from("usage_events").select(EVENT_COLUMNS).eq("user_id", profile.id).order("ts", { ascending: false }).limit(25),
    supabase.from("live_sessions").select("last_seen,model").eq("user_id", profile.id).order("last_seen", { ascending: false }).limit(1),
  ]);

  const pick = (rows: Totals[] | null) => rows?.find((r) => r.user_id === profile.id);
  const week = pick(weekRes.data);
  const today = pick(todayRes.data);
  const all = pick(allRes.data);
  const daily = (dailyRes.data ?? []) as DailyRow[];
  const events = (eventsRes.data ?? []) as UsageEvent[];
  const live = liveRes.data?.[0];
  const isLive = !!live && now - Date.parse(live.last_seen) < LIVE_WINDOW_MS;

  // Past days from rollups; today from exact raw totals.
  const perDay: Record<string, number> = {};
  for (const r of daily) if (r.day < todayKey) perDay[r.day] = (perDay[r.day] ?? 0) + Number(r.total_tokens);
  if (today) perDay[todayKey] = today.total_tokens;
  const activeDays = Object.entries(perDay).filter(([, v]) => v > 0).map(([d]) => d);
  const { current, longest } = streaks(activeDays, now);

  const mix = new Map<string, ModelShare>();
  for (const r of daily) {
    if (r.day < days[0]) continue;
    const key = `${r.provider}/${r.model}`;
    const m = mix.get(key) ?? { provider: r.provider, model: r.model, total_tokens: 0, cost_usd: 0, calls: 0 };
    m.total_tokens += Number(r.total_tokens);
    m.cost_usd += Number(r.cost_usd);
    m.calls += Number(r.call_count);
    mix.set(key, m);
  }
  const mixRows = [...mix.values()].sort((a, b) => b.total_tokens - a.total_tokens).slice(0, 8);
  const bestDay = Object.entries(perDay).sort((a, b) => b[1] - a[1])[0];

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
          <Tile label="This week" value={tokens(week?.total_tokens ?? 0)} hint={`${(week?.call_count ?? 0).toLocaleString("en-US")} calls · ${usd(week?.cost_usd ?? 0)}`} />
          <Tile label="All time" value={tokens(all?.total_tokens ?? 0)} hint={`${(all?.call_count ?? 0).toLocaleString("en-US")} calls · ${usd(Number(all?.cost_usd ?? 0))}`} />
          <Tile label="Current streak" value={`${current} ${current === 1 ? "day" : "days"}`} hint={`Longest: ${longest} ${longest === 1 ? "day" : "days"}`} />
          <Tile
            label="Biggest day"
            value={bestDay ? tokens(bestDay[1]) : "–"}
            hint={bestDay ? new Date(`${bestDay[0]}T00:00:00Z`).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" }) : "no usage yet"}
          />
        </section>

        <div className="grid">
          <section className="card" aria-labelledby="pd-h">
            <div className="card-head">
              <h2 id="pd-h">Tokens per day</h2>
              <span className="sub">last 30 days, UTC</span>
            </div>
            <DailyBars points={days.map((day) => ({ day, value: perDay[day] ?? 0 }))} />
          </section>
          <section className="card" aria-labelledby="mm-h">
            <div className="card-head">
              <h2 id="mm-h">Model mix</h2>
              <span className="sub">last 30 days</span>
            </div>
            <ModelBars rows={mixRows} />
          </section>
        </div>

        <section className="card" aria-labelledby="rc-h">
          <div className="card-head">
            <h2 id="rc-h">Recent calls</h2>
          </div>
          {events.length === 0 ? (
            <p className="empty">No calls in the last 30 days.</p>
          ) : (
            <ul className="feed">
              {events.map((e) => (
                <li key={e.event_id}>
                  <span className="line">
                    {tokens(e.total_tokens)} tokens on {model(e.model)} <span className="sub">{e.provider}</span>
                    {e.is_subagent && <span className="badge">subagent</span>}
                  </span>
                  <span className="meta">
                    {tokens(e.input_tokens)} in · {tokens(e.output_tokens)} out · {tokens(e.cache_read_tokens)} cached · {usd(Number(e.cost_usd))} ·{" "}
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

function Tile({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="tile">
      <div className="label">{label}</div>
      <div className="big">{value}</div>
      <div className="hint">{hint}</div>
    </div>
  );
}
