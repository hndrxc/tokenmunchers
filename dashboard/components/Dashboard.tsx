"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Avatar } from "@/components/Avatar";
import { DailyBars } from "@/components/DailyBars";
import { Sparkline } from "@/components/Sparkline";
import { ago, duration, model, tokens, usd } from "@/lib/format";
import { addEvent, dailyByUser, LIVE_WINDOW_MS, metricOf, normalizeEvent, utcDay } from "@/lib/stats";
import { createClient } from "@/lib/supabase/client";
import { subscribeAuthed } from "@/lib/supabase/realtime";
import type { DailyRow, LiveSession, Profile, Totals, UsageEvent } from "@/lib/types";

type Period = "today" | "week" | "all";
type Metric = "tokens" | "cost";

export interface DashboardProps {
  profiles: Profile[];
  week: Record<string, Totals>;
  today: Record<string, Totals>;
  allTime: Record<string, Totals>;
  feed: UsageEvent[];
  live: LiveSession[];
  daily: DailyRow[];
  days: string[];
  todayKey: string;
  serverNow: number;
}

const FEED_LIMIT = 50;

export function Dashboard(props: DashboardProps) {
  const router = useRouter();
  const [profiles, setProfiles] = useState<Record<string, Profile>>(() => Object.fromEntries(props.profiles.map((p) => [p.id, p])));
  const [week, setWeek] = useState(props.week);
  const [today, setToday] = useState(props.today);
  const [allTime, setAllTime] = useState(props.allTime);
  const [feed, setFeed] = useState(props.feed);
  const [fresh, setFresh] = useState<Set<string>>(new Set());
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

  // Clock for relative times and presence expiry; reload at UTC midnight so
  // "today" and the 30-day window re-base.
  useEffect(() => {
    const t = setInterval(() => {
      const n = Date.now();
      setNow(n);
      if (utcDay(n) !== props.todayKey) router.refresh();
    }, 5000);
    setNow(Date.now());
    return () => clearInterval(t);
  }, [props.todayKey, router]);

  const ensureProfile = useCallback(async (userId: string) => {
    if (profilesRef.current[userId]) return;
    const { data } = await createClient().from("profiles").select("id,handle,display_name,avatar_url").eq("id", userId).maybeSingle();
    if (data) setProfiles((p) => ({ ...p, [data.id]: data }));
  }, []);

  useEffect(() => {
    return subscribeAuthed(createClient(), (supabase) =>
      supabase
        .channel("tokenmunchers-live")
        .on("postgres_changes", { event: "INSERT", schema: "public", table: "usage_events" }, (payload) => {
          const e = normalizeEvent(payload.new);
          void ensureProfile(e.user_id);
          setWeek((m) => addEvent(m, e));
          setAllTime((m) => addEvent(m, e));
          if (utcDay(Date.parse(e.ts)) === props.todayKey) setToday((m) => addEvent(m, e));
          setFeed((f) => (f.some((x) => x.event_id === e.event_id) ? f : [e, ...f].slice(0, FEED_LIMIT)));
          setFresh((s) => new Set(s).add(e.event_id));
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
  }, [ensureProfile, props.todayKey]);

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
      .map((p) => ({ profile: p, t: totals[p.id], value: metricOf(totals[p.id], metric, includeSubagents) }))
      .sort((a, b) => b.value - a.value || a.profile.display_name.localeCompare(b.profile.display_name));
  }, [profiles, totals, metric, includeSubagents]);

  const groupDaily = useMemo(
    () => props.days.map((day) => ({ day, value: Object.values(daily).reduce((s, u) => s + (u[day] ?? 0), 0) })),
    [daily, props.days],
  );

  const periodLabel = period === "today" ? "today (UTC)" : period === "week" ? "last 7 days" : "all time";
  const groupTotal = rows.reduce((s, r) => s + r.value, 0);

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
                    ["cost", "Cost"],
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
                  <th className="spark-cell">Last 30 days</th>
                  <th className="num calls">Calls</th>
                  <th className="num">{metric === "tokens" ? "Tokens" : "Cost"}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(({ profile: p, t, value }, i) => (
                  <tr key={p.id} style={value === 0 ? { opacity: 0.55 } : undefined}>
                    <td className="rank">{value > 0 ? i + 1 : "–"}</td>
                    <td>
                      <span className="who">
                        <Avatar profile={p} />
                        <Link href={`/u/${p.handle}`}>{p.display_name}</Link>
                        {liveSet.has(p.id) && <span className="live-dot pulse" aria-label="live now" />}
                      </span>
                    </td>
                    <td className="spark-cell">
                      <Sparkline days={props.days} values={daily[p.id] ?? {}} label={p.display_name} />
                    </td>
                    <td className="num calls sub">{t ? t.call_count.toLocaleString("en-US") : 0}</td>
                    <td className="num">
                      <span className="value">{metric === "tokens" ? tokens(value) : usd(value)}</span>
                      {metric === "tokens" && includeSubagents && t && t.subagent_tokens > 0 && (
                        <div className="sub" title="Share from subagent / swarm calls">
                          {pct(t.subagent_tokens, t.total_tokens)} subagent
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="sub" style={{ margin: "10px 0 0" }}>
              {metric === "tokens" ? tokens(groupTotal) + " tokens" : usd(groupTotal)} across the group, {periodLabel}.
              {metric === "cost" && " Cost is as reported by OMP and is $0 on subscription plans."}
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
            <span className="sub">every model call, as it lands</span>
          </div>
          {feed.length === 0 ? (
            <p className="empty">No calls yet. Install the OMP plugin from Settings.</p>
          ) : (
            <ul className="feed" aria-live="polite">
              {feed.map((e) => {
                const p = profiles[e.user_id];
                return (
                  <li key={e.event_id} className={fresh.has(e.event_id) ? "fresh" : undefined}>
                    <Avatar profile={p} size={24} />
                    <span className="line">
                      <strong>{p?.display_name ?? "…"}</strong>, {tokens(e.total_tokens)} tokens on {model(e.model)}
                      {e.is_subagent && <span className="badge">subagent</span>}
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

function pct(part: number, whole: number): string {
  const p = (part / Math.max(1, whole)) * 100;
  return p > 0 && p < 1 ? "<1%" : `${Math.round(p)}%`;
}
