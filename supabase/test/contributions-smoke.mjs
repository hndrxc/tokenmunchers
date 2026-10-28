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
  const dailyParts = (await db.query('select has_unknown_tokens,has_unknown_calls,has_unknown_cost from public.usage_daily_all where user_id=$1',[uid])).rows;
  assert.ok(dailyParts.some(r=>r.has_unknown_cost));
  // Preserve OMP's exact-today versus past-rollup split alongside collector rows.
  await db.query("insert into public.usage_events(event_id,user_id,session_id,ts,provider,model,input_tokens) values($1,$2,'omp-smoke',now(),'omp','omp-model',7)",[randomUUID(),uid]);
  await db.query("insert into public.usage_daily(user_id,day,provider,model,is_subagent,call_count,input_tokens,total_tokens) values($1,(now() at time zone 'utc')::date,'omp','omp-model',false,1,7,7),($1,(now() at time zone 'utc')::date-1,'omp','omp-model',false,1,5,5)",[uid]);
  await db.query("insert into public.usage_history(user_id,device_id,day,provider,model,is_subagent,call_count,input_tokens) values($1,$2,'2025-12-31','omp','omp-model',false,1,9)",[uid,randomUUID()]);
  assert.equal(Number((await totals()).total_tokens),501,'today raw OMP events and their rollup count once');
  await publish([
    observation('estimated-subagent',{timestamp:new Date().toISOString(),input_tokens:10,output_tokens:1,is_subagent:true,cost_usd:0.5,cost_basis:'estimated'}),
    observation('unknown-attribution',{timestamp:new Date().toISOString(),input_tokens:0,output_tokens:0,is_subagent:null,cost_usd:0,cost_basis:'reported'}),
  ]);
  assert.equal(Number((await totals()).total_tokens),512);
  assert.equal((await totals()).has_estimated_cost,true);
  assert.equal((await totals()).subagent_tokens,null,'unknown attribution cannot create an exact main-only total');
  // Member reads work; no client writes or access to staged payloads/RPC.
  await db.exec(`set role authenticated; set request.jwt.claim.sub='${uid}';`);
  assert.equal((await db.query('select * from public.leaderboard_all_time()')).rows.length,1);
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
  assert.equal((await db.query("select has_table_privilege('tokenmunchers_discord_reader','public.collector_contributions','select') readable")).rows[0].readable,false);
  await db.exec('reset role;');
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
  console.log('PASS staged atomicity/replay/cleanup/staging limit/cutoff immutability/ownership/null rollups/supersession/aliases/stale snapshots/RLS grants/bot null semantics/rate-limit reset timing/revocation');
} finally { await db.close(); }
