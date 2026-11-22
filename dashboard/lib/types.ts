export interface Profile {
  id: string;
  handle: string;
  display_name: string;
  avatar_url: string | null;
}

export interface Spend {
  known_cost_usd: number | null;
  unpriced_tokens: number | null;
  has_unknown_cost?: boolean;
  has_estimated_cost?: boolean;
}

export interface AccountingQuality extends Pick<Spend, "has_unknown_cost" | "has_estimated_cost"> {
  has_unknown_tokens?: boolean;
  has_unknown_calls?: boolean;
  has_reconstructed?: boolean;
}

/** One row of leaderboard / leaderboard_since / leaderboard_all_time. */
export interface Totals extends AccountingQuality, Spend {
  user_id: string;
  call_count: number | null;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  total_tokens: number | null;
  subagent_tokens: number | null;
  // Optional during rollout; null means coverage for the snapshot is unavailable.
  known_subagent_tokens?: number | null;
  known_main_tokens?: number | null;
  cost_usd: number | null;
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

export interface DailyRow extends AccountingQuality, Spend {
  user_id: string;
  day: string;
  provider: string;
  model: string;
  is_subagent: boolean | null;
  call_count: number | null;
  total_tokens: number | null;
  cost_usd: number | null;
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
