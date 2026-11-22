-- Activity is a projection of accepted requests, not a second write or rollup path.
create index collector_contributions_activity_ts_idx
  on public.collector_contributions (ts desc)
  where partition = 'live' and not superseded
    and observation->>'grain' = 'request'
    and observation->>'identity_strength' in ('request', 'response')
    and observation->>'quality' in ('observed', 'partial');

create index collector_contributions_activity_user_ts_idx
  on public.collector_contributions (user_id, ts desc)
  where partition = 'live' and not superseded
    and observation->>'grain' = 'request'
    and observation->>'identity_strength' in ('request', 'response')
    and observation->>'quality' in ('observed', 'partial');

create function public.recent_activity(p_user_id uuid default null)
returns table (
  id text,
  source text,
  user_id uuid,
  ts timestamptz,
  provider text,
  model text,
  input_tokens bigint,
  output_tokens bigint,
  cache_read_tokens bigint,
  cache_write_tokens bigint,
  total_tokens bigint,
  cost_usd numeric,
  is_subagent boolean
)
language sql stable security invoker set search_path = '' as $$
  with ingest as (
    select ('ingest:' || e.event_id::text) collate pg_catalog."C" as id,
      'ingest'::text as source, e.user_id, e.ts, e.provider, e.model,
      e.input_tokens, e.output_tokens, e.cache_read_tokens, e.cache_write_tokens,
      e.total_tokens, e.cost_usd, e.is_subagent
    from public.usage_events e
    where p_user_id is null or e.user_id = p_user_id
    order by e.ts desc, id
    limit case when p_user_id is null then 50 else 25 end
  ), collector as (
    -- UUID and harness have fixed delimiter-free representations; the last field
    -- is the whole canonical ID. Built-in SHA-256 needs no extension-schema grant.
    select ('collector:' || encode(sha256(convert_to(
        c.user_id::text || ':' || c.harness || ':' || c.canonical_id, 'UTF8'
      )), 'hex')) collate pg_catalog."C" as id,
      c.harness as source, c.user_id, c.ts,
      c.observation->>'provider' as provider, c.observation->>'model' as model,
      c.input_tokens, c.output_tokens, c.cache_read_tokens, c.cache_write_tokens,
      c.total_tokens, c.cost_usd, (c.observation->>'is_subagent')::boolean as is_subagent
    from public.collector_contributions c
    where (p_user_id is null or c.user_id = p_user_id)
      and c.partition = 'live' and not c.superseded
      and c.observation->>'grain' = 'request'
      and c.observation->>'identity_strength' in ('request', 'response')
      and c.observation->>'quality' in ('observed', 'partial')
      -- Match the UTC-day horizon of raw event retention without deleting collector rows.
      and c.ts >= (((now() at time zone 'utc')::date - 8)::timestamp at time zone 'utc')
    order by c.ts desc, id
    limit case when p_user_id is null then 50 else 25 end
  )
  select * from ingest
  union all
  select * from collector
  order by ts desc, id
  limit case when p_user_id is null then 50 else 25 end;
$$;

revoke all on function public.recent_activity(uuid) from public, anon, authenticated, service_role;
grant execute on function public.recent_activity(uuid) to authenticated;
