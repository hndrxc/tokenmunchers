import { LoginButtons } from "./LoginButtons";

export default async function Login({ searchParams }: { searchParams: Promise<{ error?: string }> }) {
  const { error } = await searchParams;
  return (
    <div className="login">
      <h1>
        token<span style={{ color: "var(--accent)" }}>munchers</span>
      </h1>
      <p className="sub">Who&apos;s burning the most tokens this week. Invite-only.</p>
      {error && (
        <p className="notice error" style={{ textAlign: "left", marginTop: 20 }}>
          {error === "not_invited"
            ? "That account isn't on the invite list. Ask to be added, then try again."
            : "Sign-in failed. Try again."}
        </p>
      )}
      <LoginButtons />
    </div>
  );
}
