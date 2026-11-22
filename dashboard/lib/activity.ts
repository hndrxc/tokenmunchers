import type { UsageEvent } from "./types";

export interface ActivityCall {
  id: string;
  source: "ingest" | "claude-code" | "codex";
  user_id: string;
  ts: string;
  provider: string;
  model: string;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  total_tokens: number | null;
  cost_usd: number | null;
  is_subagent: boolean | null;
}

export function activityFromIngest(event: UsageEvent): ActivityCall {
  return {
    id: `ingest:${event.event_id}`,
    source: "ingest",
    user_id: event.user_id,
    ts: event.ts,
    provider: event.provider,
    model: event.model,
    input_tokens: event.input_tokens,
    output_tokens: event.output_tokens,
    cache_read_tokens: event.cache_read_tokens,
    cache_write_tokens: event.cache_write_tokens,
    total_tokens: event.total_tokens,
    cost_usd: event.cost_usd,
    is_subagent: event.is_subagent,
  };
}

export function mergeActivity(authoritative: ActivityCall[], realtime: ActivityCall[]): ActivityCall[] {
  const unique = new Map<string, ActivityCall>();
  for (const call of realtime) unique.set(call.id, call);
  for (const call of authoritative) unique.set(call.id, call);
  return [...unique.values()]
    .map((call) => ({
      call,
      millis: Date.parse(call.ts),
      // Postgres timestamps can have finer precision than Date.parse preserves.
      subMillis: (call.ts.match(/:\d{2}\.\d{3}(\d+)/)?.[1] ?? "").replace(/0+$/, ""),
    }))
    .sort((a, b) => b.millis - a.millis
      || (a.subMillis < b.subMillis ? 1 : a.subMillis > b.subMillis ? -1 : 0)
      || (a.call.id < b.call.id ? -1 : a.call.id > b.call.id ? 1 : 0))
    .slice(0, 50)
    .map(({ call }) => call);
}
