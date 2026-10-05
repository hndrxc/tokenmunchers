import { NextResponse, type NextRequest } from "next/server";

import { createClient } from "@/lib/supabase/server";

export async function GET(request: NextRequest) {
  const { searchParams, origin } = request.nextUrl;

  // Supabase sends the user back with an error when the allowlist trigger
  // rejects the sign-up ("Database error saving new user").
  const authError = searchParams.get("error_description") ?? searchParams.get("error");
  if (authError) {
    const reason = /database error/i.test(authError) ? "not_invited" : "auth_failed";
    return NextResponse.redirect(`${origin}/login?error=${reason}`);
  }

  const code = searchParams.get("code");
  if (code) {
    const supabase = await createClient();
    const { error } = await supabase.auth.exchangeCodeForSession(code);
    if (!error) return NextResponse.redirect(`${origin}/`);
  }
  return NextResponse.redirect(`${origin}/login?error=auth_failed`);
}
