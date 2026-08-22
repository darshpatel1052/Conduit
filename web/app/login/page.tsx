"use client";

import Link from "next/link";
import { FormEvent, useEffect, useState } from "react";
import { useRouter } from "next/navigation";

export default function LoginPage() {
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    fetch("/api/control/session/me", { cache: "no-store" }).then((response) => {
      if (response.ok) router.replace("/dashboard");
    });
  }, [router]);

  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    const response = await fetch("/api/control/session/login", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ email, password }),
    });
    const body = await response.json(); setBusy(false);
    if (!response.ok) { setError(body.error?.message ?? "Could not sign in"); return; }
    sessionStorage.setItem("conduit_csrf", body.csrf_token);
    router.push("/dashboard"); router.refresh();
  }

  return (
    <main className="control-auth">
      <section className="control-auth-shell">
        <div className="control-auth-story">
          <Link href="/" className="control-auth-brand" aria-label="Conduit home">
            <span className="brand-mark">C</span>
            <span>Conduit</span>
          </Link>
          <div>
            <p className="control-eyebrow">LLM gateway administration</p>
            <h1>Change the model policy.<br />Keep the application stable.</h1>
            <p>Projects own their provider credentials, model profiles, limits, and usage history. Applications only need one OpenAI-compatible endpoint.</p>
          </div>
          <dl className="control-auth-flow">
            <div><dt>Application requests</dt><dd>fast</dd></div>
            <div><dt>Project policy chooses</dt><dd>primary → fallback</dd></div>
            <div><dt>Conduit records</dt><dd>cost · latency · usage</dd></div>
          </dl>
        </div>

        <form className="control-card control-login" onSubmit={submit}>
          <div><p className="control-eyebrow">Administrator access</p><h2>Sign in to your projects</h2></div>
          <p className="control-muted">There is no public registration. A platform owner bootstraps the installation and invites other administrators.</p>
          <label>Email<input required type="email" value={email} onChange={(event) => setEmail(event.target.value)} autoComplete="email" /></label>
          <label>Password<input required type="password" value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" /></label>
          {error && <p className="control-error" role="alert">{error}</p>}
          <button className="control-primary" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
          <p className="control-bootstrap-hint">First installation? Bootstrap the owner with <code>conduit admin bootstrap</code>.</p>
        </form>
      </section>
    </main>
  );
}
