import assert from 'node:assert/strict';
import { test } from 'node:test';
import { activityFromIngest, mergeActivity } from '../lib/activity.ts';

const ingestEvent = {
  event_id: 'abc',
  user_id: 'user',
  session_id: 'session',
  ts: '2026-01-01T00:00:00Z',
  provider: 'openai',
  model: 'model',
  input_tokens: 1,
  output_tokens: 2,
  cache_read_tokens: 3,
  cache_write_tokens: 4,
  total_tokens: 10,
  cost_usd: 0,
  is_subagent: false,
};

function collectorCall(id, source, ts) {
  return {
    id, source, ts,
    user_id: 'user',
    provider: 'provider',
    model: 'model',
    input_tokens: null,
    output_tokens: null,
    cache_read_tokens: null,
    cache_write_tokens: null,
    total_tokens: null,
    cost_usd: null,
    is_subagent: false,
  };
}

test('realtime ingest uses the server ID and keeps zero cost and false attribution', () => {
  const { event_id, session_id, ...fields } = ingestEvent;
  assert.deepEqual(activityFromIngest(ingestEvent), { ...fields, id: `ingest:${event_id}`, source: 'ingest' });
});

test('merge keeps distinct sources and nullable metadata; the server wins ingest overlaps', () => {
  const realtime = activityFromIngest(ingestEvent);
  const committed = { ...realtime, cost_usd: 0.25 };
  const claudeCode = collectorCall('claude-code:abc', 'claude-code', '2026-01-01T01:00:00.000001Z');
  const codex = collectorCall('codex:abc', 'codex', '2026-01-01T00:59:59Z');
  const liveOnly = activityFromIngest({ ...ingestEvent, event_id: 'new', ts: '2026-01-01T02:00:00Z' });
  const merged = mergeActivity([codex, committed, claudeCode], [realtime, liveOnly]);
  assert.deepEqual(merged, [liveOnly, claudeCode, codex, committed]);
});

test('ordering uses timestamp precision and ascending ID for equal instants', () => {
  const first = collectorCall('codex:z', 'codex', '2026-01-01T01:00:00.100002+01:00');
  const second = collectorCall('codex:a', 'codex', '2026-01-01T00:00:00.100001Z');
  const third = collectorCall('codex:b', 'codex', '2026-01-01T01:00:00.1000010+01:00');
  assert.deepEqual(mergeActivity([third, second, first], []), [first, second, third]);
  assert.deepEqual(mergeActivity([second, third], [first]), [first, second, third]);
});

test('cap applies after merging, deduplicating, and ordering', () => {
  const calls = Array.from({ length: 51 }, (_, n) => activityFromIngest({
    ...ingestEvent,
    event_id: String(n),
    ts: new Date(Date.UTC(2026, 0, 1, 0, 0, n)).toISOString(),
  }));
  assert.deepEqual(mergeActivity(calls.slice(0, 50), calls.slice(49)), calls.slice(1).reverse());
});
