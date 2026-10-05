"use client";

import { useState } from "react";

import type { Profile } from "@/lib/types";

export function Avatar({ profile, size = 28 }: { profile: Pick<Profile, "display_name" | "avatar_url"> | undefined; size?: number }) {
  const [broken, setBroken] = useState(false);
  const name = profile?.display_name ?? "?";
  if (profile?.avatar_url && !broken) {
    return (
      // eslint-disable-next-line @next/next/no-img-element
      <img
        className="avatar"
        src={profile.avatar_url}
        alt=""
        width={size}
        height={size}
        style={{ width: size, height: size }}
        onError={() => setBroken(true)}
      />
    );
  }
  return (
    <span className="avatar" aria-hidden style={{ width: size, height: size, fontSize: size * 0.42 }}>
      {name.slice(0, 1).toUpperCase()}
    </span>
  );
}
