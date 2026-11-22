import assert from 'node:assert/strict';
import { test } from 'node:test';
import { addEvent, addNullable, byUser, combineSpend, dailyByUser, emptySpend, emptyTotals, metricOf, normalizeSpend, tokenAttributionNote } from '../lib/stats.ts';
import { spendNote, tokens, usd } from '../lib/format.ts';
test('unknown values survive normalization, aggregate additions and live OMP updates',()=>{
  const unknown={...emptyTotals('user'),total_tokens:null,output_tokens:null,cost_usd:null,call_count:null,unpriced_tokens:null,has_unknown_cost:true,has_reconstructed:true};
  assert.equal(byUser([unknown]).user.cost_usd,null);
  const event={user_id:'user',input_tokens:2,output_tokens:3,cache_read_tokens:4,cache_write_tokens:5,total_tokens:14,cost_usd:1,is_subagent:false};
  const updated=addEvent({user:unknown},event).user;
  for(const field of ['total_tokens','output_tokens','cost_usd','call_count']) assert.equal(updated[field],null);
  assert.equal(updated.input_tokens,2);
  assert.equal(updated.known_cost_usd,1);
  assert.equal(updated.unpriced_tokens,null);
  assert.equal(updated.has_unknown_cost,true);
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
test('spend normalization and mixing preserve priced zero, all-unpriced activity and coverage',()=>{
  const unpriced=normalizeSpend({known_cost_usd:null,unpriced_tokens:'1500',has_unknown_cost:true});
  const priced=normalizeSpend({known_cost_usd:'2.5',unpriced_tokens:'0',has_estimated_cost:true});
  assert.equal(unpriced.known_cost_usd,null);
  assert.equal(unpriced.unpriced_tokens,1500);
  assert.equal(combineSpend(emptySpend(),unpriced).known_cost_usd,null);
  assert.equal(usd(unpriced.known_cost_usd),'Unknown');
  assert.equal(usd(combineSpend(unpriced,{...emptySpend(),known_cost_usd:0}).known_cost_usd),'$0');
  const mixed=combineSpend(unpriced,priced);
  assert.deepEqual(mixed,{known_cost_usd:2.5,unpriced_tokens:1500,has_unknown_cost:true,has_estimated_cost:true});
  assert.equal(spendNote(mixed),'1.5k tokens unpriced · includes estimates');
  const unavailable=combineSpend(mixed,{...unpriced,unpriced_tokens:null});
  assert.equal(unavailable.unpriced_tokens,null);
  assert.equal(spendNote(unavailable),'some usage unpriced · includes estimates');
  assert.equal(spendNote(emptySpend()),'');
  assert.equal(metricOf(undefined,'cost',true),0);
});
test('per-user and realtime accounting show known spend without changing strict totals',()=>{
  const row={...emptyTotals('user'),call_count:'1',total_tokens:'100',cost_usd:null,known_cost_usd:'2.5',unpriced_tokens:'100',has_unknown_cost:true};
  const users=byUser([row]);
  assert.equal(users.user.call_count,1);
  assert.equal(users.user.total_tokens,100);
  assert.equal(users.user.cost_usd,null);
  assert.equal(metricOf(users.user,'cost',true),2.5);
  const event={user_id:'user',input_tokens:1,output_tokens:2,cache_read_tokens:0,cache_write_tokens:0,total_tokens:3,cost_usd:'1.25',is_subagent:false};
  const updated=addEvent(users,event).user;
  assert.equal(updated.cost_usd,null);
  assert.equal(updated.unpriced_tokens,100);
  assert.equal(metricOf(updated,'cost',true),3.75);
  const zero=addEvent({}, {...event,cost_usd:0}).user;
  assert.equal(zero.cost_usd,0);
  assert.equal(metricOf(zero,'cost',true),0);
});

test('mixed attribution shows only a floored subagent share or a known-main annotation',()=>{
  const mixed=byUser([{
    ...emptyTotals('user'),total_tokens:'1000',subagent_tokens:null,
    known_subagent_tokens:'129',known_main_tokens:'400',
  }]).user;
  assert.equal(mixed.known_subagent_tokens,129);
  assert.equal(mixed.known_main_tokens,400);
  assert.equal(metricOf(mixed,'tokens',true),1000);
  assert.equal(metricOf(mixed,'tokens',false),null);
  assert.deepEqual(tokenAttributionNote(mixed,true),{kind:'lower-bound',percent:'≥12%'});
  assert.deepEqual(tokenAttributionNote(mixed,false),{kind:'known-main',tokens:400});
  assert.deepEqual(tokenAttributionNote({...mixed,known_subagent_tokens:290},true),{kind:'lower-bound',percent:'≥29%'});
  assert.deepEqual(tokenAttributionNote({...mixed,known_subagent_tokens:9},true),{kind:'unknown-share',tokens:9});
  assert.deepEqual(tokenAttributionNote({...mixed,known_subagent_tokens:0},true),{kind:'unknown-share',tokens:null});
  const unknownTotal={...mixed,total_tokens:null};
  assert.equal(metricOf(unknownTotal,'tokens',true),null);
  assert.equal(metricOf(unknownTotal,'tokens',false),null);
  assert.deepEqual(tokenAttributionNote(unknownTotal,true),{kind:'unknown-share',tokens:129});
  assert.deepEqual(tokenAttributionNote(unknownTotal,false),{kind:'known-main',tokens:400});
  assert.equal(tokenAttributionNote({...mixed,known_main_tokens:0},false),null);
});

test('exact attribution retains rounding and hides known zero without coverage fields',()=>{
  const exact={...emptyTotals('user'),total_tokens:1000,known_subagent_tokens:null,known_main_tokens:null};
  assert.equal(tokenAttributionNote(exact,true),null);
  assert.equal(tokenAttributionNote(undefined,true),null);
  const subagents={...exact,subagent_tokens:129};
  assert.equal(metricOf(subagents,'tokens',false),871);
  assert.equal(tokenAttributionNote(subagents,false),null);
  assert.deepEqual(tokenAttributionNote(subagents,true),{kind:'exact-share',percent:'13%'});
  assert.deepEqual(tokenAttributionNote({...subagents,subagent_tokens:9},true),{kind:'exact-share',percent:'<1%'});
  assert.deepEqual(tokenAttributionNote({...subagents,total_tokens:null},true),{kind:'unknown-share',tokens:129});
});

test('live classified events add known counts but cannot recover old snapshot coverage',()=>{
  const subagent={user_id:'user',input_tokens:10,output_tokens:10,cache_read_tokens:0,cache_write_tokens:0,total_tokens:20,cost_usd:1,is_subagent:true};
  const main={...subagent,is_subagent:false};
  const base={...emptyTotals('user'),total_tokens:100,subagent_tokens:null};
  const {known_subagent_tokens: _subagent,known_main_tokens: _main,...legacy}=base;
  for(const row of [legacy,{...legacy,known_subagent_tokens:null,known_main_tokens:null}]){
    const updated=addEvent(addEvent(byUser([row]),subagent),main).user;
    assert.equal(updated.known_subagent_tokens,null);
    assert.equal(updated.known_main_tokens,null);
    assert.deepEqual(tokenAttributionNote(updated,true),{kind:'unknown-share',tokens:null});
    assert.equal(tokenAttributionNote(updated,false),null);
  }
  const covered=byUser([{...base,known_subagent_tokens:'10',known_main_tokens:'15'}]);
  const updated=addEvent(addEvent(covered,subagent),main).user;
  assert.equal(updated.total_tokens,140);
  assert.equal(updated.known_subagent_tokens,30);
  assert.equal(updated.known_main_tokens,35);
  assert.equal(metricOf(updated,'tokens',false),null);
  assert.deepEqual(tokenAttributionNote(updated,true),{kind:'lower-bound',percent:'≥21%'});
  const first=addEvent({},main).user;
  assert.equal(first.known_subagent_tokens,0);
  assert.equal(first.known_main_tokens,20);
  assert.equal(metricOf(first,'tokens',false),20);
});
