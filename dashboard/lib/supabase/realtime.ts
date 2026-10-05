import type { RealtimeChannel, SupabaseClient } from "@supabase/supabase-js";

/**
 * Joins a Realtime channel only after the socket carries the user's JWT.
 * The browser client restores its session from cookies asynchronously; a
 * channel joined before that is treated as anon, and RLS then turns every
 * postgres_changes event into "401 Unauthorized". Returns a cleanup function.
 */
export function subscribeAuthed(supabase: SupabaseClient, build: (supabase: SupabaseClient) => RealtimeChannel): () => void {
  let channel: RealtimeChannel | undefined;
  let cancelled = false;
  void (async () => {
    const { data } = await supabase.auth.getSession();
    if (cancelled) return;
    await supabase.realtime.setAuth(data.session?.access_token ?? null);
    if (cancelled) return;
    channel = build(supabase);
  })();
  return () => {
    cancelled = true;
    if (channel) void supabase.removeChannel(channel);
  };
}
