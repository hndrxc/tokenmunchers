-- Per-user totals since an arbitrary timestamp (e.g. start of today, or 7 days ago).
-- Runs as the caller, so RLS on usage_events applies.
create function public.leaderboard_since(p_since timestamptz)
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
  where e.ts >= greatest(p_since, now() - interval '30 days')
  group by e.user_id;
$$;
revoke all on function public.leaderboard_since(timestamptz) from public, anon;
grant execute on function public.leaderboard_since(timestamptz) to authenticated;
