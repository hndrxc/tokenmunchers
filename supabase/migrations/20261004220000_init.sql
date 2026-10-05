-- tokenmunchers: initial schema
-- Tables: allowlist, profiles, api_keys, usage_events, usage_daily, live_sessions
-- Only the ingest Edge Function (service role) writes usage data.

create extension if not exists pgcrypto with schema extensions;
create extension if not exists pg_cron with schema pg_catalog;

create schema if not exists private;
revoke all on schema private from public;
grant usage on schema private to authenticated;

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

-- Invite-only: sign-up is rejected unless the email is listed here.
create table public.allowlist (
  email text primary key check (email = lower(email)),
  added_at timestamptz not null default now()
);

create table public.profiles (
  id uuid primary key references auth.users (id) on delete cascade,
  handle text not null unique check (handle ~ '^[a-z0-9_-]{2,32}$'),
  display_name text not null check (char_length(display_name) between 1 and 64),
  avatar_url text check (char_length(avatar_url) <= 512),
  created_at timestamptz not null default now()
);

create table public.api_keys (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references public.profiles (id) on delete cascade,
  label text not null default 'omp' check (char_length(label) between 1 and 64),
  key_hash text not null unique check (key_hash ~ '^[0-9a-f]{64}$'),
  key_prefix text not null,
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);
create index api_keys_user_id_idx on public.api_keys (user_id);

create table public.usage_events (
  event_id uuid primary key,
  user_id uuid not null references public.profiles (id) on delete cascade,
  session_id text not null check (char_length(session_id) between 1 and 128),
  ts timestamptz not null,
  received_at timestamptz not null default now(),
  provider text not null check (char_length(provider) between 1 and 64),
  model text not null check (char_length(model) between 1 and 128),
  input_tokens bigint not null default 0 check (input_tokens >= 0),
  output_tokens bigint not null default 0 check (output_tokens >= 0),
  cache_read_tokens bigint not null default 0 check (cache_read_tokens >= 0),
  cache_write_tokens bigint not null default 0 check (cache_write_tokens >= 0),
  total_tokens bigint generated always as
    (input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) stored,
  cost_usd numeric(14, 6) not null default 0 check (cost_usd >= 0),
  is_subagent boolean not null default false,
  client_version text check (char_length(client_version) <= 32)
);
create index usage_events_user_ts_idx on public.usage_events (user_id, ts desc);
create index usage_events_ts_idx on public.usage_events (ts desc);

create table public.usage_daily (
  user_id uuid not null references public.profiles (id) on delete cascade,
  day date not null,
  provider text not null,
  model text not null,
  is_subagent boolean not null,
  call_count bigint not null default 0,
  input_tokens bigint not null default 0,
  output_tokens bigint not null default 0,
  cache_read_tokens bigint not null default 0,
  cache_write_tokens bigint not null default 0,
  total_tokens bigint not null default 0,
  cost_usd numeric(16, 6) not null default 0,
  primary key (user_id, day, provider, model, is_subagent)
);
create index usage_daily_day_idx on public.usage_daily (day);

-- Presence. A row is "live" while last_seen is within 90 seconds.
create table public.live_sessions (
  user_id uuid not null references public.profiles (id) on delete cascade,
  session_id text not null check (char_length(session_id) between 1 and 128),
  provider text,
  model text,
  started_at timestamptz not null default now(),
  last_seen timestamptz not null default now(),
  primary key (user_id, session_id)
);

create table private.rate_limits (
  key_id uuid not null,
  window_start timestamptz not null,
  hits int not null default 0,
  primary key (key_id, window_start)
);

-- ---------------------------------------------------------------------------
-- Membership helper (security definer so policies on profiles don't recurse)
-- ---------------------------------------------------------------------------

create function private.is_member()
returns boolean
language sql
stable
security definer
set search_path = ''
as $$
  select exists (select 1 from public.profiles where id = (select auth.uid()));
$$;
revoke all on function private.is_member() from public;
grant execute on function private.is_member() to authenticated;

-- ---------------------------------------------------------------------------
-- Grants + RLS
-- ---------------------------------------------------------------------------

revoke all on public.allowlist, public.profiles, public.api_keys, public.usage_events,
  public.usage_daily, public.live_sessions from anon, authenticated;

alter table public.allowlist enable row level security;
alter table public.profiles enable row level security;
alter table public.api_keys enable row level security;
alter table public.usage_events enable row level security;
alter table public.usage_daily enable row level security;
alter table public.live_sessions enable row level security;

-- allowlist: no client access at all (managed from the SQL editor).

-- profiles: friends read everyone; you edit only your own display fields.
grant select on public.profiles to authenticated;
grant update (handle, display_name, avatar_url) on public.profiles to authenticated;
create policy "members read profiles" on public.profiles
  for select to authenticated using ((select private.is_member()));
create policy "users update own profile" on public.profiles
  for update to authenticated
  using (id = (select auth.uid()))
  with check (id = (select auth.uid()));

-- api_keys: strictly your own. Plaintext keys never touch the table.
grant select (id, user_id, label, key_prefix, created_at, last_used_at, revoked_at)
  on public.api_keys to authenticated;
grant insert (user_id, label, key_hash, key_prefix) on public.api_keys to authenticated;
grant update (revoked_at) on public.api_keys to authenticated;
create policy "users read own keys" on public.api_keys
  for select to authenticated using (user_id = (select auth.uid()));
create policy "members create own keys" on public.api_keys
  for insert to authenticated
  with check (user_id = (select auth.uid()) and (select private.is_member()));
create policy "users revoke own keys" on public.api_keys
  for update to authenticated
  using (user_id = (select auth.uid()))
  with check (user_id = (select auth.uid()));

-- usage tables: read-only for members. No client insert/update/delete grants.
grant select on public.usage_events, public.usage_daily, public.live_sessions to authenticated;
create policy "members read events" on public.usage_events
  for select to authenticated using ((select private.is_member()));
create policy "members read daily" on public.usage_daily
  for select to authenticated using ((select private.is_member()));
create policy "members read presence" on public.live_sessions
  for select to authenticated using ((select private.is_member()));

-- ---------------------------------------------------------------------------
-- New users: invite check + profile creation
-- ---------------------------------------------------------------------------

create function private.handle_new_user()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
declare
  meta jsonb := coalesce(new.raw_user_meta_data, '{}'::jsonb);
  base text;
  candidate text;
begin
  if not exists (select 1 from public.allowlist where email = lower(new.email)) then
    raise exception 'tokenmunchers is invite-only: % is not on the allowlist', new.email
      using errcode = '42501';
  end if;

  base := lower(coalesce(meta->>'user_name', meta->>'preferred_username', meta->>'name',
                         split_part(new.email, '@', 1), 'user'));
  base := left(regexp_replace(base, '[^a-z0-9_-]', '', 'g'), 24);
  if char_length(base) < 2 then base := 'user'; end if;

  candidate := base;
  if exists (select 1 from public.profiles where handle = candidate) then
    candidate := base || '-' || left(replace(new.id::text, '-', ''), 6);
  end if;

  insert into public.profiles (id, handle, display_name, avatar_url)
  values (
    new.id,
    candidate,
    left(coalesce(meta->>'full_name', meta->>'name', meta->>'user_name', candidate), 64),
    left(meta->>'avatar_url', 512)
  );
  return new;
end;
$$;

create trigger on_auth_user_created
  after insert on auth.users
  for each row execute function private.handle_new_user();

-- ---------------------------------------------------------------------------
-- Client RPCs
-- ---------------------------------------------------------------------------

-- Creates a plugin key and returns the plaintext once. Runs as the caller, so
-- the insert is checked by the api_keys RLS policy.
create function public.create_api_key(p_label text default 'omp')
returns text
language plpgsql
volatile
security invoker
set search_path = ''
as $$
declare
  v_key text := 'tm_' || translate(encode(extensions.gen_random_bytes(32), 'base64'), '+/=', '-_');
begin
  insert into public.api_keys (user_id, label, key_hash, key_prefix)
  values (
    (select auth.uid()),
    coalesce(nullif(trim(p_label), ''), 'omp'),
    encode(extensions.digest(v_key, 'sha256'), 'hex'),
    left(v_key, 10)
  );
  return v_key;
end;
$$;
revoke all on function public.create_api_key(text) from public, anon;
grant execute on function public.create_api_key(text) to authenticated;

-- Deletes the caller's account; cascades to profile, keys and all events.
create function public.delete_my_account()
returns void
language plpgsql
volatile
security definer
set search_path = ''
as $$
declare
  v_uid uuid := (select auth.uid());
begin
  if v_uid is null then
    raise exception 'not signed in' using errcode = '42501';
  end if;
  delete from auth.users where id = v_uid;
end;
$$;
revoke all on function public.delete_my_account() from public, anon;
grant execute on function public.delete_my_account() to authenticated;

-- Leaderboard over raw events (last N days). Runs as the caller, so RLS applies.
create function public.leaderboard(p_days int default 7)
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
  where e.ts >= now() - make_interval(days => least(greatest(p_days, 1), 30))
  group by e.user_id;
$$;
revoke all on function public.leaderboard(int) from public, anon;
grant execute on function public.leaderboard(int) to authenticated;

-- All-time leaderboard from rollups.
create function public.leaderboard_all_time()
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
    d.user_id,
    sum(d.call_count)::bigint,
    sum(d.input_tokens)::bigint,
    sum(d.output_tokens)::bigint,
    sum(d.cache_read_tokens)::bigint,
    sum(d.cache_write_tokens)::bigint,
    sum(d.total_tokens)::bigint,
    coalesce(sum(d.total_tokens) filter (where d.is_subagent), 0)::bigint,
    sum(d.cost_usd)
  from public.usage_daily d
  group by d.user_id;
$$;
revoke all on function public.leaderboard_all_time() from public, anon;
grant execute on function public.leaderboard_all_time() to authenticated;

-- ---------------------------------------------------------------------------
-- Ingest (called only by the Edge Function with the secret key)
-- ---------------------------------------------------------------------------

-- p_events: array of already-validated events, each with a "kind" of
-- usage | session_start | heartbeat | session_end.
create function public.ingest(p_key_hash text, p_events jsonb)
returns jsonb
language plpgsql
volatile
security definer
set search_path = ''
as $$
declare
  v_key_id uuid;
  v_user_id uuid;
  v_last_used timestamptz;
  v_hits int;
  v_inserted int := 0;
  v_count int := jsonb_array_length(p_events);
begin
  select k.id, k.user_id, k.last_used_at into v_key_id, v_user_id, v_last_used
  from public.api_keys k
  where k.key_hash = p_key_hash and k.revoked_at is null;

  if v_key_id is null then
    return jsonb_build_object('error', 'invalid_key');
  end if;

  insert into private.rate_limits as r (key_id, window_start, hits)
  values (v_key_id, date_trunc('minute', now()), v_count)
  on conflict (key_id, window_start) do update set hits = r.hits + excluded.hits
  returning hits into v_hits;

  if v_hits > 600 then
    return jsonb_build_object('error', 'rate_limited');
  end if;

  if v_last_used is null or v_last_used < now() - interval '1 minute' then
    update public.api_keys set last_used_at = now() where id = v_key_id;
  end if;

  with ins as (
    insert into public.usage_events (
      event_id, user_id, session_id, ts, provider, model,
      input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
      cost_usd, is_subagent, client_version
    )
    select
      e.event_id, v_user_id, e.session_id, e.ts, e.provider, e.model,
      e.input_tokens, e.output_tokens, e.cache_read_tokens, e.cache_write_tokens,
      e.cost_usd, e.is_subagent, e.client_version
    from jsonb_to_recordset(p_events) as e (
      kind text, event_id uuid, session_id text, ts timestamptz, provider text, model text,
      input_tokens bigint, output_tokens bigint, cache_read_tokens bigint,
      cache_write_tokens bigint, cost_usd numeric, is_subagent boolean, client_version text
    )
    where e.kind = 'usage'
    on conflict (event_id) do nothing
    returning 1
  )
  select count(*) into v_inserted from ins;

  -- Presence: start/heartbeat events, plus fresh main-agent usage events.
  insert into public.live_sessions as l (user_id, session_id, provider, model, started_at, last_seen)
  select distinct on (e.session_id)
    v_user_id, e.session_id, e.provider, e.model,
    coalesce(e.started_at, now()), now()
  from jsonb_to_recordset(p_events) as e (
    kind text, session_id text, provider text, model text, started_at timestamptz,
    ts timestamptz, is_subagent boolean
  )
  where e.kind in ('session_start', 'heartbeat')
     or (e.kind = 'usage' and not coalesce(e.is_subagent, false)
         and e.ts > now() - interval '90 seconds')
  order by e.session_id, e.ts desc nulls last
  on conflict (user_id, session_id) do update set
    provider = coalesce(excluded.provider, l.provider),
    model = coalesce(excluded.model, l.model),
    last_seen = excluded.last_seen;

  delete from public.live_sessions l
  using jsonb_to_recordset(p_events) as e (kind text, session_id text)
  where e.kind = 'session_end' and l.user_id = v_user_id and l.session_id = e.session_id;

  return jsonb_build_object('accepted', v_count, 'inserted', v_inserted);
end;
$$;
revoke all on function public.ingest(text, jsonb) from public, anon, authenticated;
grant execute on function public.ingest(text, jsonb) to service_role;

-- ---------------------------------------------------------------------------
-- Rollups + retention (pg_cron)
-- ---------------------------------------------------------------------------

-- Recomputes daily rollups (UTC days) for the given range. Idempotent.
create function private.rollup_usage(p_from date, p_to date)
returns void
language sql
volatile
security definer
set search_path = ''
as $$
  insert into public.usage_daily as d (
    user_id, day, provider, model, is_subagent, call_count,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_usd
  )
  select
    e.user_id, (e.ts at time zone 'utc')::date, e.provider, e.model, e.is_subagent, count(*),
    sum(e.input_tokens), sum(e.output_tokens), sum(e.cache_read_tokens),
    sum(e.cache_write_tokens), sum(e.total_tokens), sum(e.cost_usd)
  from public.usage_events e
  where e.ts >= p_from::timestamp at time zone 'utc'
    and e.ts < (p_to + 1)::timestamp at time zone 'utc'
  group by 1, 2, 3, 4, 5
  on conflict (user_id, day, provider, model, is_subagent) do update set
    call_count = excluded.call_count,
    input_tokens = excluded.input_tokens,
    output_tokens = excluded.output_tokens,
    cache_read_tokens = excluded.cache_read_tokens,
    cache_write_tokens = excluded.cache_write_tokens,
    total_tokens = excluded.total_tokens,
    cost_usd = excluded.cost_usd;
$$;
revoke all on function private.rollup_usage(date, date) from public;

-- The ingest function rejects events older than 7 days, so re-rolling the last
-- 8 days every 10 minutes always catches late (queued) events before the
-- 30-day raw retention deletes them.
select cron.schedule(
  'tokenmunchers-rollup',
  '*/10 * * * *',
  $$select private.rollup_usage(((now() at time zone 'utc')::date - 8), (now() at time zone 'utc')::date)$$
);

select cron.schedule(
  'tokenmunchers-retention',
  '15 3 * * *',
  $$
    delete from public.usage_events where ts < now() - interval '30 days';
    delete from private.rate_limits where window_start < now() - interval '1 hour';
  $$
);

select cron.schedule(
  'tokenmunchers-stale-presence',
  '*/5 * * * *',
  $$delete from public.live_sessions where last_seen < now() - interval '10 minutes'$$
);

-- ---------------------------------------------------------------------------
-- Realtime
-- ---------------------------------------------------------------------------

alter publication supabase_realtime add table public.usage_events, public.live_sessions;
