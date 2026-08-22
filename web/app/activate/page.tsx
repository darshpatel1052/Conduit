"use client";

import { FormEvent, useState } from "react";
import { useRouter } from "next/navigation";

export default function ActivatePage() {
  const router=useRouter();const [error,setError]=useState("");const [busy,setBusy]=useState(false);
  async function submit(event:FormEvent<HTMLFormElement>){event.preventDefault();setBusy(true);const data=new FormData(event.currentTarget);const response=await fetch("/api/control/invitations/activate",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({token:data.get("token"),display_name:data.get("display_name"),password:data.get("password")})});const body=await response.json();setBusy(false);if(!response.ok){setError(body.error?.message??"Activation failed");return}router.push("/login")}
  return <main className="control-auth"><form className="control-card control-login" onSubmit={submit}><span className="brand-mark">C</span><div><p className="control-eyebrow">Administrator invitation</p><h1>Activate your account</h1></div><p className="control-muted">Your account is local to this Conduit installation. The invitation can be used once.</p><label>Invitation token<input name="token" required/></label><label>Display name<input name="display_name" required autoComplete="name"/></label><label>Password<input name="password" required type="password" minLength={12} autoComplete="new-password"/></label>{error&&<p className="control-error">{error}</p>}<button className="control-primary" disabled={busy}>{busy?"Activating…":"Activate account"}</button></form></main>
}
