import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readBody, validateRequest, MAX_BODY_BYTES } from '../functions/contributions/validation.ts';
import { publicationResponse } from '../functions/contributions/response.ts';
const common = { device_id:'11111111-1111-4111-8111-111111111111',snapshot_id:'22222222-2222-4222-8222-222222222222',harness:'codex' };
const observation = { harness:'codex',canonical_id:'response-a',identity_strength:'response',grain:'request',timestamp:'2026-01-01T12:00:00.000Z',interval_start:null,interval_end:null,provider:'unknown',model:'unknown',session_id:null,parent_session_id:null,is_subagent:null,input_tokens:100,output_tokens:null,cache_read_tokens:0,cache_write_tokens:0,call_count:null,cost_usd:null,cost_basis:'unknown',quality:'partial' };
const chunk = (overrides = {}) => ({...common,action:'chunk',index:0,observations:[{...observation,...overrides}]});
test('preserves nullable metadata and rejects content, diagnostics, paths and unknown fields',()=>{
  assert.equal(validateRequest(chunk()),null);
  for(const field of ['prompt','content','file','diagnostics','__proto__']) {
    const request=chunk();
    Object.defineProperty(request.observations[0],field,{value:'not allowed',enumerable:true});
    assert.match(validateRequest(request),/unknown field/);
  }
  assert.match(validateRequest({...chunk(),raw:'not allowed'}),/unknown field/);
});
test('does not coerce numeric strings, negative, nonfinite or unsafe values',()=>{
  for(const value of ['1',-1,Infinity,NaN,Number.MAX_SAFE_INTEGER+1]) assert.match(validateRequest(chunk({input_tokens:value})),/invalid input_tokens/);
  assert.match(validateRequest(chunk({cost_usd:0})),/cost basis mismatch/);
  assert.equal(validateRequest(chunk({cost_usd:0,cost_basis:'reported'})),null);
});
test('rejects wrong harness, summaries, invalid dates, malformed intervals and invented reconstructed calls',()=>{
  for(const row of [{harness:'claude-code'},{grain:'session_summary'},{timestamp:'2026-02-30T12:00:00Z'},{timestamp:null},{grain:'reconstructed_interval',quality:'reconstructed',call_count:1}]) assert.notEqual(validateRequest(chunk(row)),null);
  assert.notEqual(validateRequest(chunk({grain:'reconstructed_interval',quality:'reconstructed'})),null);
  assert.equal(validateRequest(chunk({grain:'reconstructed_interval',quality:'reconstructed',interval_start:'2026-01-01T11:00:00Z',interval_end:'2026-01-01T12:00:00Z'})),null);
});
test('bounds chunk entries including permanent corrections and rejects duplicate identities',()=>{
  assert.equal(validateRequest({...common,action:'begin',cutoff:'2026-01-01T00:00:00Z',chunk_count:1000}),null);
  assert.notEqual(validateRequest({...common,action:'begin',cutoff:'2026-01-01T00:00:00Z',chunk_count:1001}),null);
  assert.notEqual(validateRequest({...chunk(),observations:[observation,observation]}),null);
  assert.notEqual(validateRequest({...chunk(),superseded_canonical_ids:['response-a']}),null);
  assert.notEqual(validateRequest({...chunk(),identity_aliases:[{from:'response-a',to:'response-a'}]}),null);
  assert.equal(validateRequest({...chunk(),identity_aliases:[{from:'old-request',to:'response-a'}]}),null);
  assert.notEqual(validateRequest({...chunk(),superseded_canonical_ids:Array.from({length:500},(_,i)=>String(i))}),null);
  assert.equal(validateRequest({...chunk(),observations:[],superseded_canonical_ids:['retired']}),null);
});
test('empty snapshots require zero chunks, while every transmitted chunk has metadata or corrections',()=>{
  assert.equal(validateRequest({...common,action:'begin',cutoff:'2026-01-01T00:00:00Z',chunk_count:0}),null);
  assert.equal(validateRequest({...common,action:'commit'}),null);
  assert.notEqual(validateRequest({...chunk(),observations:[]}),null);
  const rows=Array.from({length:500},(_,index)=>({...observation,canonical_id:`response-${index}`}));
  assert.equal(validateRequest({...chunk(),observations:rows}),null);
  assert.notEqual(validateRequest({...chunk(),observations:[...rows,{...observation,canonical_id:'overflow'}]}),null);
});
test('intervals cannot reverse, cross UTC days, or attribute a different recorded day',()=>{
  for(const [start,end] of [
    ['2026-01-01T12:00:01Z','2026-01-01T12:00:00Z'],
    ['2026-01-01T23:59:59Z','2026-01-02T00:00:00Z'],
    ['2026-01-02T11:00:00Z','2026-01-02T12:00:00Z'],
  ]) assert.notEqual(validateRequest(chunk({grain:'reconstructed_interval',quality:'reconstructed',interval_start:start,interval_end:end})),null);
  const missing=chunk(); delete missing.observations[0].output_tokens;
  assert.notEqual(validateRequest(missing),null,'unavailable is explicit null, not an omitted schema field');
});
test('enforces UTF-8 byte limits before parsing, including streamed bodies',async()=>{
  assert.equal(await readBody(new Request('https://example.invalid',{method:'POST',body:'é'.repeat(MAX_BODY_BYTES/2+1)})),null);
  const encoder=new TextEncoder();
  const body=new ReadableStream({start(controller){controller.enqueue(encoder.encode('a'.repeat(MAX_BODY_BYTES)));controller.enqueue(encoder.encode('b'));controller.close();}});
  assert.equal(await readBody(new Request('https://example.invalid',{method:'POST',body,duplex:'half'})),null);
  assert.equal(await readBody(new Request('https://example.invalid',{method:'POST',body:'a'.repeat(MAX_BODY_BYTES)})),'a'.repeat(MAX_BODY_BYTES));
  await assert.rejects(readBody(new Request('https://example.invalid',{method:'POST',body:new Uint8Array([0xff])})),TypeError);
});
test('rate-limited publication exposes reset-aware Retry-After in header and body; other limits do not',async()=>{
  const limited=publicationResponse({error:'rate_limited',retry_after_seconds:17});
  assert.equal(limited.status,429);
  assert.equal(limited.headers.get('retry-after'),'17');
  assert.deepEqual(await limited.json(),{error:'rate_limited',retry_after_seconds:17});
  for(const wait of [0,61,1.5,'17',null,undefined]) assert.equal(publicationResponse({error:'rate_limited',retry_after_seconds:wait}).headers.get('retry-after'),null,`malformed wait ${wait} is not forwarded`);
  const staging=publicationResponse({error:'staging_limit'});
  assert.equal(staging.status,429);
  assert.equal(staging.headers.get('retry-after'),null,'staging capacity is not time-based');
  for(const [error,status] of [['invalid_key',401],['ownership_conflict',409],['cutoff_conflict',409],['stale_snapshot',409],['incomplete_snapshot',400],['invalid_index',400]]) assert.equal(publicationResponse({error}).status,status,error);
  assert.equal(publicationResponse({ok:true,action:'commit'}).status,200);
});
