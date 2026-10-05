"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { createClient } from "@/lib/supabase/client";
import { subscribeAuthed } from "@/lib/supabase/realtime";

/** Re-renders the server page (debounced) when this user logs a new call. */
export function LiveRefresh({ userId }: { userId: string }) {
  const router = useRouter();
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const unsubscribe = subscribeAuthed(createClient(), (supabase) =>
      supabase
        .channel(`profile-${userId}`)
        .on("postgres_changes", { event: "INSERT", schema: "public", table: "usage_events", filter: `user_id=eq.${userId}` }, () => {
          clearTimeout(timer);
          timer = setTimeout(() => router.refresh(), 1500);
        })
        .subscribe(),
    );
    return () => {
      clearTimeout(timer);
      unsubscribe();
    };
  }, [userId, router]);
  return null;
}
