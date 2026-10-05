-- 1. Raw events are kept 8 days instead of 30; usage_daily stays forever.
-- 2. usage_history holds one-off imports of usage from before a machine started
--    reporting live (daily totals from OMP's local stats.db), per device.
-- 3. Leaderboards and charts read live rollups + history together.

-- ---------------------------------------------------------------------------
-- Retention + rollup windows
-- ---------------------------------------------------------------------------

-- Ingest accepts events up to 7 days old, so the newest UTC day a late event
-- can land on is today - 7. Re-rolling today-7..today catches every late event,
-- and raw rows are kept until the start of UTC day today-8, so a re-roll never
-- sees a day whose raw rows have been partly deleted (which would overwrite a
-- good rollup with a partial one).
select cron.unschedule('tokenmunchers-rollup');
select cron.schedule(
  'tokenmunchers-rollup',
  '*/10 * * * *',
  $$select private.rollup_usage(((now() at time zone 'utc')::date - 7), (now() at time zone 'utc')::date)$$
);

select cron.unschedule('tokenmunchers-retention');
select cron.schedule(
  'tokenmunchers-retention',
  '15 3 * * *',
  $$
    delete from public.usage_events
      where ts < (((now() at time zone 'utc')::date - 8)::timestamp at time zone 'utc');
    delete from private.rate_limits where window_start < now() - interval '1 hour';
  $$
);

create or replace function public.leaderboard_since(p_since timestamptz)
returns table (
  user_id uuid,
  call_count bigint,
  input_tokens bigint,
  output_tokens bigint,
  cache_read_tokens bigint,
  cache_write_tokens bigint,
  total_tokens bigint,
  subagent_tokens bigint,
  cost_usd numeric
)
language sql
stable
security invoker
set search_path = ''
as $$
  select
    e.user_id,
    count(*),
    sum(e.input_tokens)::bigint,
    sum(e.output_tokens)::bigint,
    sum(e.cache_read_tokens)::bigint,
    sum(e.cache_write_tokens)::bigint,
    sum(e.total_tokens)::bigint,
    coalesce(sum(e.total_tokens) filter (where e.is_subagent), 0)::bigint,
    sum(e.cost_usd)
  from public.usage_events e
  where e.ts >= greatest(p_since, now() - interval '7 days')
  group by e.user_id;
$$;

create or replace function public.leaderboard(p_days int default 7)
returns table (
  user_id uuid,
  call_count bigint,
  input_tokens bigint,
  output_tokens bigint,
  cache_read_tokens bigint,
  cache_write_tokens bigint,
  total_tokens bigint,
  subagent_tokens bigint,
  cost_usd numeric
)
language sql
stable
security invoker
set search_path = ''
as $$
  select
    e.user_id,
    count(*),
    sum(e.input_tokens)::bigint,
    sum(e.output_tokens)::bigint,
    sum(e.cache_read_tokens)::bigint,
    sum(e.cache_write_tokens)::bigint,
    sum(e.total_tokens)::bigint,
    coalesce(sum(e.total_tokens) filter (where e.is_subagent), 0)::bigint,
    sum(e.cost_usd)
  from public.usage_events e
  where e.ts >= now() - make_interval(days => least(greatest(p_days, 1), 7))
  group by e.user_id;
$$;

-- ---------------------------------------------------------------------------
-- Pre-install history
-- ---------------------------------------------------------------------------

-- One row per user/device/UTC day/provider/model/subagent flag. A device is one
-- plugin install (random id kept on that machine). Each upload replaces the
-- device's previous one, so re-running the import never double counts, and two
-- machines never overwrite each other. The plugin only uploads calls made
-- before that machine's first live event, so history and live never overlap.
create table public.usage_history (
  user_id uuid not null references public.profiles (id) on delete cascade,
  device_id uuid not null,
  day date not null,
  provider text not null check (char_length(provider) between 1 and 64),
  model text not null check (char_length(model) between 1 and 128),
  is_subagent boolean not null,
  call_count bigint not null default 0 check (call_count >= 0),
  input_tokens bigint not null default 0 check (input_tokens >= 0),
  output_tokens bigint not null default 0 check (output_tokens >= 0),
  cache_read_tokens bigint not null default 0 check (cache_read_tokens >= 0),
  cache_write_tokens bigint not null default 0 check (cache_write_tokens >= 0),
  total_tokens bigint generated always as
    (input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) stored,
  cost_usd numeric(16, 6) not null default 0 check (cost_usd >= 0),
  uploaded_at timestamptz not null default now(),
  primary key (user_id, device_id, day, provider, model, is_subagent)
);
create index usage_history_day_idx on public.usage_history (day);

revoke all on public.usage_history from anon, authenticated;
alter table public.usage_history enable row level security;
grant select on public.usage_history to authenticated;
create policy "members read history" on public.usage_history
  for select to authenticated using ((select private.is_member()));

-- Live rollups and imported history, summed per user/day/provider/model/flag.
-- security_invoker so the RLS policies of both tables apply to the caller.
create view public.usage_daily_all
with (security_invoker = true)
as
  select user_id, day, provider, model, is_subagent,
         sum(call_count)::bigint as call_count,
         sum(input_tokens)::bigint as input_tokens,
         sum(output_tokens)::bigint as output_tokens,
         sum(cache_read_tokens)::bigint as cache_read_tokens,
         sum(cache_write_tokens)::bigint as cache_write_tokens,
         sum(total_tokens)::bigint as total_tokens,
         sum(cost_usd) as cost_usd
  from (
    select user_id, day, provider, model, is_subagent, call_count, input_tokens, output_tokens,
           cache_read_tokens, cache_write_tokens, total_tokens, cost_usd
    from public.usage_daily
    union all
    select user_id, day, provider, model, is_subagent, call_count, input_tokens, output_tokens,
           cache_read_tokens, cache_write_tokens, total_tokens, cost_usd
    from public.usage_history
  ) u
  group by user_id, day, provider, model, is_subagent;

revoke all on public.usage_daily_all from anon, authenticated;
grant select on public.usage_daily_all to authenticated;

-- All-time: imported history + rollups for past UTC days + raw events for today.
create or replace function public.leaderboard_all_time()
returns table (
  user_id uuid,
  call_count bigint,
  input_tokens bigint,
  output_tokens bigint,
  cache_read_tokens bigint,
  cache_write_tokens bigint,
  total_tokens bigint,
  subagent_tokens bigint,
  cost_usd numeric
)
language sql
stable
security invoker
set search_path = ''
as $$
  with today as (select (now() at time zone 'utc')::date as d),
  parts as (
    select d.user_id, d.call_count, d.input_tokens, d.output_tokens, d.cache_read_tokens,
           d.cache_write_tokens, d.total_tokens,
           case when d.is_subagent then d.total_tokens else 0 end as subagent_tokens, d.cost_usd
    from public.usage_daily d, today
    where d.day < today.d
    union all
    select h.user_id, h.call_count, h.input_tokens, h.output_tokens, h.cache_read_tokens,
           h.cache_write_tokens, h.total_tokens,
           case when h.is_subagent then h.total_tokens else 0 end, h.cost_usd
    from public.usage_history h
    union all
    select e.user_id, 1, e.input_tokens, e.output_tokens, e.cache_read_tokens,
           e.cache_write_tokens, e.total_tokens,
           case when e.is_subagent then e.total_tokens else 0 end, e.cost_usd
    from public.usage_events e, today
    where e.ts >= today.d::timestamp at time zone 'utc'
  )
  select p.user_id, sum(p.call_count)::bigint, sum(p.input_tokens)::bigint, sum(p.output_tokens)::bigint,
         sum(p.cache_read_tokens)::bigint, sum(p.cache_write_tokens)::bigint, sum(p.total_tokens)::bigint,
         sum(p.subagent_tokens)::bigint, sum(p.cost_usd)
  from parts p
  group by p.user_id;
$$;

-- ---------------------------------------------------------------------------
-- History RPC (called only by the history Edge Function with the secret key)
-- ---------------------------------------------------------------------------

-- p_action 'status': returns the user's earliest live event (used by plugins
--   upgrading from 0.1.0 to find where their live data starts) and this
--   device's current upload.
-- p_action 'upload': with p_reset, deletes the device's previous upload first
--   (the first chunk of an import), then upserts p_rows.
create function public.ingest_history(
  p_key_hash text,
  p_action text,
  p_device_id uuid,
  p_reset boolean default false,
  p_rows jsonb default '[]'::jsonb
)
returns jsonb
language plpgsql
volatile
security definer
set search_path = ''
as $$
declare
  v_key_id uuid;
  v_user_id uuid;
  v_hits int;
  v_written int := 0;
begin
  select k.id, k.user_id into v_key_id, v_user_id
  from public.api_keys k
  where k.key_hash = p_key_hash and k.revoked_at is null;

  if v_key_id is null then
    return jsonb_build_object('error', 'invalid_key');
  end if;

  -- Shares the per-key budget with ingest; a history request costs 10 hits.
  insert into private.rate_limits as r (key_id, window_start, hits)
  values (v_key_id, date_trunc('minute', now()), 10)
  on conflict (key_id, window_start) do update set hits = r.hits + excluded.hits
  returning hits into v_hits;

  if v_hits > 600 then
    return jsonb_build_object('error', 'rate_limited');
  end if;

  if p_action = 'status' then
    return jsonb_build_object(
      -- The exact first raw event when raw rows still cover the first day,
      -- otherwise the start of the first rolled-up day.
      'first_event_at', (
        select case
          when ev.t is not null and (dd.d is null or dd.d >= (ev.t at time zone 'utc')::date) then ev.t
          else dd.d::timestamp at time zone 'utc'
        end
        from (select min(e.ts) as t from public.usage_events e where e.user_id = v_user_id) ev,
             (select min(d.day) as d from public.usage_daily d where d.user_id = v_user_id) dd
      ),
      'device', (
        select jsonb_build_object(
          'rows', count(*),
          'days', count(distinct h.day),
          'total_tokens', coalesce(sum(h.total_tokens), 0),
          'uploaded_at', max(h.uploaded_at)
        )
        from public.usage_history h
        where h.user_id = v_user_id and h.device_id = p_device_id
      )
    );
  end if;

  if p_action <> 'upload' then
    return jsonb_build_object('error', 'unknown_action');
  end if;

  if p_reset then
    delete from public.usage_history h where h.user_id = v_user_id and h.device_id = p_device_id;
  end if;

  with ins as (
    insert into public.usage_history as h (
      user_id, device_id, day, provider, model, is_subagent, call_count,
      input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd
    )
    select
      v_user_id, p_device_id, r.day, r.provider, r.model, r.is_subagent, r.call_count,
      r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.cost_usd
    from jsonb_to_recordset(p_rows) as r (
      day date, provider text, model text, is_subagent boolean, call_count bigint,
      input_tokens bigint, output_tokens bigint, cache_read_tokens bigint,
      cache_write_tokens bigint, cost_usd numeric
    )
    on conflict (user_id, device_id, day, provider, model, is_subagent) do update set
      call_count = excluded.call_count,
      input_tokens = excluded.input_tokens,
      output_tokens = excluded.output_tokens,
      cache_read_tokens = excluded.cache_read_tokens,
      cache_write_tokens = excluded.cache_write_tokens,
      cost_usd = excluded.cost_usd,
      uploaded_at = now()
    returning 1
  )
  select count(*) into v_written from ins;

  return jsonb_build_object('written', v_written);
end;
$$;
revoke all on function public.ingest_history(text, text, uuid, boolean, jsonb) from public, anon, authenticated;
grant execute on function public.ingest_history(text, text, uuid, boolean, jsonb) to service_role;
