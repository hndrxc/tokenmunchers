-- Report measured, classified token coverage without changing strict totals.
-- PostgreSQL requires recreation to append columns to a RETURNS TABLE signature.
drop function public.leaderboard(integer);
drop function public.leaderboard_since(timestamptz);
drop function public.leaderboard_all_time();

create function public.leaderboard_all_time()
returns table (
  user_id uuid, call_count bigint, input_tokens bigint, output_tokens bigint,
  cache_read_tokens bigint, cache_write_tokens bigint, total_tokens bigint,
  subagent_tokens bigint, cost_usd numeric, has_unknown_tokens boolean,
  has_unknown_cost boolean, has_unknown_calls boolean, has_estimated_cost boolean,
  has_reconstructed boolean, known_cost_usd numeric, unpriced_tokens bigint,
  known_subagent_tokens bigint, known_main_tokens bigint
)
language sql stable security invoker set search_path = '' as $$
  select user_id,
    case when count(call_count) = count(*) then sum(call_count)::bigint end,
    case when count(input_tokens) = count(*) then sum(input_tokens)::bigint end,
    case when count(output_tokens) = count(*) then sum(output_tokens)::bigint end,
    case when count(cache_read_tokens) = count(*) then sum(cache_read_tokens)::bigint end,
    case when count(cache_write_tokens) = count(*) then sum(cache_write_tokens)::bigint end,
    case when count(total_tokens) = count(*) then sum(total_tokens)::bigint end,
    case when count(subagent_tokens) = count(*) then sum(subagent_tokens)::bigint end,
    case when count(cost_usd) = count(*) then sum(cost_usd) end,
    bool_or(has_unknown_tokens), bool_or(has_unknown_cost), bool_or(has_unknown_calls),
    bool_or(has_estimated_cost), bool_or(has_reconstructed),
    sum(cost_usd),
    case when count(*) filter (where cost_usd is null and total_tokens is null) = 0
      then coalesce(sum(total_tokens) filter (where cost_usd is null), 0)::bigint
    end,
    coalesce(sum(total_tokens) filter (where is_subagent is true), 0)::bigint,
    coalesce(sum(total_tokens) filter (where is_subagent is false), 0)::bigint
  from public.usage_accounting_parts
  group by user_id;
$$;
revoke all on function public.leaderboard_all_time() from public, anon;
grant execute on function public.leaderboard_all_time() to authenticated;

create function public.leaderboard_since(p_since timestamptz)
returns table (
  user_id uuid, call_count bigint, input_tokens bigint, output_tokens bigint,
  cache_read_tokens bigint, cache_write_tokens bigint, total_tokens bigint,
  subagent_tokens bigint, cost_usd numeric, has_unknown_tokens boolean,
  has_unknown_cost boolean, has_unknown_calls boolean, has_estimated_cost boolean,
  has_reconstructed boolean, known_cost_usd numeric, unpriced_tokens bigint,
  known_subagent_tokens bigint, known_main_tokens bigint
)
language sql stable security invoker set search_path = '' as $$
  with parts as (
    -- Only raw OMP usage is bounded by the seven-day window.
    select user_id, 1::bigint as call_count, input_tokens, output_tokens,
      cache_read_tokens, cache_write_tokens, total_tokens,
      case when is_subagent then total_tokens else 0 end as subagent_tokens, cost_usd,
      false as has_unknown_tokens, false as has_unknown_cost, false as has_unknown_calls,
      false as has_estimated_cost, false as has_reconstructed, is_subagent
    from public.usage_events
    where ts >= greatest(p_since, now() - interval '7 days')
    union all
    select user_id, call_count, input_tokens, output_tokens,
      cache_read_tokens, cache_write_tokens, total_tokens, subagent_tokens, cost_usd,
      has_unknown_tokens, has_unknown_cost, has_unknown_calls, has_estimated_cost, has_reconstructed,
      is_subagent
    from public.collector_accounting_parts
    where ts >= p_since
  )
  select user_id,
    case when count(call_count) = count(*) then sum(call_count)::bigint end,
    case when count(input_tokens) = count(*) then sum(input_tokens)::bigint end,
    case when count(output_tokens) = count(*) then sum(output_tokens)::bigint end,
    case when count(cache_read_tokens) = count(*) then sum(cache_read_tokens)::bigint end,
    case when count(cache_write_tokens) = count(*) then sum(cache_write_tokens)::bigint end,
    case when count(total_tokens) = count(*) then sum(total_tokens)::bigint end,
    case when count(subagent_tokens) = count(*) then sum(subagent_tokens)::bigint end,
    case when count(cost_usd) = count(*) then sum(cost_usd) end,
    bool_or(has_unknown_tokens), bool_or(has_unknown_cost), bool_or(has_unknown_calls),
    bool_or(has_estimated_cost), bool_or(has_reconstructed),
    sum(cost_usd),
    case when count(*) filter (where cost_usd is null and total_tokens is null) = 0
      then coalesce(sum(total_tokens) filter (where cost_usd is null), 0)::bigint
    end,
    coalesce(sum(total_tokens) filter (where is_subagent is true), 0)::bigint,
    coalesce(sum(total_tokens) filter (where is_subagent is false), 0)::bigint
  from parts
  group by user_id;
$$;
revoke all on function public.leaderboard_since(timestamptz) from public, anon;
grant execute on function public.leaderboard_since(timestamptz) to authenticated;

create function public.leaderboard(p_days integer default 7)
returns table (
  user_id uuid, call_count bigint, input_tokens bigint, output_tokens bigint,
  cache_read_tokens bigint, cache_write_tokens bigint, total_tokens bigint,
  subagent_tokens bigint, cost_usd numeric, has_unknown_tokens boolean,
  has_unknown_cost boolean, has_unknown_calls boolean, has_estimated_cost boolean,
  has_reconstructed boolean, known_cost_usd numeric, unpriced_tokens bigint,
  known_subagent_tokens bigint, known_main_tokens bigint
)
language sql stable security invoker set search_path = '' as $$
  select * from public.leaderboard_since(now() - make_interval(days => least(greatest(p_days, 1), 7)));
$$;
revoke all on function public.leaderboard(integer) from public, anon;
grant execute on function public.leaderboard(integer) to authenticated;
