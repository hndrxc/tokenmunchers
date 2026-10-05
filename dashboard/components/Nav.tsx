import Link from "next/link";

export function Nav({ handle }: { handle?: string }) {
  return (
    <nav className="nav">
      <Link href="/" className="brand">
        token<span>munchers</span>
      </Link>
      <div className="nav-links">
        <Link href="/">Leaderboard</Link>
        {handle && <Link href={`/u/${handle}`}>Profile</Link>}
        <Link href="/settings">Settings</Link>
        <form action="/auth/signout" method="post">
          <button className="linklike" type="submit">
            Sign out
          </button>
        </form>
      </div>
    </nav>
  );
}
