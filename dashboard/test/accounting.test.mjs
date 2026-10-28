import assert from 'node:assert/strict';
import { test } from 'node:test';
import { addEvent, addNullable, byUser, dailyByUser, emptyTotals, metricOf } from '../lib/stats.ts';
import { tokens, usd } from '../lib/format.ts';
test('unknown values survive normalization, aggregate additions and live OMP updates',()=>{
  const unknown={...emptyTotals('user'),total_tokens:null,output_tokens:null,cost_usd:null,call_count:null,has_reconstructed:true};
  assert.equal(byUser([unknown]).user.cost_usd,null);
  const event={user_id:'user',input_tokens:2,output_tokens:3,cache_read_tokens:4,cache_write_tokens:5,total_tokens:14,cost_usd:1,is_subagent:false};
  const updated=addEvent({user:unknown},event).user;
  for(const field of ['total_tokens','output_tokens','cost_usd','call_count']) assert.equal(updated[field],null);
  assert.equal(updated.input_tokens,2);
  assert.equal(updated.has_reconstructed,true);
  assert.equal(addNullable(null,0),null);
  assert.equal(addNullable(0,null),null);
  assert.equal(metricOf(unknown,'tokens',true),null);
  assert.equal(metricOf({...unknown,total_tokens:100,subagent_tokens:null},'tokens',false),null);
  assert.equal(metricOf(undefined,'tokens',true),0);
});
test('daily aggregation propagates missing counts and unknown main/subagent attribution',()=>{
  const base={user_id:'user',day:'2026-01-01',is_subagent:false,total_tokens:100};
  const all=dailyByUser([base,{...base,total_tokens:null}],{},true,'2026-01-03');
  assert.equal(all.user['2026-01-01'],null);
  const main=dailyByUser([{...base,is_subagent:null}],{},false,'2026-01-03');
  assert.equal(main.user['2026-01-01'],null);
  assert.equal(dailyByUser([base],{},true,'2026-01-03').user['2026-01-01'],100);
});
test('unknown formatters never display zero while genuine zero stays numeric',()=>{
  assert.equal(tokens(null),'Unknown'); assert.equal(usd(null),'Unknown');
  assert.equal(tokens(0),'0'); assert.equal(usd(0),'$0');
});
