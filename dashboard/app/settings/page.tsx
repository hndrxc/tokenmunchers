import { Nav } from "@/components/Nav";
import { getMe } from "@/lib/me";
import { createClient } from "@/lib/supabase/server";
import type { ApiKey } from "@/lib/types";

import { NotMember } from "../NotMember";
import { SettingsClient } from "./SettingsClient";

export const dynamic = "force-dynamic";

export default async function SettingsPage() {
  const supabase = await createClient();
  const { profile: me } = await getMe(supabase);
  if (!me) return <NotMember />;

  const { data: keys } = await supabase
    .from("api_keys")
    .select("id,label,key_prefix,created_at,last_used_at,revoked_at")
    .order("created_at", { ascending: false });

  return (
    <>
      <Nav handle={me.handle} />
      <SettingsClient profile={me} keys={(keys ?? []) as ApiKey[]} />
    </>
  );
}
