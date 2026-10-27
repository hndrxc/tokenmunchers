-- ---------------------------------------------------------------------------
-- Discord Bot Read-Only Role & Leaderboard RPC
-- ---------------------------------------------------------------------------

-- Dedicated NOLOGIN role for the Discord bot read-only service.
-- An admin separately provisions a LOGIN role that inherits this role.
do $$
begin
  if not exists (select 1 from pg_catalog.pg_roles where rolname = 'tokenmunchers_discord_reader') then
    create role tokenmunchers_discord_reader nologin;
  end if;
end;
$$;

-- Guard schema usage: grant only USAGE to reader, ensure no CREATE privileges.
revoke create on schema public from public;
revoke all on schema public from tokenmunchers_discord_reader;
grant usage on schema public to tokenmunchers_discord_reader;

-- Wrapper function for Discord bot /leaderboard command:
-- p_period: 'today' (UTC day start), 'week' (rolling 7 days), or 'all' (all-time).
-- Returns handle, display_name, total_tokens, subagent_tokens, cost_usd.
-- Joins all registered profiles (zero-filled for profiles without usage).
create or replace function public.discord_leaderboard(p_period text default 'week')
returns table (
  handle text,
  display_name text,
  total_tokens bigint,
  subagent_tokens bigint,
  cost_usd numeric
)
language plpgsql
stable
security definer
set search_path = ''
as $$
begin
  if p_period is null or p_period not in ('today', 'week', 'all') then
    raise exception 'invalid period: %; expected today, week, or all', p_period
      using errcode = '22023';
  end if;

  if p_period = 'today' then
    return query
      select
        p.handle,
        p.display_name,
        coalesce(l.total_tokens, 0::bigint) as total_tokens,
        coalesce(l.subagent_tokens, 0::bigint) as subagent_tokens,
        coalesce(l.cost_usd, 0::numeric) as cost_usd
      from public.profiles p
      left join public.leaderboard_since(
        ((now() at time zone 'utc')::date)::timestamp at time zone 'utc'
      ) l on l.user_id = p.id;
  elsif p_period = 'week' then
    return query
      select
        p.handle,
        p.display_name,
        coalesce(l.total_tokens, 0::bigint) as total_tokens,
        coalesce(l.subagent_tokens, 0::bigint) as subagent_tokens,
        coalesce(l.cost_usd, 0::numeric) as cost_usd
      from public.profiles p
      left join public.leaderboard_since(
        now() - interval '7 days'
      ) l on l.user_id = p.id;
  else
    return query
      select
        p.handle,
        p.display_name,
        coalesce(l.total_tokens, 0::bigint) as total_tokens,
        coalesce(l.subagent_tokens, 0::bigint) as subagent_tokens,
        coalesce(l.cost_usd, 0::numeric) as cost_usd
      from public.profiles p
      left join public.leaderboard_all_time() l on l.user_id = p.id;
  end if;
end;
$$;

-- Revoke execute from public, anon, and authenticated so wrapper is inaccessible to web/client RPCs.
-- Grant execute strictly to tokenmunchers_discord_reader.
revoke all on function public.discord_leaderboard(text) from public, anon, authenticated;
grant execute on function public.discord_leaderboard(text) to tokenmunchers_discord_reader;
