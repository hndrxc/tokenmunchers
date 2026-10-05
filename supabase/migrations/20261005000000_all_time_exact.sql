-- All-time totals: rollups for past UTC days plus exact raw events for today,
-- so the number never lags behind the live feed.
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
