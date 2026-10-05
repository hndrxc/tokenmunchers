"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { ago } from "@/lib/format";
import { createClient } from "@/lib/supabase/client";
import type { ApiKey, Profile } from "@/lib/types";

export function SettingsClient({ profile, keys }: { profile: Profile; keys: ApiKey[] }) {
  const router = useRouter();
  const [label, setLabel] = useState("");
  const [newKey, setNewKey] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState(profile.display_name);
  const [saved, setSaved] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const now = Date.now();

  const run = async (fn: () => Promise<{ error: { message: string } | null }>) => {
    setBusy(true);
    setError(null);
    const { error } = await fn();
    setBusy(false);
    if (error) setError(error.message);
    return !error;
  };

  const createKey = async () => {
    const supabase = createClient();
    setBusy(true);
    setError(null);
    const { data, error } = await supabase.rpc("create_api_key", { p_label: label.trim() || "omp" });
    setBusy(false);
    if (error) return setError(error.message);
    setNewKey(data as string);
    setCopied(false);
    setLabel("");
    router.refresh();
  };

  const revoke = async (id: string) => {
    if (!window.confirm("Revoke this key? Any machine using it stops reporting.")) return;
    if (await run(async () => createClient().from("api_keys").update({ revoked_at: new Date().toISOString() }).eq("id", id))) router.refresh();
  };

  const saveName = async () => {
    if (await run(async () => createClient().from("profiles").update({ display_name: name.trim() }).eq("id", profile.id))) {
      setSaved(true);
      router.refresh();
    }
  };

  const deleteAccount = async () => {
    const supabase = createClient();
    if (await run(async () => supabase.rpc("delete_my_account"))) {
      await supabase.auth.signOut();
      window.location.href = "/login";
    }
  };

  const copy = async () => {
    if (!newKey) return;
    await navigator.clipboard.writeText(newKey);
    setCopied(true);
  };

  const active = keys.filter((k) => !k.revoked_at);
  const revoked = keys.filter((k) => k.revoked_at);

  return (
    <div className="stack" style={{ maxWidth: 760 }}>
      {error && <p className="notice error">{error}</p>}

      <section className="card">
        <div className="card-head">
          <h2>Connect Oh My Pi</h2>
        </div>
        <ol style={{ margin: 0, paddingLeft: 20, display: "grid", gap: 10 }}>
          <li>
            Install the plugin:
            <pre>omp plugin install omp-tokenmunchers</pre>
          </li>
          <li>Create an API key below. It is shown once.</li>
          <li>
            In OMP, run <code>/munch login</code> and paste the key. <code>/munch status</code> shows queue and connection state;{" "}
            <code>/munch pause</code> stops reporting for private or client work.
          </li>
        </ol>
        <p className="sub" style={{ marginBottom: 0 }}>
          Only provider, model, token counts, cost, timestamps, a session id and a subagent flag are sent. Never prompts, responses,
          code, file paths, repo names or tool output; the server rejects any other field.
        </p>
      </section>

      <section className="card">
        <div className="card-head">
          <h2>API keys</h2>
          <span className="sub">one per machine is handy</span>
        </div>
        {newKey && (
          <div className="notice" style={{ marginBottom: 12 }}>
            <strong>Copy this key now. You won&apos;t see it again.</strong>
            <div className="secret" style={{ margin: "8px 0" }}>
              {newKey}
            </div>
            <div className="row">
              <button className="btn" onClick={copy}>
                {copied ? "Copied" : "Copy"}
              </button>
              <span className="sub">
                Then in OMP: <code>/munch login {newKey.slice(0, 10)}…</code>
              </span>
            </div>
          </div>
        )}
        <div className="row" style={{ marginBottom: 12 }}>
          <input
            className="input"
            placeholder="Label, e.g. laptop"
            value={label}
            maxLength={64}
            onChange={(e) => setLabel(e.target.value)}
            aria-label="Key label"
          />
          <button className="btn primary" onClick={createKey} disabled={busy}>
            Create key
          </button>
        </div>
        {active.length === 0 ? (
          <p className="empty">No active keys.</p>
        ) : (
          <table className="lb">
            <thead>
              <tr>
                <th>Label</th>
                <th>Key</th>
                <th>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {active.map((k) => (
                <tr key={k.id}>
                  <td>{k.label}</td>
                  <td>
                    <code>{k.key_prefix}…</code>
                  </td>
                  <td className="sub">{k.last_used_at ? ago(k.last_used_at, now) : "never"}</td>
                  <td className="num">
                    <button className="btn danger" onClick={() => revoke(k.id)} disabled={busy}>
                      Revoke
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {revoked.length > 0 && <p className="sub">{revoked.length} revoked key(s) hidden.</p>}
      </section>

      <section className="card">
        <div className="card-head">
          <h2>Profile</h2>
          <span className="sub">@{profile.handle}</span>
        </div>
        <div className="row">
          <input
            className="input"
            value={name}
            maxLength={64}
            onChange={(e) => {
              setName(e.target.value);
              setSaved(false);
            }}
            aria-label="Display name"
          />
          <button className="btn" onClick={saveName} disabled={busy || !name.trim() || name === profile.display_name}>
            {saved ? "Saved" : "Save name"}
          </button>
        </div>
      </section>

      <section className="card">
        <div className="card-head">
          <h2>Delete account</h2>
        </div>
        <p className="sub" style={{ marginTop: 0 }}>
          Removes your profile, keys and every usage event, permanently. Type <code>{profile.handle}</code> to confirm.
        </p>
        <div className="row">
          <input className="input" value={confirm} onChange={(e) => setConfirm(e.target.value)} aria-label="Confirm handle" />
          <button className="btn danger" onClick={deleteAccount} disabled={busy || confirm !== profile.handle}>
            Delete everything
          </button>
        </div>
      </section>
    </div>
  );
}
