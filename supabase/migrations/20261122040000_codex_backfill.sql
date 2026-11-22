-- Codex backfill mode: an optional inclusive backfill_from per source.
-- Backfill adds an immutable source mode whose staged rows must be observed
-- Codex request/response records inside
-- [backfill_from, cutoff), with all tokens present, one call, nonzero total,
-- unknown cost, and a valid session. Inserts are history append-only: already
-- accepted canonical IDs (any device/metadata/partition, superseded included)
-- and reserved retirement IDs are never updated or resurrected. Modern
-- candidates on a UTC date with any active reconstructed Codex contribution for
-- that user are conservatively skipped so legacy interval rows are preserved.
-- Normal commits acknowledge but do not write observations owned by another
-- backfill source. Aliases may retire the publishing source's request identity
-- in favor of an active backfill response; cross-source supersession still fails.
alter table private.collector_sources add column backfill_from timestamptz;
alter table private.collector_sources add constraint collector_sources_backfill_mode_check check (backfill_from is null or (harness='codex' and backfill_from<cutoff));

create or replace function public.publish_contributions(p_key_hash text,p_request jsonb)
returns jsonb language plpgsql volatile security definer set search_path = '' as $$
declare
  uid uuid; kid uuid; hits integer;
  dev uuid := (p_request->>'device_id')::uuid;
  sid uuid := (p_request->>'snapshot_id')::uuid;
  source text := p_request->>'harness';
  action text := p_request->>'action';
  bf timestamptz := (p_request->>'backfill_from')::timestamptz;
  backfill boolean := p_request ? 'backfill_from';
  src private.collector_sources%rowtype;
  snap private.contribution_snapshots%rowtype;
  payload jsonb; retired jsonb; aliases jsonb; payload_digest text; previous_digest text;
  total integer; distinct_total integer; conflict_id text; fresh integer;
begin
  select k.user_id,k.id into uid,kid from public.api_keys k where k.key_hash=p_key_hash and k.revoked_at is null;
  if uid is null then return jsonb_build_object('error','invalid_key'); end if;
  insert into private.rate_limits as r(key_id,window_start,hits) values(kid,date_trunc('minute',now()),10)
    on conflict(key_id,window_start) do update set hits=r.hits+10 returning r.hits into hits;
  if hits>600 then return jsonb_build_object('error','rate_limited','retry_after_seconds',greatest(1,ceil(extract(epoch from date_trunc('minute',now())+interval '1 minute'-now()))::integer)); end if;
  update public.api_keys set last_used_at=now() where id=kid and (last_used_at is null or last_used_at<now()-interval '1 minute');
  if source not in ('claude-code','codex') or action not in ('begin','chunk','commit') then return jsonb_build_object('error','invalid_body'); end if;
  if backfill and (action<>'begin' or source<>'codex' or bf is null or (p_request->>'cutoff') is null or bf>=(p_request->>'cutoff')::timestamptz) then return jsonb_build_object('error','invalid_body'); end if;
  perform pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(uid::text,0));
  select * into src from private.collector_sources where user_id=uid and device_id=dev;
  if action='begin' then
    if src.user_id is not null and (src.harness<>source or src.cutoff<>(p_request->>'cutoff')::timestamptz) then return jsonb_build_object('error','cutoff_conflict'); end if;
    if src.user_id is not null and ((src.backfill_from is not null) <> backfill) then return jsonb_build_object('error','mode_conflict'); end if;
    if src.user_id is not null and backfill and src.backfill_from<>bf then return jsonb_build_object('error','window_conflict'); end if;
    insert into private.collector_sources(user_id,device_id,harness,cutoff,backfill_from) values(uid,dev,source,(p_request->>'cutoff')::timestamptz,case when backfill then bf end) on conflict do nothing;
    delete from private.contribution_snapshots where user_id=uid and committed_at is null and created_at<now()-interval '7 days';
    select * into snap from private.contribution_snapshots where user_id=uid and device_id=dev and snapshot_id=sid;
    if snap.user_id is not null and (snap.harness<>source or snap.chunk_count<>(p_request->>'chunk_count')::integer) then return jsonb_build_object('error','snapshot_conflict'); end if;
    if snap.user_id is null then
      if (select count(*) from private.contribution_snapshots where user_id=uid and committed_at is null)>=4 then return jsonb_build_object('error','staging_limit'); end if;
      insert into private.contribution_snapshots(user_id,device_id,harness,snapshot_id,chunk_count) values(uid,dev,source,sid,(p_request->>'chunk_count')::integer);
    end if;
  else
    if src.user_id is null or src.harness<>source then return jsonb_build_object('error','snapshot_conflict'); end if;
    select * into snap from private.contribution_snapshots where user_id=uid and device_id=dev and snapshot_id=sid;
    if snap.user_id is null or snap.harness<>source then return jsonb_build_object('error','snapshot_conflict'); end if;
    if action='chunk' then
      if (p_request->>'index')::integer not between 0 and snap.chunk_count-1 then return jsonb_build_object('error','invalid_index'); end if;
      payload := p_request->'observations';
      retired := coalesce(p_request->'superseded_canonical_ids','[]'::jsonb);
      aliases := coalesce(p_request->'identity_aliases','[]'::jsonb);
      if jsonb_typeof(payload)<>'array' or jsonb_typeof(retired)<>'array' or jsonb_typeof(aliases)<>'array' or jsonb_array_length(payload)+jsonb_array_length(retired)+jsonb_array_length(aliases) not between 1 and 500 then return jsonb_build_object('error','invalid_body'); end if;
      payload_digest := encode(extensions.digest(jsonb_build_array(payload,retired,aliases)::text,'sha256'),'hex');
      select digest into previous_digest from private.contribution_chunks where user_id=uid and device_id=dev and snapshot_id=sid and chunk_index=(p_request->>'index')::integer;
      if previous_digest is not null and previous_digest<>payload_digest then return jsonb_build_object('error','snapshot_conflict'); end if;
      if previous_digest is null then
        if snap.committed_at is not null then return jsonb_build_object('error','snapshot_conflict'); end if;
        insert into private.contribution_chunks(user_id,device_id,snapshot_id,chunk_index,digest,observations,superseded_canonical_ids,identity_aliases) values(uid,dev,sid,(p_request->>'index')::integer,payload_digest,payload,retired,aliases);
      end if;
    else
      if snap.committed_at is not null then return jsonb_build_object('ok',true,'action',action,'snapshot_id',sid,'accepted',snap.accepted,'committed',true); end if;
      if snap.sequence<src.published_sequence then return jsonb_build_object('error','stale_snapshot'); end if;
      if (select count(*) from private.contribution_chunks where user_id=uid and device_id=dev and snapshot_id=sid)<>snap.chunk_count then return jsonb_build_object('error','incomplete_snapshot'); end if;
      select count(*),count(distinct o->>'canonical_id') into total,distinct_total from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid;
      if total<>distinct_total then return jsonb_build_object('error','duplicate_identity'); end if;
      if src.backfill_from is not null then
        if exists(select 1 from private.contribution_chunks c where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and (jsonb_array_length(c.superseded_canonical_ids)>0 or jsonb_array_length(c.identity_aliases)>0)) then return jsonb_build_object('error','backfill_rejects_corrections'); end if;
        if exists(
          select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
          where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and (
            o->>'grain'<>'request' or o->>'identity_strength'<>'response' or o->>'quality'<>'observed'
            or (o->>'interval_start' is not null or o->>'interval_end' is not null)
            or (o->>'timestamp')::timestamptz < src.backfill_from or (o->>'timestamp')::timestamptz >= src.cutoff
            or o->>'input_tokens' is null or o->>'output_tokens' is null or o->>'cache_read_tokens' is null or o->>'cache_write_tokens' is null
            or coalesce((o->>'call_count')::bigint,-1)<>1
            or ((o->>'input_tokens')::bigint+(o->>'output_tokens')::bigint+(o->>'cache_read_tokens')::bigint+(o->>'cache_write_tokens')::bigint)<=0
            or o->>'cost_usd' is not null or o->>'cost_basis'<>'unknown'
            or o->>'session_id' is null or o->>'session_id'=''
          )
        ) then return jsonb_build_object('error','backfill_window_violation'); end if;
        with ins as (
          insert into public.collector_contributions as accepted(user_id,harness,canonical_id,device_id,observation,superseded,ts,partition,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,call_count,cost_usd)
          select uid,source,o->>'canonical_id',dev,o,false,(o->>'timestamp')::timestamptz,'history',
            (o->>'input_tokens')::bigint,(o->>'output_tokens')::bigint,(o->>'cache_read_tokens')::bigint,(o->>'cache_write_tokens')::bigint,
            (o->>'call_count')::bigint,(o->>'cost_usd')::numeric
          from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
          cross join lateral (select date_trunc('day',((o->>'timestamp')::timestamptz at time zone 'utc')) as utc_day) d
          where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid
            and not exists(select 1 from public.collector_contributions existing where existing.user_id=uid and existing.harness=source and existing.canonical_id=o->>'canonical_id')
            and not exists(select 1 from private.contribution_retirements r where r.user_id=uid and r.harness=source and r.canonical_id=o->>'canonical_id')
            and not exists(
              select 1 from public.collector_contributions rc
              where rc.user_id=uid and rc.harness=source and not rc.superseded and rc.observation->>'quality'='reconstructed'
                and rc.ts >= (d.utc_day at time zone 'utc')
                and rc.ts < ((d.utc_day+interval '1 day') at time zone 'utc')
            )
          on conflict do nothing
          returning 1
        ) select count(*)::int into fresh from ins;
        update private.collector_sources set published_sequence=snap.sequence where user_id=uid and device_id=dev;
        update private.contribution_snapshots set committed_at=now(),accepted=fresh where user_id=uid and device_id=dev and snapshot_id=sid;
        update private.contribution_chunks set observations=null,superseded_canonical_ids='[]'::jsonb,identity_aliases='[]'::jsonb where user_id=uid and device_id=dev and snapshot_id=sid;
        return jsonb_build_object('ok',true,'action',action,'snapshot_id',sid,'accepted',fresh,'committed',true);
      end if;
      select o->>'canonical_id' into conflict_id
      from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
      join public.collector_contributions accepted on accepted.user_id=uid and accepted.harness=source and accepted.canonical_id=o->>'canonical_id' and accepted.device_id<>dev
      left join private.collector_sources owner on owner.user_id=accepted.user_id and owner.device_id=accepted.device_id
      where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and owner.backfill_from is null limit 1;
      if conflict_id is not null then return jsonb_build_object('error','ownership_conflict','canonical_id',conflict_id); end if;
      select o->>'canonical_id' into conflict_id from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o join private.contribution_retirements r on r.user_id=uid and r.harness=source and r.canonical_id=o->>'canonical_id' and r.device_id<>dev where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid limit 1;
      if conflict_id is not null then return jsonb_build_object('error','ownership_conflict','canonical_id',conflict_id); end if;
      if exists(
        select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements_text(c.superseded_canonical_ids) retired_id
        join private.contribution_retirements r on r.user_id=uid and r.harness=source and r.canonical_id=retired_id
        where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and r.device_id<>dev
      ) or exists(
        select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
        join private.contribution_retirements r on r.user_id=uid and r.harness=source and r.canonical_id=o->>'canonical_id'
        where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and o->>'grain'<>'reconstructed_interval'
      ) then return jsonb_build_object('error','supersession_conflict'); end if;
      if exists(
        select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements_text(c.superseded_canonical_ids) retired_id
        join public.collector_contributions accepted on accepted.user_id=uid and accepted.harness=source and accepted.canonical_id=retired_id
        where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and (accepted.device_id<>dev or accepted.observation->>'grain'<>'reconstructed_interval')
      ) then return jsonb_build_object('error','supersession_conflict'); end if;
      if exists(
        select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
        join private.contribution_chunks retired_chunks on retired_chunks.user_id=uid and retired_chunks.device_id=dev and retired_chunks.snapshot_id=sid
        where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and retired_chunks.superseded_canonical_ids ? (o->>'canonical_id')
      ) then return jsonb_build_object('error','supersession_conflict'); end if;
      if exists(select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.identity_aliases) a where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid group by a->>'from' having count(*)>1) then return jsonb_build_object('error','alias_conflict'); end if;
      if exists(
        select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.identity_aliases) a
        left join public.collector_contributions old on old.user_id=uid and old.harness=source and old.canonical_id=a->>'from'
        where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and (
          a->>'from'=a->>'to' or (old.user_id is not null and (old.device_id<>dev or old.observation->>'identity_strength'<>'request' or (old.alias_to is not null and old.alias_to<>a->>'to')))
          or not exists(select 1 from private.contribution_chunks target cross join lateral jsonb_array_elements(target.observations) o where target.user_id=uid and target.device_id=dev and target.snapshot_id=sid and o->>'canonical_id'=a->>'to' and o->>'identity_strength'='response')
          or exists(select 1 from private.contribution_chunks target cross join lateral jsonb_array_elements(target.observations) o where target.user_id=uid and target.device_id=dev and target.snapshot_id=sid and o->>'canonical_id'=a->>'from')
          or exists(select 1 from private.contribution_chunks target cross join lateral jsonb_array_elements(target.identity_aliases) other where target.user_id=uid and target.device_id=dev and target.snapshot_id=sid and other->>'from'=a->>'to')
          or exists(select 1 from private.contribution_chunks target where target.user_id=uid and target.device_id=dev and target.snapshot_id=sid and (target.superseded_canonical_ids ? (a->>'from') or target.superseded_canonical_ids ? (a->>'to')))
          or exists(select 1 from private.contribution_retirements r where r.user_id=uid and r.harness=source and r.canonical_id in (a->>'from',a->>'to'))
          or exists(select 1 from public.collector_contributions prior where prior.user_id=uid and prior.harness=source and prior.canonical_id=a->>'to' and prior.superseded)
        )
      ) then return jsonb_build_object('error','alias_conflict'); end if;
      if exists(select 1 from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and ((o->>'interval_start')::timestamptz<src.cutoff and (o->>'interval_end')::timestamptz>=src.cutoff)) then return jsonb_build_object('error','unattributable_interval'); end if;
      insert into private.contribution_retirements(user_id,harness,canonical_id,device_id)
      select distinct uid,source,retired_id,dev from private.contribution_chunks c cross join lateral jsonb_array_elements_text(c.superseded_canonical_ids) retired_id where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid
      on conflict(user_id,harness,canonical_id) do nothing;
      insert into public.collector_contributions as accepted(user_id,harness,canonical_id,device_id,observation,superseded,ts,partition,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,call_count,cost_usd)
      select uid,source,o->>'canonical_id',dev,o,exists(select 1 from private.contribution_retirements r where r.user_id=uid and r.harness=source and r.canonical_id=o->>'canonical_id'),(o->>'timestamp')::timestamptz,case when (o->>'timestamp')::timestamptz<src.cutoff then 'history' else 'live' end,(o->>'input_tokens')::bigint,(o->>'output_tokens')::bigint,(o->>'cache_read_tokens')::bigint,(o->>'cache_write_tokens')::bigint,(o->>'call_count')::bigint,(o->>'cost_usd')::numeric
      from private.contribution_chunks c cross join lateral jsonb_array_elements(c.observations) o
      where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid
        and not exists(
          select 1 from public.collector_contributions prior
          join private.collector_sources owner on owner.user_id=prior.user_id and owner.device_id=prior.device_id
          where prior.user_id=uid and prior.harness=source and prior.canonical_id=o->>'canonical_id'
            and prior.device_id<>dev and owner.backfill_from is not null
        )
      on conflict(user_id,harness,canonical_id) do update set observation=excluded.observation,superseded=accepted.superseded or excluded.superseded,ts=excluded.ts,partition=excluded.partition,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_write_tokens=excluded.cache_write_tokens,call_count=excluded.call_count,cost_usd=excluded.cost_usd;
      update public.collector_contributions accepted set superseded=true
      from private.contribution_chunks c cross join lateral jsonb_array_elements_text(c.superseded_canonical_ids) retired_id
      where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid and accepted.user_id=uid and accepted.harness=source and accepted.device_id=dev and accepted.canonical_id=retired_id;
      -- An alias can retire our request identity without changing the accepted response.
      insert into public.collector_contributions as old(user_id,harness,canonical_id,device_id,observation,superseded,alias_to,ts,partition,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,call_count,cost_usd)
      select distinct uid,source,a->>'from',dev,jsonb_set(jsonb_set(target.observation,'{canonical_id}',a->'from'),'{identity_strength}',to_jsonb('request'::text)),true,a->>'to',target.ts,target.partition,target.input_tokens,target.output_tokens,target.cache_read_tokens,target.cache_write_tokens,target.call_count,target.cost_usd
      from private.contribution_chunks c cross join lateral jsonb_array_elements(c.identity_aliases) a
      join public.collector_contributions target on target.user_id=uid and target.harness=source and target.canonical_id=a->>'to'
      where c.user_id=uid and c.device_id=dev and c.snapshot_id=sid
      on conflict(user_id,harness,canonical_id) do update set superseded=true,alias_to=excluded.alias_to;
      update private.collector_sources set published_sequence=snap.sequence where user_id=uid and device_id=dev;
      update private.contribution_snapshots set committed_at=now(),accepted=total where user_id=uid and device_id=dev and snapshot_id=sid;
      update private.contribution_chunks set observations=null,superseded_canonical_ids='[]'::jsonb,identity_aliases='[]'::jsonb where user_id=uid and device_id=dev and snapshot_id=sid;
      return jsonb_build_object('ok',true,'action',action,'snapshot_id',sid,'accepted',total,'committed',true);
    end if;
  end if;
  return jsonb_build_object('ok',true,'action',action,'snapshot_id',sid);
end;
$$;
revoke all on function public.publish_contributions(text,jsonb) from public,anon,authenticated;
grant execute on function public.publish_contributions(text,jsonb) to service_role;
