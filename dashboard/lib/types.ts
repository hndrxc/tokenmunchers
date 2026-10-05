export interface Profile {
  id: string;
  handle: string;
  display_name: string;
  avatar_url: string | null;
}

/** One row of leaderboard / leaderboard_since / leaderboard_all_time. */
export interface Totals {
  user_id: string;
  call_count: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  subagent_tokens: number;
  cost_usd: number;
}

export interface UsageEvent {
  event_id: string;
  user_id: string;
  session_id: string;
  ts: string;
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cost_usd: number;
  is_subagent: boolean;
}

export interface LiveSession {
  user_id: string;
  session_id: string;
  provider: string | null;
  model: string | null;
  started_at: string;
  last_seen: string;
}

export interface DailyRow {
  user_id: string;
  day: string;
  provider: string;
  model: string;
  is_subagent: boolean;
  call_count: number;
  total_tokens: number;
  cost_usd: number;
}

export interface ApiKey {
  id: string;
  label: string;
  key_prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked_at: string | null;
}

export const EVENT_COLUMNS =
  "event_id,user_id,session_id,ts,provider,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,total_tokens,cost_usd,is_subagent";
