// Parent-run, real PostgreSQL/WASM integration coverage, with only Supabase auth/cron stubs.
// Usage: npm install --prefix /tmp/tm-db @electric-sql/pglite
// PGLITE_MODULE_DIR=/tmp/tm-db/node_modules node supabase/test/contributions-smoke.mjs
import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createHash, randomUUID } from 'node:crypto';
const modules = process.env.PGLITE_MODULE_DIR;
const { PGlite } = await import(modules ? pathToFileURL(resolve(modules, '@electric-sql/pglite/dist/index.js')).href : '@electric-sql/pglite');
const { pgcrypto } = await import(modules ? pathToFileURL(resolve(modules, '@electric-sql/pglite/dist/contrib/pgcrypto.js')).href : '@electric-sql/pglite/contrib/pgcrypto');
const db = new PGlite({ extensions: { pgcrypto } });
try {
  await db.exec(`
    create role anon; create role authenticated; create role service_role bypassrls;
    create schema extensions; create schema auth; create schema cron;
    create table auth.users(id uuid primary key,email text,raw_user_meta_data jsonb default '{}');
    create function auth.uid() returns uuid language sql stable as $$select nullif(current_setting('request.jwt.claim.sub',true),'')::uuid$$;
    grant usage on schema auth to authenticated;
    create function cron.schedule(text,text,text) returns bigint language sql as $$select 1::bigint$$;
    create function cron.unschedule(text) returns boolean language sql as $$select true$$;
    create publication supabase_realtime;
  `);
  const migrations = new URL('../migrations/', import.meta.url);
  for (const name of (await readdir(migrations)).filter(n => n.endsWith('.sql')).sort()) {
    let sql = await readFile(new URL(name, migrations), 'utf8');
    // PGlite has no scheduler daemon. Its pgcrypto implementation is real, not stubbed.
    sql = sql.replace('create extension if not exists pg_cron with schema pg_catalog;', '');
    await db.exec(`begin;\n${sql}\ncommit;`);
    console.log(`Applied ${name}`);
  }
  const uid = randomUUID(), device = randomUUID(), otherDevice = randomUUID();
  const cutoff = '2026-01-02T00:00:00.000Z';
  const hash = createHash('sha256').update('synthetic-test-key').digest('hex');
  await db.query("insert into public.allowlist(email) values('accounting-smoke@example.invalid')");
  await db.query("insert into auth.users(id,email) values($1,'accounting-smoke@example.invalid')", [uid]);
  await db.query("insert into public.api_keys(user_id,key_hash,key_prefix) values($1,$2,'test')", [uid,hash]);
  const observation = (id, overrides = {}) => ({ harness:'codex',canonical_id:id,identity_strength:'response',grain:'request',timestamp:'2026-01-01T12:00:00.000Z',interval_start:null,interval_end:null,provider:'unknown',model:'test-model',session_id:null,parent_session_id:null,is_subagent:false,input_tokens:100,output_tokens:20,cache_read_tokens:0,cache_write_tokens:0,call_count:1,cost_usd:null,cost_basis:'unknown',quality:'observed',...overrides });
  async function request(action, sid, extra = {}, dev = device) {
    const { rows } = await db.query('select public.publish_contributions($1,$2::jsonb) result',[hash, JSON.stringify({ action,device_id:dev,harness:'codex',snapshot_id:sid,...extra })]);
    return rows[0].result;
  }
  async function publish(rows, extra = {}) {
    const sid = randomUUID();
    assert.equal((await request('begin',sid,{cutoff,chunk_count:1})).ok,true);
    assert.equal((await request('chunk',sid,{index:0,observations:rows,...extra})).ok,true);
    const result = await request('commit',sid);
    assert.equal(result.ok,true,JSON.stringify(result));
    return sid;
  }
  async function totals() { return (await db.query('select * from public.leaderboard_all_time() where user_id=$1',[uid])).rows[0]; }
  const first = await publish([observation('response-a')]);
  assert.equal((await request('commit',first)).accepted,1);
  assert.equal((await request('chunk',first,{index:0,observations:[observation('response-a')]})).ok,true);
  assert.equal((await request('chunk',first,{index:0,observations:[observation('changed')]})).error,'snapshot_conflict');
  assert.equal(Number((await totals()).total_tokens),120);
  assert.equal((await totals()).cost_usd,null);
  const interrupted = randomUUID();
  await request('begin',interrupted,{cutoff,chunk_count:2});
  await request('chunk',interrupted,{index:0,observations:[observation('response-b')]});
  assert.equal((await request('commit',interrupted)).error,'incomplete_snapshot');
  assert.equal(Number((await totals()).total_tokens),120,'uncommitted chunks must not publish');
  await request('chunk',interrupted,{index:1,observations:[observation('response-c',{timestamp:cutoff})]});
  assert.equal((await request('commit',interrupted)).accepted,2);
  assert.equal(Number((await totals()).total_tokens),360);
  const partitions = (await db.query('select canonical_id,partition from public.collector_contributions where user_id=$1 order by canonical_id',[uid])).rows;
  assert.deepEqual(partitions.map(r=>r.partition),['history','history','live']);
  const empty = randomUUID();
  await request('begin',empty,{cutoff,chunk_count:0});
  assert.equal((await request('commit',empty)).accepted,0);
  assert.equal(Number((await totals()).total_tokens),360,'cleanup must not delete accepted rows');
  const copy = randomUUID();
  await request('begin',copy,{cutoff,chunk_count:1},otherDevice);
  await request('chunk',copy,{index:0,observations:[observation('response-a')]},otherDevice);
  assert.equal((await request('commit',copy,{},otherDevice)).error,'ownership_conflict');
  assert.equal(Number((await totals()).total_tokens),360);
  assert.equal((await request('begin',randomUUID(),{cutoff:'2026-01-03T00:00:00.000Z',chunk_count:0})).error,'cutoff_conflict');
  assert.equal((await request('begin',randomUUID(),{cutoff,chunk_count:0,harness:'claude-code'})).error,'cutoff_conflict','a device source cannot switch harness');
  assert.equal(new Date((await db.query('select cutoff from private.collector_sources where device_id=$1',[device])).rows[0].cutoff).toISOString(),cutoff,'rejected begins leave the registered cutoff immutable');
  assert.equal((await db.query('select count(*)::int n from private.contribution_chunks c join private.contribution_snapshots s using(user_id,device_id,snapshot_id) where s.committed_at is not null and c.observations is not null')).rows[0].n,0,'committed chunk payloads are cleared; only digests remain for replay');
  const weak = observation('legacy',{identity_strength:'scoped',grain:'reconstructed_interval',quality:'reconstructed',call_count:null,output_tokens:null,interval_start:'2026-01-01T11:00:00.000Z',interval_end:'2026-01-01T12:00:00.000Z'});
  await publish([weak]);
  assert.equal((await totals()).total_tokens,null);
  assert.equal((await totals()).call_count,null);
  assert.equal((await totals()).has_reconstructed,true);
  assert.equal(Number((await totals()).known_main_tokens),360,'classified rows with missing token totals do not erase measured main tokens');
  const daily = (await db.query('select total_tokens,call_count,cost_usd from public.usage_daily_all where user_id=$1',[uid])).rows;
  assert.ok(daily.some(r=>r.total_tokens===null && r.call_count===null && r.cost_usd===null));
  await publish([],{superseded_canonical_ids:['legacy']});
  assert.equal(Number((await totals()).total_tokens),360);
  await publish([weak]);
  assert.equal(Number((await totals()).total_tokens),360,'superseded intervals cannot resurrect');
  const fallback = observation('request-fallback',{identity_strength:'request'});
  await publish([fallback]);
  await publish([observation('response-upgraded')],{identity_aliases:[{from:'request-fallback',to:'response-upgraded'}]});
  assert.equal(Number((await totals()).total_tokens),480,'alias upgrade must count once');
  await publish([fallback]);
  assert.equal(Number((await totals()).total_tokens),480,'late fallback cannot resurrect');
  const stale = randomUUID();
  await request('begin',stale,{cutoff,chunk_count:0});
  await publish([observation('response-a')]);
  assert.equal((await request('commit',stale)).error,'stale_snapshot');
  const reject = randomUUID();
  await request('begin',reject,{cutoff,chunk_count:1});
  await request('chunk',reject,{index:0,observations:[],superseded_canonical_ids:['response-a']});
  assert.equal((await request('commit',reject)).error,'supersession_conflict');
  assert.equal(Number((await totals()).total_tokens),480);
  // Start a fresh rate-limit minute for the remaining independent scenarios.
  await db.query('delete from private.rate_limits');
  await db.query("update private.contribution_snapshots set created_at=now()-interval '8 days' where committed_at is null");
  await publish([],{superseded_canonical_ids:['unpublished-legacy']});
  assert.equal((await db.query("select count(*)::int n from public.collector_contributions where canonical_id='unpublished-legacy'")).rows[0].n,0,'retirement must not fabricate an observation');
  await publish([{...weak,canonical_id:'unpublished-legacy'}]);
  assert.equal(Number((await totals()).total_tokens),480,'retirement before initial delivery must prevent resurrection');
  const retiredCopy = randomUUID();
  await request('begin',retiredCopy,{cutoff,chunk_count:1},otherDevice);
  await request('chunk',retiredCopy,{index:0,observations:[{...weak,canonical_id:'unpublished-legacy'}]},otherDevice);
  assert.equal((await request('commit',retiredCopy,{},otherDevice)).error,'ownership_conflict');
  const retiredSteal = randomUUID();
  await request('begin',retiredSteal,{cutoff,chunk_count:1},otherDevice);
  await request('chunk',retiredSteal,{index:0,observations:[],superseded_canonical_ids:['unpublished-legacy']},otherDevice);
  assert.equal((await request('commit',retiredSteal,{},otherDevice)).error,'supersession_conflict','another source cannot take over a retirement reservation');
  const duplicate = randomUUID();
  await request('begin',duplicate,{cutoff,chunk_count:2});
  for (const index of [0,1]) await request('chunk',duplicate,{index,observations:[observation('cross-chunk-duplicate')]});
  assert.equal((await request('commit',duplicate)).error,'duplicate_identity');
  const crossing = randomUUID();
  await request('begin',crossing,{cutoff,chunk_count:1});
  await request('chunk',crossing,{index:0,observations:[{...weak,canonical_id:'cross-cutoff',timestamp:cutoff,interval_start:'2026-01-01T23:59:59Z',interval_end:cutoff}]});
  assert.equal((await request('commit',crossing)).error,'unattributable_interval');
  assert.equal(Number((await totals()).total_tokens),480,'all rejected commits retain last-good publication');
  // At most four pending snapshots per user, across devices; this ceiling is capacity-based, not time-based.
  assert.equal((await db.query('select count(*)::int n from private.contribution_snapshots where user_id=$1 and committed_at is null',[uid])).rows[0].n,4);
  assert.equal((await request('begin',randomUUID(),{cutoff,chunk_count:1})).error,'staging_limit');
  await db.query("update private.contribution_snapshots set created_at=now()-interval '8 days' where committed_at is null");
  const aliasConflict = randomUUID();
  await request('begin',aliasConflict,{cutoff,chunk_count:1});
  assert.deepEqual((await db.query('select snapshot_id from private.contribution_snapshots where committed_at is null')).rows.map(r=>r.snapshot_id),[aliasConflict],'begin expires abandoned staging');
  assert.equal((await db.query('select count(*)::int n from private.contribution_chunks where snapshot_id in ($1,$2)',[duplicate,crossing])).rows[0].n,0,'expired staging removes its chunks');
  await request('chunk',aliasConflict,{index:0,observations:[observation('invalid-upgrade')],identity_aliases:[{from:'response-a',to:'invalid-upgrade'}]});
  assert.equal((await request('commit',aliasConflict)).error,'alias_conflict');
  assert.equal(Number((await totals()).total_tokens),480);
  const since = (await db.query('select * from public.leaderboard_since($1) where user_id=$2',[cutoff,uid])).rows[0];
  assert.equal(Number(since.total_tokens),120,'old post-cutoff recovery is not subject to OMP seven-day limit');
  assert.equal(Number(since.known_main_tokens),120,'the inclusive collector cutoff also bounds known main tokens');
  const dailyParts = (await db.query('select has_unknown_tokens,has_unknown_calls,has_unknown_cost from public.usage_daily_all where user_id=$1',[uid])).rows;
  assert.ok(dailyParts.some(r=>r.has_unknown_cost));
  // Preserve OMP's exact-today versus past-rollup split alongside collector rows.
  await db.query("insert into public.usage_events(event_id,user_id,session_id,ts,provider,model,input_tokens) values($1,$2,'omp-smoke',now(),'omp','omp-model',7)",[randomUUID(),uid]);
  await db.query("insert into public.usage_daily(user_id,day,provider,model,is_subagent,call_count,input_tokens,total_tokens) values($1,(now() at time zone 'utc')::date,'omp','omp-model',false,1,7,7),($1,(now() at time zone 'utc')::date-1,'omp','omp-model',false,1,5,5)",[uid]);
  await db.query("insert into public.usage_history(user_id,device_id,day,provider,model,is_subagent,call_count,input_tokens) values($1,$2,'2025-12-31','omp','omp-model',false,1,9)",[uid,randomUUID()]);
  assert.equal(Number((await totals()).total_tokens),501,'today raw OMP events and their rollup count once');
  // Old raw OMP rows must stay out of range even when the collector cutoff includes them.
  await db.query("insert into public.usage_events(event_id,user_id,session_id,ts,provider,model,input_tokens) values($1,$2,'omp-old-smoke',$3,'omp','omp-model',17)",[randomUUID(),uid,cutoff]);
  // Place the unclassified observation after the raw OMP event and the classified observation.
  const attributionCutoff = new Date((await db.query(
    "select date_trunc('second',now()) + interval '1 second' as ts",
  )).rows[0].ts).toISOString();
  await publish([
    observation('estimated-subagent',{timestamp:new Date(Date.parse(attributionCutoff)-1000).toISOString(),input_tokens:10,output_tokens:1,is_subagent:true,cost_usd:0.5,cost_basis:'estimated'}),
    observation('unknown-attribution',{timestamp:attributionCutoff,input_tokens:13,output_tokens:0,is_subagent:null,cost_usd:0,cost_basis:'reported'}),
  ]);
  assert.equal(Number((await totals()).total_tokens),525);
  assert.equal((await totals()).has_estimated_cost,true);
  assert.equal(Number((await totals()).subagent_tokens),11,'unclassified usage counts as main, so the main-only total stays exact');
  // Member reads work; no client writes or access to staged payloads/RPC.
  await db.exec(`set role authenticated; set request.jwt.claim.sub='${uid}';`);
  assert.equal((await db.query('select * from public.leaderboard_all_time()')).rows.length,1);
  const attributionFields = `total_tokens::int, subagent_tokens::int, known_subagent_tokens::int, known_main_tokens::int`;
  const attribution = (total_tokens,subagent_tokens,known_subagent_tokens,known_main_tokens) =>
    ({total_tokens,subagent_tokens,known_subagent_tokens,known_main_tokens});
  const attributedUsage = async (source, ...args) => (await db.query(
    `select ${attributionFields} from public.${source} where user_id=$1`, [uid,...args],
  )).rows[0];
  assert.deepEqual(await attributedUsage('leaderboard_all_time()'),attribution(525,11,11,514),
    'all-time includes history, past rollups, and today’s raw events only once');
  assert.deepEqual(await attributedUsage('leaderboard_since($2)',cutoff),attribution(151,11,11,140),
    'since includes older post-cutoff collector usage, but not older raw OMP events or rollups');
  assert.deepEqual(await attributedUsage('leaderboard()'),attribution(31,11,11,20),
    'the default week wrapper returns only in-window tokens');
  assert.deepEqual(await attributedUsage('leaderboard_since($2)',attributionCutoff),attribution(13,0,0,13),
    'unclassified tokens count as main');
  assert.deepEqual((await db.query('select is_subagent from public.recent_activity($1) where total_tokens=13',[uid])).rows,
    [{is_subagent:false}],'Activity shows an unclassified collector call as main');
  const grants = (await db.query("select has_table_privilege('authenticated','public.collector_contributions','insert') writes,has_function_privilege('authenticated','public.publish_contributions(text,jsonb)','execute') rpc")).rows[0];
  assert.equal(grants.writes,false); assert.equal(grants.rpc,false);
  assert.equal((await db.query("select has_table_privilege('authenticated','private.contribution_retirements','select') readable")).rows[0].readable,false);
  await db.exec(`set request.jwt.claim.sub='${randomUUID()}';`);
  assert.equal((await db.query('select * from public.collector_contributions')).rows.length,0,'nonmembers cannot read contributions');
  assert.equal((await db.query('select * from public.leaderboard_all_time()')).rows.length,0,'security-invoker aggregate views retain RLS');
  await db.exec('reset role;');
  await db.exec('set role tokenmunchers_discord_reader;');
  const bot = (await db.query("select * from public.discord_leaderboard('all')")).rows[0];
  assert.equal(bot.cost_usd,null,'bot wrapper must preserve unknown cost');
  assert.equal(Number(bot.subagent_tokens),11,'bot wrapper counts unclassified usage as main');
  assert.equal((await db.query("select has_table_privilege('tokenmunchers_discord_reader','public.collector_contributions','select') readable")).rows[0].readable,false);
  await db.exec('reset role;');
  // ---- Codex backfill mode (issue #4): immutable window, preserved rows, legacy overlap guard ----
  await db.query('delete from private.rate_limits');
  const bfDevice = randomUUID();
  const bfFrom = '2026-01-01T00:00:00.000Z';
  const bfDevice2 = randomUUID();
  const bfFrom2 = '2026-02-10T00:00:00.000Z';
  const bfCutoff2 = '2026-02-12T00:00:00.000Z';
  const backfillObservation = (id, overrides = {}) => ({ harness:'codex',canonical_id:id,identity_strength:'response',grain:'request',timestamp:'2026-01-01T12:00:00.000Z',interval_start:null,interval_end:null,provider:'unknown',model:'test-model',session_id:'session-bf',parent_session_id:null,is_subagent:false,input_tokens:100,output_tokens:20,cache_read_tokens:0,cache_write_tokens:0,call_count:1,cost_usd:null,cost_basis:'unknown',quality:'observed',...overrides });
  const totalTokens = async () => Number((await db.query('select coalesce(sum(total_tokens),0)::float8 s from public.collector_contributions where user_id=$1 and not superseded',[uid])).rows[0].s);
  // Old exact canonical with different metadata/partition stays untouched; new rows insert alongside.
  await publish([observation('bf-live',{timestamp:cutoff})]);
  const bfMixedBefore = await totalTokens();
  const bfMixed = randomUUID();
  await request('begin',bfMixed,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfMixed,{index:0,observations:[{...backfillObservation('bf-live',{input_tokens:888,output_tokens:888})},{...backfillObservation('bf-new')}]},bfDevice);
  assert.equal((await request('commit',bfMixed,{},bfDevice)).accepted,1,'only genuinely new rows count');
  const bfLiveRow = (await db.query('select input_tokens,output_tokens,partition from public.collector_contributions where user_id=$1 and canonical_id=$2',[uid,'bf-live'])).rows[0];
  assert.equal(Number(bfLiveRow.input_tokens),100); assert.equal(bfLiveRow.partition,'live','pre-existing exact canonical is never updated');
  const bfNewRow = (await db.query('select partition,total_tokens from public.collector_contributions where user_id=$1 and canonical_id=$2',[uid,'bf-new'])).rows[0];
  assert.equal(bfNewRow.partition,'history'); assert.equal(Number(bfNewRow.total_tokens),120);
  assert.equal(await totalTokens(),bfMixedBefore+120,'backfill inserts count exactly once');
  // Ambiguous reconstructed legacy day skips modern candidates; clean days still insert.
  await publish([observation('bf-legacy-interval',{identity_strength:'scoped',grain:'reconstructed_interval',quality:'reconstructed',call_count:null,output_tokens:null,interval_start:'2026-02-10T09:00:00.000Z',interval_end:'2026-02-10T18:00:00.000Z',timestamp:'2026-02-10T12:00:00.000Z'})]);
  const bfOverlapBefore = await totalTokens();
  const bfOverlap = randomUUID();
  await request('begin',bfOverlap,{cutoff:bfCutoff2,backfill_from:bfFrom2,chunk_count:1},bfDevice2);
  await request('chunk',bfOverlap,{index:0,observations:[backfillObservation('bf-overlap',{timestamp:'2026-02-10T15:00:00.000Z'}),backfillObservation('bf-clean',{timestamp:'2026-02-11T15:00:00.000Z'})]},bfDevice2);
  const bfOverlapResult = await request('commit',bfOverlap,{},bfDevice2);
  assert.equal(bfOverlapResult.accepted,1,'ambiguous reconstructed day skips modern candidates');
  assert.equal((await db.query('select count(*)::int n from public.collector_contributions where user_id=$1 and canonical_id in ($2,$3)',[uid,'bf-overlap','bf-clean'])).rows[0].n,1);
  assert.equal(await totalTokens(),bfOverlapBefore+120);
  assert.equal((await db.query('select superseded from public.collector_contributions where user_id=$1 and canonical_id=$2',[uid,'bf-legacy-interval'])).rows[0].superseded,false,'legacy interval rows are preserved');
  await publish([],{superseded_canonical_ids:['bf-legacy-interval']});
  assert.equal((await db.query('select superseded from public.collector_contributions where user_id=$1 and canonical_id=$2',[uid,'bf-legacy-interval'])).rows[0].superseded,true,'retired interval rows stay superseded');
  // Interrupted backfill stays invisible; a later exact replay commits and replays its receipt.
  const bfInterrupted = randomUUID();
  await request('begin',bfInterrupted,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfInterrupted,{index:0,observations:[backfillObservation('bf-replay-1'),backfillObservation('bf-replay-2')]},bfDevice);
  assert.equal((await db.query('select count(*)::int n from public.collector_contributions where user_id=$1 and canonical_id like $2',[uid,'bf-replay-%'])).rows[0].n,0,'interrupted backfill has no visibility');
  const bfReplayBefore = await totalTokens();
  const bfRerun = randomUUID();
  await request('begin',bfRerun,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfRerun,{index:0,observations:[backfillObservation('bf-replay-1'),backfillObservation('bf-replay-2')]},bfDevice);
  const rerun = await request('commit',bfRerun,{},bfDevice);
  assert.equal(rerun.accepted,2);
  assert.equal(await totalTokens(),bfReplayBefore+240);
  const replay = await request('commit',bfRerun,{},bfDevice);
  assert.equal(replay.accepted,2,'acceptance receipt is preserved on replay');
  assert.equal((await db.query('select count(*)::int n from public.collector_contributions where user_id=$1 and canonical_id like $2',[uid,'bf-replay-%'])).rows[0].n,2);
  // Strict window: from inclusive, cutoff exclusive; malformed rows reject the whole commit.
  const bfBoundaryBefore = await totalTokens();
  const bfBoundaryOk = randomUUID();
  await request('begin',bfBoundaryOk,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfBoundaryOk,{index:0,observations:[backfillObservation('bf-at-from',{timestamp:bfFrom})]},bfDevice);
  assert.equal((await request('commit',bfBoundaryOk,{},bfDevice)).accepted,1,'backfill_from is inclusive');
  const bfInvalidWindow = randomUUID();
  await request('begin',bfInvalidWindow,{cutoff,backfill_from:bfFrom,chunk_count:2},bfDevice);
  await request('chunk',bfInvalidWindow,{index:0,observations:[backfillObservation('bf-at-cutoff',{timestamp:cutoff})]},bfDevice);
  await request('chunk',bfInvalidWindow,{index:1,observations:[backfillObservation('bf-before-from',{timestamp:'2025-12-31T23:59:59.000Z'})]},bfDevice);
  assert.equal((await request('commit',bfInvalidWindow,{},bfDevice)).error,'backfill_window_violation','cutoff is exclusive and from is inclusive');
  assert.equal((await db.query('select count(*)::int n from public.collector_contributions where user_id=$1 and canonical_id in ($2,$3)',[uid,'bf-at-cutoff','bf-before-from'])).rows[0].n,0);
  assert.equal(await totalTokens(),bfBoundaryBefore+120);
  const bfShape = randomUUID();
  await request('begin',bfShape,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfShape,{index:0,observations:[backfillObservation('bf-shaped',{call_count:null})]},bfDevice);
  assert.equal((await request('commit',bfShape,{},bfDevice)).error,'backfill_window_violation');
  // Drain abandoned staging so later begins have capacity; stale snapshots are pruned at begin.
  await db.query('delete from private.rate_limits');
  await db.query("update private.contribution_snapshots set created_at=now()-interval '8 days' where committed_at is null");
  const bfDrain = randomUUID();
  await request('begin',bfDrain,{cutoff,backfill_from:bfFrom,chunk_count:0},bfDevice);
  assert.equal((await request('commit',bfDrain,{},bfDevice)).accepted,0,'stale staging prunes on begin');
  const bfCost = randomUUID();
  await request('begin',bfCost,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfCost,{index:0,observations:[backfillObservation('bf-cost',{cost_usd:0,cost_basis:'reported'})]},bfDevice);
  assert.equal((await request('commit',bfCost,{},bfDevice)).error,'backfill_window_violation','backfill cost must be unknown');
  // Backfill snapshots reject corrections: superseded identities and aliases.
  const bfCorrections = randomUUID();
  await request('begin',bfCorrections,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfCorrections,{index:0,observations:[backfillObservation('bf-no-corrections')],superseded_canonical_ids:['whatever']},bfDevice);
  assert.equal((await request('commit',bfCorrections,{},bfDevice)).error,'backfill_rejects_corrections');
  const bfAlias = randomUUID();
  await request('begin',bfAlias,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfAlias,{index:0,observations:[backfillObservation('bf-no-alias')],identity_aliases:[{from:'x',to:'bf-no-alias'}]},bfDevice);
  assert.equal((await request('commit',bfAlias,{},bfDevice)).error,'backfill_rejects_corrections');
  // Reserved retirement IDs are never resurrected by backfill.
  await publish([],{superseded_canonical_ids:['bf-retired-canonical']});
  const bfRetiredBefore = await totalTokens();
  const bfRetired = randomUUID();
  await request('begin',bfRetired,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfRetired,{index:0,observations:[backfillObservation('bf-retired-canonical'),backfillObservation('bf-adjacent')]},bfDevice);
  const bfRetiredResult = await request('commit',bfRetired,{},bfDevice);
  assert.equal(bfRetiredResult.accepted,1,'retired reservations are never resurrected');
  assert.equal((await db.query('select count(*)::int n from public.collector_contributions where user_id=$1 and canonical_id=$2',[uid,'bf-retired-canonical'])).rows[0].n,0);
  assert.equal(await totalTokens(),bfRetiredBefore+120);
  // Prune rerun: stale backfill staging is pruned like legacy staging; a rerun then succeeds.
  await db.query("update private.contribution_snapshots set created_at=now()-interval '8 days' where committed_at is null");
  const bfPrune = randomUUID();
  await request('begin',bfPrune,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await db.query("update private.contribution_snapshots set created_at=now()-interval '8 days' where committed_at is null");
  const bfPruneBefore = await totalTokens();
  const bfAfterPrune = randomUUID();
  await request('begin',bfAfterPrune,{cutoff,backfill_from:bfFrom,chunk_count:1},bfDevice);
  await request('chunk',bfAfterPrune,{index:0,observations:[backfillObservation('bf-pruned-rerun')]},bfDevice);
  assert.equal((await request('commit',bfAfterPrune,{},bfDevice)).accepted,1,'stale backfill staging is pruned and rerun works');
  assert.equal(await totalTokens(),bfPruneBefore+120);
  // Source mode + lower/upper bounds are immutable; existing sources cannot be adopted into another mode.
  assert.equal((await request('begin',randomUUID(),{cutoff,backfill_from:'2025-12-31T00:00:00.000Z',chunk_count:1},bfDevice)).error,'window_conflict');
  assert.equal((await request('begin',randomUUID(),{cutoff:'2026-01-03T00:00:00.000Z',backfill_from:bfFrom,chunk_count:1},bfDevice)).error,'cutoff_conflict');
  assert.equal((await request('begin',randomUUID(),{cutoff,chunk_count:1},bfDevice)).error,'mode_conflict','a backfill source is never demoted');
  assert.equal((await request('begin',randomUUID(),{cutoff,backfill_from:bfFrom,chunk_count:1})).error,'mode_conflict','a legacy source cannot be adopted as backfill');
  assert.equal((await request('begin',randomUUID(),{cutoff,backfill_from:cutoff,chunk_count:1},bfDevice)).error,'invalid_body','from must strictly precede cutoff');
  // Large staged snapshot: begin + 59 chunks spend one whole shared minute (60 requests x 10 of 600 hits).
  // The commit must receive accurate reset timing, publish nothing while limited, and succeed after
  // waiting exactly Retry-After seconds, resuming from the failed request rather than chunk zero.
  const sleep = ms => new Promise(done => setTimeout(done, ms));
  const windowLeft = async () => Number((await db.query("select extract(epoch from date_trunc('minute',now())+interval '1 minute'-now())::float8 s")).rows[0].s);
  // Keep the burst inside one window and bound the real wait: start 12-40 s before the reset.
  const left = await windowLeft();
  const align = left > 40 ? left - 40 : left < 12 ? left + 20 : 0;
  if (align > 0) { console.log(`Aligning to the rate window (${Math.ceil(align)} s)`); await sleep(align * 1000); }
  await db.query('delete from private.rate_limits');
  const beforeBurst = Number((await totals()).total_tokens);
  const large = randomUUID();
  assert.equal((await request('begin',large,{cutoff,chunk_count:59})).ok,true);
  for (let index = 0; index < 59; index++) assert.equal((await request('chunk',large,{index,observations:[observation(`large-${index}`)]})).ok,true,`chunk ${index} fits the shared minute`);
  const upper = Math.ceil(await windowLeft());
  const limited = await request('commit',large);
  const limitedAt = Date.now();
  const lower = Math.ceil(await windowLeft());
  assert.equal((await db.query('select count(distinct window_start)::int n from private.rate_limits')).rows[0].n,1,'burst stayed inside one rate window');
  assert.equal(limited.error,'rate_limited');
  assert.ok(Number.isInteger(limited.retry_after_seconds),'retry_after_seconds is an integer');
  assert.ok(limited.retry_after_seconds >= Math.max(1,lower) && limited.retry_after_seconds <= upper,`retry_after_seconds ${limited.retry_after_seconds} must equal the remaining window (${lower}..${upper})`);
  assert.equal((await request('chunk',large,{index:58,observations:[observation('large-58')]})).error,'rate_limited','retries inside the window stay limited; there is no bypass');
  assert.equal(Number((await totals()).total_tokens),beforeBurst,'a rate-limited commit publishes nothing');
  console.log(`Waiting Retry-After ${limited.retry_after_seconds} s for the rate window reset`);
  await sleep(Math.max(0, limited.retry_after_seconds * 1000 - (Date.now() - limitedAt)));
  const resumed = await request('commit',large);
  assert.equal(resumed.ok,true,JSON.stringify(resumed));
  assert.equal(resumed.accepted,59,'commit resumes from staged chunks without replaying chunk zero');
  assert.equal(Number((await totals()).total_tokens),beforeBurst+59*120);
  await db.query('update public.api_keys set revoked_at=now() where key_hash=$1',[hash]);
  assert.equal((await request('commit',first)).error,'invalid_key');
  console.log('PASS staged contributions, Codex backfill retention and overlap, RLS, rate-limit reset, and revocation');

  // Separate member/key after the rate-limit scenario; freeze the UTC day across the
  // raw-event and daily-rollup assertions, even if the smoke run crosses midnight.
  await db.exec('begin');
  const profile = randomUUID(), desktop = randomUUID(), event = randomUUID();
  const activityHash = createHash('sha256').update('synthetic-activity-key').digest('hex');
  await db.query("insert into public.allowlist(email) values('activity-smoke@example.invalid')");
  await db.query("insert into auth.users(id,email) values($1,'activity-smoke@example.invalid')", [profile]);
  await db.query("insert into public.api_keys(user_id,key_hash,key_prefix) values($1,$2,'test')", [profile,activityHash]);
  const timestamp = new Date((await db.query('select now() as ts')).rows[0].ts).toISOString();
  const day = timestamp.slice(0,10);
  const activityRequest = async (action, snapshot_id, extra = {}) => {
    const { rows } = await db.query('select public.publish_contributions($1,$2::jsonb) result', [
      activityHash, JSON.stringify({ action,snapshot_id,device_id:desktop,harness:'codex',...extra }),
    ]);
    assert.equal(rows[0].result.ok,true,JSON.stringify(rows[0].result));
    return rows[0].result;
  };
  const stageActivity = async (observations, extra = {}) => {
    const sid = randomUUID();
    await activityRequest('begin',sid,{cutoff:timestamp,chunk_count:1});
    await activityRequest('chunk',sid,{index:0,observations,...extra});
    return sid;
  };
  const exact = observation('activity-private-canonical',{
    timestamp,provider:'omp',model:'mixed-model',session_id:'activity-private-session',
    parent_session_id:'activity-private-parent',input_tokens:100,output_tokens:20,
    cache_read_tokens:4,cache_write_tokens:2,
  });
  const activity = async (...args) => (await db.query(
    args.length ? 'select * from public.recent_activity($1)' : 'select * from public.recent_activity()', args,
  )).rows;
  const asMember = () => db.exec(`set role authenticated; set request.jwt.claim.sub='${uid}';`);
  const spendFields = `cost_usd::float8, known_cost_usd::float8, unpriced_tokens::int,
    has_unknown_cost, has_estimated_cost`;
  const spend = (cost_usd,known_cost_usd,unpriced_tokens,has_unknown_cost,has_estimated_cost = false) =>
    ({cost_usd,known_cost_usd,unpriced_tokens,has_unknown_cost,has_estimated_cost});
  const allTimeSpend = async () => (await db.query(
    `select ${spendFields} from public.leaderboard_all_time() where user_id=$1`, [profile],
  )).rows[0];
  const sinceSpend = async () => (await db.query(
    `select ${spendFields} from public.leaderboard_since($1::date::timestamp at time zone 'utc') where user_id=$2`,
    [day,profile],
  )).rows[0];
  const weekSpend = async () => (await db.query(
    `select ${spendFields} from public.leaderboard(7) where user_id=$1`, [profile],
  )).rows[0];
  const dailySpend = async model => (await db.query(
    `select ${spendFields} from public.usage_daily_all where user_id=$1 and day=$2 and model=$3`,
    [profile,day,model],
  )).rows[0];

  const exactSnapshot = await stageActivity([exact]);
  await asMember();
  assert.deepEqual(await activity(profile),[],'staging a live request does not publish it');
  assert.equal(await allTimeSpend(),undefined,'staging is not usage and must not report measured zero');
  await db.exec('reset role');
  const ack = await activityRequest('commit',exactSnapshot);
  assert.equal(ack.committed,true);
  assert.equal(ack.accepted,1);
  await asMember();
  assert.deepEqual((await activity(profile)).map(r=>r.model),['mixed-model'],'acknowledged live request appears immediately');
  const allUnpriced = spend(null,null,126,true);
  assert.deepEqual(await allTimeSpend(),allUnpriced);
  assert.deepEqual(await sinceSpend(),allUnpriced);
  assert.deepEqual(await weekSpend(),allUnpriced);
  assert.deepEqual(await dailySpend('mixed-model'),allUnpriced);
  await db.exec('reset role');

  await db.query(`insert into public.usage_events(event_id,user_id,session_id,ts,provider,model,input_tokens,cost_usd)
    values($1,$2,'activity-private-omp-session',$3,'omp','mixed-model',7,1.25)`,[event,profile,timestamp]);
  await db.query(`insert into public.usage_daily(user_id,day,provider,model,is_subagent,call_count,input_tokens,total_tokens,cost_usd)
    values($1,$2,'omp','mixed-model',false,1,7,7,1.25)`,[profile,day]);
  const fallbackLive = observation('activity-fallback',{timestamp,model:'superseded-model',identity_strength:'request'});
  const retiredLive = {
    ...weak,canonical_id:'activity-retired',timestamp,interval_start:timestamp,interval_end:timestamp,model:'retired-model',
  };
  const seed = await stageActivity([
    observation('activity-history',{model:'history-model',cost_usd:5,cost_basis:'estimated'}),
    observation('activity-recent-history',{
      timestamp:new Date(Date.parse(timestamp)-1000).toISOString(),
      model:'recent-history-model',cost_usd:0,cost_basis:'reported',
    }),
    fallbackLive,retiredLive,
    observation('activity-scoped',{timestamp,model:'zero-model',identity_strength:'scoped',cost_usd:0,cost_basis:'reported'}),
  ]);
  assert.equal((await activityRequest('commit',seed)).accepted,5);
  const upgrade = await stageActivity([exact],{
    identity_aliases:[{from:fallbackLive.canonical_id,to:exact.canonical_id}],
    superseded_canonical_ids:[retiredLive.canonical_id],
  });
  assert.equal((await activityRequest('commit',upgrade)).accepted,1);
  await asMember();
  // Today's OMP rollup is not counted twice, and older priced collector history
  // contributes to all-time but not to either of the restricted periods.
  const mixed = spend(null,1.25,126,true);
  assert.deepEqual(await allTimeSpend(),spend(null,6.25,126,true,true));
  assert.deepEqual(await sinceSpend(),mixed);
  assert.deepEqual(await weekSpend(),mixed);
  assert.deepEqual(await dailySpend('mixed-model'),mixed);
  assert.deepEqual(await dailySpend('zero-model'),spend(0,0,0,false),'measured zero remains priced');
  await db.exec('reset role');

  const reconstructed = await stageActivity([{
    ...retiredLive,canonical_id:'activity-reconstruction',identity_strength:'request',model:'unknown-token-model',
  }]);
  assert.equal((await activityRequest('commit',reconstructed)).accepted,1);
  await stageActivity([observation('activity-staged',{timestamp,model:'staged-model'})]);

  await asMember();
  assert.deepEqual(await dailySpend('unknown-token-model'),spend(null,null,null,true));
  assert.deepEqual(await sinceSpend(),spend(null,1.25,null,true),'unknown unpriced token count propagates without erasing priced spend');
  assert.deepEqual(await weekSpend(),spend(null,1.25,null,true));
  assert.deepEqual(await allTimeSpend(),spend(null,6.25,null,true,true));
  const recent = (await activity(profile)).sort((a,b)=>a.source.localeCompare(b.source));
  assert.equal(recent.length,2,'history, staged, superseded, retired, scoped, and reconstructed rows stay out of Activity');
  const [collectorCall,ingestCall] = recent;
  const commonActivity = {user_id:profile,ts:timestamp,provider:'omp',model:'mixed-model',is_subagent:false};
  const normalizeActivity = ({id,...row}) => ({
    ...row,ts:new Date(row.ts).toISOString(),
    ...Object.fromEntries(['input_tokens','output_tokens','cache_read_tokens','cache_write_tokens','total_tokens','cost_usd']
      .map(key=>[key,row[key] === null ? null : Number(row[key])])),
  });
  assert.deepEqual(normalizeActivity(collectorCall),{
    ...commonActivity,source:'codex',input_tokens:100,output_tokens:20,cache_read_tokens:4,cache_write_tokens:2,total_tokens:126,cost_usd:null,
  });
  assert.deepEqual(normalizeActivity(ingestCall),{
    ...commonActivity,source:'ingest',input_tokens:7,output_tokens:0,cache_read_tokens:0,cache_write_tokens:0,total_tokens:7,cost_usd:1.25,
  });
  assert.equal(ingestCall.id,`ingest:${event}`);
  assert.equal(typeof collectorCall.id,'string');
  assert.ok(collectorCall.id.length > 0);
  const exposed = JSON.stringify(recent,(_key,value)=>typeof value === 'bigint' ? String(value) : value);
  for (const secret of [desktop,exactSnapshot,exact.canonical_id,exact.session_id,exact.parent_session_id,'activity-private-omp-session']) {
    assert.ok(!exposed.includes(secret),`Activity must not expose ${secret}`);
  }
  assert.equal((await activity()).filter(r=>r.user_id === profile).length,2,'unfiltered Activity includes the selected profile');
  const otherProfile = await activity(uid);
  assert.ok(otherProfile.some(r=>r.source === 'ingest' && r.model === 'omp-model'));
  assert.ok(otherProfile.every(r=>r.user_id === uid),'profile filter does not leak other profiles');
  await db.exec(`set request.jwt.claim.sub='${randomUUID()}';`);
  assert.deepEqual(await activity(),[]);
  assert.deepEqual(await activity(profile),[]);
  await db.exec('reset role; set role anon;');
  await assert.rejects(activity(profile),{code:'42501'});
  await db.exec('rollback');
  console.log('PASS source-labelled recent Activity, member/privacy boundaries, and mixed-cost coverage across daily and period aggregates');
  console.log('PASS staged atomicity/replay/cleanup/staging limit/cutoff immutability/ownership/null rollups/supersession/aliases/stale snapshots/RLS grants/bot null semantics/rate-limit reset timing/revocation');
} finally { await db.close(); }
