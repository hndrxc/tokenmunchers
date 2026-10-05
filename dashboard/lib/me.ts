import type { SupabaseClient } from "@supabase/supabase-js";

import type { Profile } from "./types";

/** The signed-in user's id (verified via getClaims) and profile, if they're a member. */
export async function getMe(supabase: SupabaseClient): Promise<{ userId: string | null; profile: Profile | null }> {
  const { data } = await supabase.auth.getClaims();
  const userId = data?.claims?.sub ?? null;
  if (!userId) return { userId: null, profile: null };
  const { data: profile } = await supabase
    .from("profiles")
    .select("id,handle,display_name,avatar_url")
    .eq("id", userId)
    .maybeSingle();
  return { userId, profile };
}
