"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { ActivitySourceBadge, ActivityTagControls } from "@/components/ActivityTags";
import { Avatar } from "@/components/Avatar";
import { DailyBars } from "@/components/DailyBars";
import { Sparkline } from "@/components/Sparkline";
import { activityFromIngest, mergeActivity, type ActivityCall } from "@/lib/activity";
import { ago, duration, model, spendNote, tokens, usd } from "@/lib/format";
import { addEvent, addNullable, combineSpend, dailyByUser, emptySpend, LIVE_WINDOW_MS, metricOf, normalizeEvent, tokenAttributionNote, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/client";
import { subscribeAuthed } from "@/lib/supabase/realtime";
import type { DailyRow, LiveSession, Profile, Totals } from "@/lib/types";

type Period = "today" | "week" | "all";
type Metric = "tokens" | "cost";

export interface DashboardProps {
  profiles: Profile[];
  week: Record<string, Totals>;
  today: Record<string, Totals>;
  allTime: Record<string, Totals>;
  feed: ActivityCall[];
  live: LiveSession[];
  daily: DailyRow[];
  days: string[];
  todayKey: string;
  serverNow: number;
}

type OptimisticActivity = { serverNow: number; calls: ActivityCall[] };

export function Dashboard(props: DashboardProps) {
  const router = useRouter();
  const [loadedProfiles, setLoadedProfiles] = useState<Record<string, Profile>>({});
  const profiles = useMemo(
    () => ({ ...loadedProfiles, ...Object.fromEntries(props.profiles.map((p) => [p.id, p])) }),
    [loadedProfiles, props.profiles],
  );
  const [week, setWeek] = useState(props.week);
  const [today, setToday] = useState(props.today);
  const [allTime, setAllTime] = useState(props.allTime);
  const [optimisticActivity, setOptimisticActivity] = useState<OptimisticActivity>({ serverNow: props.serverNow, calls: [] });
  const activitySnapshot = useRef({ serverNow: props.serverNow, feed: props.feed });
  activitySnapshot.current = { serverNow: props.serverNow, feed: props.feed };
  const recentIngest = useRef(new Set<string>());
  // A refreshed snapshot replaces the feed; old optimistic calls must not refill it.
  const realtimeCalls = optimisticActivity.serverNow === props.serverNow ? optimisticActivity.calls : null;
  const feed = useMemo(() => mergeActivity(props.feed, realtimeCalls ?? []), [props.feed, realtimeCalls]);
  const fresh = useMemo(() => new Set(realtimeCalls?.map((call) => call.id)), [realtimeCalls]);
  const [live, setLive] = useState<Record<string, LiveSession>>(() =>
    Object.fromEntries(props.live.map((s) => [`${s.user_id}:${s.session_id}`, s])),
  );
  const [now, setNow] = useState(props.serverNow);
  const [status, setStatus] = useState<string>("connecting");

  const [period, setPeriod] = useState<Period>("week");
  const [metric, setMetric] = useState<Metric>("tokens");
  const [includeSubagents, setIncludeSubagents] = useState(true);

  const profilesRef = useRef(profiles);
  profilesRef.current = profiles;
  // First UTC day of the rolling 7-day leaderboard window; realtime inserts older
  // than this must not nudge the week totals (matches the server cutoff).
  const weekCutoff = props.days[props.days.length - 7] ?? "";

  // Clock for relative times and presence expiry; reload at UTC midnight so
  // "today" and the 30-day window re-base.
  useEffect(() => {
    const t = setInterval(() => {
      const n = Date.now();
      setNow(n);
      if (n - props.serverNow > 30_000) router.refresh();
      if (utcDay(n) !== props.todayKey) router.refresh();
    }, 5000);
    setNow(Date.now());
    return () => clearInterval(t);
  }, [props.todayKey, props.serverNow, router]);

  useEffect(() => {
    setWeek(props.week);
    setToday(props.today);
    setAllTime(props.allTime);
  }, [props.week, props.today, props.allTime]);

  const ensureProfile = useCallback(async (userId: string) => {
    if (profilesRef.current[userId]) return;
    const { data } = await createClient().from("profiles").select("id,handle,display_name,avatar_url").eq("id", userId).maybeSingle();
    if (data) setLoadedProfiles((p) => ({ ...p, [data.id]: data }));
  }, []);

  useEffect(() => {
    return subscribeAuthed(createClient(), (supabase) =>
      supabase
        .channel("tokenmunchers-live")
        .on("postgres_changes", { event: "INSERT", schema: "public", table: "usage_events" }, (payload) => {
          const e = normalizeEvent(payload.new);
          const call = activityFromIngest(e);
          const { serverNow, feed } = activitySnapshot.current;
          const recent = recentIngest.current;
          if (recent.has(call.id) || feed.some((row) => row.id === call.id)) return;
          recent.add(call.id);
          const oldest = recent.values().next().value;
          if (recent.size > 50 && oldest !== undefined) recent.delete(oldest);
          void ensureProfile(e.user_id);
          const day = utcDay(Date.parse(e.ts));
          if (day >= weekCutoff) setWeek((m) => addEvent(m, e));
          setAllTime((m) => addEvent(m, e));
          if (day === props.todayKey) setToday((m) => addEvent(m, e));
          setOptimisticActivity((previous) => ({
            serverNow,
            calls: mergeActivity(previous.serverNow === serverNow ? previous.calls : [], [call]),
          }));
          setNow(Date.now());
        })
        .on("postgres_changes", { event: "*", schema: "public", table: "live_sessions" }, (payload) => {
          if (payload.eventType === "DELETE") {
            const old = payload.old as Partial<LiveSession>;
            setLive((m) => {
              const next = { ...m };
              delete next[`${old.user_id}:${old.session_id}`];
              return next;
            });
            return;
          }
          const s = payload.new as LiveSession;
          void ensureProfile(s.user_id);
          setLive((m) => ({ ...m, [`${s.user_id}:${s.session_id}`]: s }));
          setNow(Date.now());
        })
        // SUBSCRIBED only means the channel joined; inserts flow once Postgres
        // changes confirms, so that's when we show "connected".
        .on("system", {}, (msg: { extension?: string; status?: string }) => {
          if (msg.extension === "postgres_changes") setStatus(msg.status === "ok" ? "live" : "realtime error");
        })
        .subscribe((s) => {
          if (s !== "SUBSCRIBED") setStatus(s.toLowerCase());
        }),
    );
  }, [ensureProfile, props.todayKey, weekCutoff]);

  // ---- derived ----------------------------------------------------------------

  const liveUsers = useMemo(() => {
    const byUser: Record<string, { sessions: LiveSession[] }> = {};
    for (const s of Object.values(live)) {
      if (now - Date.parse(s.last_seen) > LIVE_WINDOW_MS) continue;
      (byUser[s.user_id] ??= { sessions: [] }).sessions.push(s);
    }
    return Object.entries(byUser)
      .map(([userId, { sessions }]) => {
        sessions.sort((a, b) => Date.parse(b.last_seen) - Date.parse(a.last_seen));
        const earliest = sessions.reduce((a, s) => (s.started_at < a ? s.started_at : a), sessions[0].started_at);
        return { userId, latest: sessions[0], startedAt: earliest, count: sessions.length };
      })
      .sort((a, b) => a.startedAt.localeCompare(b.startedAt));
  }, [live, now]);
  const liveSet = useMemo(() => new Set(liveUsers.map((u) => u.userId)), [liveUsers]);

  const totals = period === "today" ? today : period === "week" ? week : allTime;
  const daily = useMemo(
    () => dailyByUser(props.daily, today, includeSubagents, props.todayKey),
    [props.daily, today, includeSubagents, props.todayKey],
  );

  const rows = useMemo(() => {
    return Object.values(profiles)
      .map((profile) => {
        const t = totals[profile.id];
        return {
          profile,
          t,
          value: metricOf(t, metric, includeSubagents),
          note: metric === "cost" && t ? spendNote(t) : "",
          attribution: metric === "tokens" ? tokenAttributionNote(t, includeSubagents) : null,
        };
      })
      .sort((a, b) => (a.value === null ? b.value === null ? 0 : 1 : b.value === null ? -1 : b.value - a.value) || a.profile.display_name.localeCompare(b.profile.display_name));
  }, [profiles, totals, metric, includeSubagents]);

  const groupDaily = useMemo(
    () => props.days.map((day) => ({ day, value: Object.values(daily).reduce<number | null>((s, u) => addNullable(s, Object.hasOwn(u, day) ? u[day] : 0), 0) })),
    [daily, props.days],
  );
  const weekDays = useMemo(() => props.days.slice(-7), [props.days]);
  const allDays = useMemo(() => {
    let earliest: string | null = null;
    for (const r of props.daily) {
      if (!earliest || r.day < earliest) earliest = r.day;
    }
    if (!earliest || earliest > props.todayKey) return [props.todayKey];
    const end = Date.parse(`${props.todayKey}T00:00:00.000Z`);
    const days: string[] = [];
    for (let t = Date.parse(`${earliest}T00:00:00.000Z`); t <= end; t += 86_400_000) {
      days.push(new Date(t).toISOString().slice(0, 10));
    }
    return days;
  }, [props.daily, props.todayKey]);

  const periodLabel = period === "today" ? "today (UTC)" : period === "week" ? "last 7 days" : "all time";
  const groupTokens = metric === "tokens" ? rows.reduce<number | null>((s, r) => addNullable(s, r.value), 0) : 0;
  const groupSpend = metric === "cost" ? Object.values(totals).reduce(combineSpend, emptySpend()) : emptySpend();
  const groupCost = Object.keys(totals).length === 0 ? 0 : groupSpend.known_cost_usd;
  const groupNote = metric === "cost" ? spendNote(groupSpend) : "";

  return (
    <div className="stack">
      <section className="card" aria-labelledby="live-h">
        <div className="card-head">
          <h2 id="live-h">Live now</h2>
          <span className="sub" title="Realtime connection">
            {status === "live" ? (
              <>
                <span className="live-dot" /> connected
              </>
            ) : status === "subscribed" || status === "connecting" ? (
              "connecting…"
            ) : (
              status
            )}
          </span>
        </div>
        {liveUsers.length === 0 ? (
          <p className="empty">Nobody is coding right now.</p>
        ) : (
          <div className="live-strip">
            {liveUsers.map(({ userId, latest, startedAt, count }) => {
              const p = profiles[userId];
              return (
                <Link className="live-chip" key={userId} href={p ? `/u/${p.handle}` : "#"}>
                  <Avatar profile={p} size={32} />
                  <span>
                    <span className="who">
                      {p?.display_name ?? "…"} <span className="live-dot pulse" aria-label="live" />
                    </span>
                    <br />
                    <span className="what">
                      {latest.model ? model(latest.model) : "coding"} · {duration(startedAt, now)}
                      {count > 1 ? ` · ${count} sessions` : ""}
                    </span>
                  </span>
                </Link>
              );
            })}
          </div>
        )}
      </section>

      <div className="grid">
        <div className="stack">
          <section className="card" aria-labelledby="lb-h">
            <div className="card-head">
              <h2 id="lb-h">Leaderboard</h2>
              <div className="controls">
                <Seg
                  value={period}
                  onChange={setPeriod}
                  options={[
                    ["today", "Today"],
                    ["week", "Week"],
                    ["all", "All time"],
                  ]}
                  label="Period"
                />
                <Seg
                  value={metric}
                  onChange={setMetric}
                  options={[
                    ["tokens", "Tokens"],
                    ["cost", "Known spend"],
                  ]}
                  label="Metric"
                />
                <Seg
                  value={includeSubagents ? "with" : "without"}
                  onChange={(v) => setIncludeSubagents(v === "with")}
                  options={[
                    ["with", "+ subagents"],
                    ["without", "Main only"],
                  ]}
                  label="Subagents"
                />
              </div>
            </div>
            <table className="lb">
              <thead>
                <tr>
                  <th className="rank">#</th>
                  <th>Who</th>
                  <th className="spark-cell">{period === "today" ? "Today" : period === "week" ? "Last 7 days" : "All time"}</th>
                  <th className="num calls">Calls</th>
                  <th className="num">{metric === "tokens" ? "Tokens" : "Known spend"}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(({ profile: p, t, value, note, attribution }, i) => (
                  <tr key={p.id} style={value === 0 ? { opacity: 0.55 } : undefined}>
                    <td className="rank">{value !== null && value > 0 ? i + 1 : "–"}</td>
                    <td>
                      <span className="who">
                        <Avatar profile={p} />
                        <Link href={`/u/${p.handle}`}>{p.display_name}</Link>
                        {liveSet.has(p.id) && <span className="live-dot pulse" aria-label="live now" />}
                      </span>
                    </td>
                    <td className="spark-cell">
                      <Sparkline
                        days={
                          period === "today"
                            ? [props.todayKey]
                            : period === "week"
                              ? weekDays
                              : allDays
                        }
                        values={daily[p.id] ?? {}}
                        label={p.display_name}
                        period={period}
                      />
                    </td>
                    <td className="num calls sub">{t ? t.call_count === null ? "Unknown" : t.call_count.toLocaleString("en-US") : 0}</td>
                    <td className="num">
                      <span className="value">{metric === "tokens" ? tokens(value) : t ? usd(value) : "–"}</span>
                      {note && <div className="sub">{note}</div>}
                      {t?.has_reconstructed && <div className="sub">includes reconstruction</div>}
                      {attribution?.kind === "known-main" && (
                        <div className="sub">Known main: {tokens(attribution.tokens)}</div>
                      )}
                      {attribution?.kind === "exact-share" && (
                        <div className="sub" title="Share from subagent / swarm calls">{attribution.percent} subagent</div>
                      )}
                      {attribution?.kind === "lower-bound" && (
                        <div className="sub" title="Subagent attribution is incomplete">
                          {attribution.percent} subagent<span className="visually-hidden">; subagent attribution is incomplete</span>
                        </div>
                      )}
                      {attribution?.kind === "unknown-share" && (
                        <div className="sub">
                          Subagent share unknown{attribution.tokens !== null && ` · Known subagent: ${tokens(attribution.tokens)}`}
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="sub" style={{ margin: "10px 0 0" }}>
              {metric === "tokens" ? tokens(groupTokens) + " tokens" : usd(groupCost) + " known spend"} across the group, {periodLabel}
              {groupNote && ` (${groupNote})`}.
            </p>
          </section>

          <section className="card" aria-labelledby="daily-h">
            <div className="card-head">
              <h2 id="daily-h">Group tokens per day</h2>
              <span className="sub">last 30 days, UTC</span>
            </div>
            <DailyBars points={groupDaily} />
          </section>
        </div>

        <section className="card" aria-labelledby="feed-h">
          <div className="card-head">
            <h2 id="feed-h">Activity</h2>
            <span className="sub">Latest calls</span>
          </div>
          <ActivityTagControls />
          {feed.length === 0 ? (
            <p className="empty">No calls yet. Set up OMP or munch from Settings.</p>
          ) : (
            <ul className="feed" aria-live="polite">
              {feed.map((e) => {
                const p = profiles[e.user_id];
                return (
                  <li key={e.id} className={fresh.has(e.id) ? "fresh" : undefined}>
                    <Avatar profile={p} size={24} />
                    <span className="line">
                      <strong>{p?.display_name ?? "…"}</strong>, {tokens(e.total_tokens)} tokens on {model(e.model)}
                      <ActivitySourceBadge source={e.source} />
                      {e.is_subagent && <>{" "}<span className="badge">subagent</span></>}
                    </span>
                    <span className="meta" title={new Date(e.ts).toLocaleString()}>
                      {ago(e.ts, now)}
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      </div>
    </div>
  );
}

function Seg<T extends string>({
  value,
  onChange,
  options,
  label,
}: {
  value: T;
  onChange: (v: T) => void;
  options: [T, string][];
  label: string;
}) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map(([v, text]) => (
        <button key={v} type="button" aria-pressed={value === v} onClick={() => onChange(v)}>
          {text}
        </button>
      ))}
    </div>
  );
}
