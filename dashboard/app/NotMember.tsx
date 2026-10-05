export function NotMember() {
  return (
    <div className="login">
      <h1>Not on the list</h1>
      <p className="sub">This account isn&apos;t on the tokenmunchers allowlist. Ask whoever runs it to add your email.</p>
      <form action="/auth/signout" method="post" style={{ marginTop: 20 }}>
        <button className="btn" type="submit">
          Sign out
        </button>
      </form>
    </div>
  );
}
