"use client";

import { useState } from "react";

import { createClient } from "@/lib/supabase/client";

export function LoginButtons() {
  const [busy, setBusy] = useState<string | null>(null);

  const signIn = async (provider: "github" | "discord") => {
    setBusy(provider);
    await createClient().auth.signInWithOAuth({
      provider,
      options: { redirectTo: `${window.location.origin}/auth/callback` },
    });
  };

  return (
    <div className="stack">
      <button className="btn primary" onClick={() => signIn("github")} disabled={busy !== null}>
        {busy === "github" ? "Redirecting…" : "Continue with GitHub"}
      </button>
      <button className="btn" onClick={() => signIn("discord")} disabled={busy !== null}>
        {busy === "discord" ? "Redirecting…" : "Continue with Discord"}
      </button>
    </div>
  );
}
