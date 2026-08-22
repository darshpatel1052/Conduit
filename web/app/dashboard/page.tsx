"use client";

import Link from "next/link";
import { FormEvent, useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";

type User = { id: string; email: string; display_name: string; platform_owner: boolean };
type Project = { id: string; slug: string; name: string; description: string; status: string; role: string; configuration_version: number };
type Provider = { id: string; name: string; type: string; base_url: string; has_credential: boolean; credential_fingerprint: string };
type ProviderTest = { provider_id: string; ok: boolean; status_code?: number; latency_ms: number; message: string };
type Target = { provider_connection_id: string; provider_name: string; upstream_model_id: string; priority: number; weight: number };
type Profile = { id: string; name: string; description: string; kind: "generation" | "embedding"; enabled: boolean; strategy: string; max_attempts: number; targets: Target[] };
type APIKey = { id: string; label: string; environment: string; prefix: string; allowed_profiles: string[]; revoked_at?: string; last_used_at?: string };
type Policy = { project_id: string; requests_per_minute: number; tokens_per_minute: number; require_end_user: boolean; cache_mode: string; cache_ttl_seconds: number; semantic_threshold: number; maximum_cacheable_temperature: number; analytics_retention_days: number; budget_warning_percent: number; monthly_budget_usd?: number; end_user_requests_per_minute?: number };
type Audit = { id: string; actor_name: string; action: string; resource_type: string; created_at: string };
type Member = { admin_id: string; display_name: string; email: string; role: string };
type UsageOverview = { requests: number; errors: number; p95_ms: number; prompt_tokens: number; completion_tokens: number; cost_usd: number; cache_hit_rate: number };
type UsageRequest = { timestamp: string; request_id: string; api_key_id: string; environment: string; end_user_pseudonym: string; operation: string; profile: string; upstream_model: string; provider: string; status: number; stream: boolean; attempts: number; duration_ms: number; prompt_tokens: number; completion_tokens: number; cost_usd: number; cache_status: string; failover: boolean; error_kind: string };

const sections = ["overview", "profiles", "providers", "keys", "policies", "usage", "members", "audit"] as const;
type Section = typeof sections[number];

async function control<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body) headers.set("Content-Type", "application/json");
  const csrf = sessionStorage.getItem("conduit_csrf");
  if (csrf && init?.method && init.method !== "GET") headers.set("X-CSRF-Token", csrf);
  const response = await fetch(`/api/control${path}`, { ...init, headers, cache: "no-store" });
  const body = response.status === 204 ? null : await response.json();
  if (!response.ok) throw new Error(body?.error?.message ?? `Request failed (${response.status})`);
  return body as T;
}

export default function Dashboard() {
  const router = useRouter();
  const [user, setUser] = useState<User>();
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectID, setProjectID] = useState("");
  const [section, setSection] = useState<Section>("overview");
  const [providers, setProviders] = useState<Provider[]>([]);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [policy, setPolicy] = useState<Policy>();
  const [audit, setAudit] = useState<Audit[]>([]);
  const [members, setMembers] = useState<Member[]>([]);
  const [usage, setUsage] = useState<UsageOverview>();
  const [requests, setRequests] = useState<UsageRequest[]>([]);
  const [usageFilter, setUsageFilter] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [revealedKey, setRevealedKey] = useState("");

  const loadProject = useCallback(async (id: string) => {
    if (!id) return;
    try {
      const [providerRows, profileRows, keyRows, policyRow, auditRows, memberRows] = await Promise.all([
        control<Provider[]>(`/projects/${id}/providers`), control<Profile[]>(`/projects/${id}/profiles`),
        control<APIKey[]>(`/projects/${id}/keys`), control<Policy>(`/projects/${id}/policy`),
        control<Audit[]>(`/projects/${id}/audit?limit=50`), control<Member[]>(`/projects/${id}/members`),
      ]);
      setProviders(providerRows); setProfiles(profileRows); setKeys(keyRows); setPolicy(policyRow); setAudit(auditRows); setMembers(memberRows); setUsageFilter(""); setError("");
      try {
        const [usageRow, requestRows] = await Promise.all([control<UsageOverview>(`/projects/${id}/usage/overview?window=24h`), control<UsageRequest[]>(`/projects/${id}/usage/requests?limit=50`)]);
        setUsage(usageRow); setRequests(requestRows);
      } catch { setUsage(undefined); setRequests([]); }
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }, []);

  useEffect(() => {
    Promise.all([control<{ user: User; csrf_token: string }>("/session/me"), control<Project[]>("/projects")])
      .then(([session, rows]) => { setUser(session.user); sessionStorage.setItem("conduit_csrf", session.csrf_token); setProjects(rows); if (rows[0]) setProjectID(rows[0].id); })
      .catch(() => router.replace("/login"));
  }, [router]);
  useEffect(() => {
    const timer = window.setTimeout(() => void loadProject(projectID), 0);
    return () => window.clearTimeout(timer);
  }, [projectID, loadProject]);

  async function mutate(action: () => Promise<unknown>, success: string) {
    setError(""); setMessage("");
    try { await action(); setMessage(success); await loadProject(projectID); }
    catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  async function createProject(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const data = new FormData(event.currentTarget);
    try {
      const created = await control<Project>("/projects", { method: "POST", body: JSON.stringify({ slug: data.get("slug"), name: data.get("name"), description: data.get("description") }) });
      const rows = await control<Project[]>("/projects"); setProjects(rows); setProjectID(created.id); setMessage("Project created."); event.currentTarget.reset();
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  async function logout() {
    await control("/session/logout", { method: "POST" }); sessionStorage.removeItem("conduit_csrf"); router.replace("/login");
  }

  async function testProvider(id: string) {
    setError(""); setMessage("Testing provider connection…");
    try {
      const result = await control<ProviderTest>(`/projects/${projectID}/providers/${id}/test`, { method: "POST" });
      setMessage(`${result.ok ? "Connection succeeded" : "Connection failed"} in ${result.latency_ms} ms${result.status_code ? ` (HTTP ${result.status_code})` : ""}.`);
      await loadProject(projectID);
    } catch (cause) { setMessage(""); setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  async function updateProject(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const data = new FormData(event.currentTarget);
    try {
      await control(`/projects/${projectID}`, { method: "PUT", body: JSON.stringify({ name: data.get("name"), description: data.get("description") }) });
      setProjects(await control<Project[]>("/projects")); setMessage("Project details updated."); await loadProject(projectID);
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  async function archiveProject() {
    if (!window.confirm("Archive this project? Its application keys will stop routing inference immediately.")) return;
    try {
      await control(`/projects/${projectID}/archive`, { method: "POST" });
      setProjects(await control<Project[]>("/projects")); setMessage("Project archived. Inference is now disabled for its keys.");
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  async function searchEndUser(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const data = new FormData(event.currentTarget); const raw = String(data.get("end_user") ?? "");
    try {
      const rows = await control<UsageRequest[]>(`/projects/${projectID}/usage/end-users/search?limit=100`, { method: "POST", body: JSON.stringify({ end_user: raw }) });
      setRequests(rows); setUsageFilter(raw); setMessage(rows.length ? `Showing usage for the supplied end-user ID.` : "No usage matched that end-user ID.");
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }

  const project = projects.find((item) => item.id === projectID);
  if (!user) return <main className="control-loading">Opening the dashboard…</main>;

  return (
    <main className="control-shell">
      <aside className="control-sidebar">
        <Link href="/" className="control-brand"><span className="brand-mark">C</span><span>Conduit<small>admin dashboard</small></span></Link>
        <label className="control-project-picker">Project<select value={projectID} onChange={(event) => setProjectID(event.target.value)}><option value="">Choose a project</option>{projects.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
        <nav>{sections.map((item) => <button key={item} onClick={() => setSection(item)} aria-current={section === item}>{item}</button>)}</nav>
        <div className="control-user"><strong>{user.display_name}</strong><span>{user.email}</span><button onClick={logout}>Sign out</button></div>
      </aside>

      <section className="control-main">
        <header><div><p className="control-eyebrow">{project?.slug ?? "No project selected"}</p><h1>{project?.name ?? "Create your first project"}</h1>{project && <p>{project.description || "Applications, profiles, credentials, and policy share this boundary."}</p>}</div>{project && <span className="control-role">{project.role.replaceAll("_", " ")}</span>}</header>
        {error && <div className="control-notice error">{error}</div>}{message && <div className="control-notice">{message}</div>}

        {!project && <Panel title="Create a project" note="A project is the isolation boundary for credentials, keys, policy, usage, and administrators."><form className="control-form" onSubmit={createProject}><label>Name<input name="name" required placeholder="Support assistant" /></label><label>Slug<input name="slug" required placeholder="support-assistant" pattern="[a-z0-9-]+" /></label><label className="wide">Description<input name="description" placeholder="Customer support applications and their model policy" /></label><button className="control-primary">Create project</button></form></Panel>}

        {project && section === "overview" && <div className="control-grid thirds"><Metric label="Profiles" value={profiles.length} detail="stable names exposed to applications"/><Metric label="Provider connections" value={providers.length} detail={`${providers.filter((item) => item.has_credential).length} with stored credentials`}/><Metric label="Active keys" value={keys.filter((item) => !item.revoked_at).length} detail="plaintext never stored"/><Panel title="How this project routes" note="Applications request a profile. Administrators decide what that profile means."><div className="control-flow"><span>application key</span><b>→</b><span>{profiles[0]?.name ?? "profile"}</span><b>→</b><span>{profiles[0]?.targets[0]?.provider_name ?? "provider"}</span></div></Panel><Panel title="Current governance"><dl className="control-kv"><div><dt>Request limit</dt><dd>{policy?.requests_per_minute ?? "—"} / minute</dd></div><div><dt>Token limit</dt><dd>{policy?.tokens_per_minute ?? "—"} / minute</dd></div><div><dt>Attribution</dt><dd>{policy?.require_end_user ? "required" : "optional"}</dd></div><div><dt>Cache</dt><dd>{policy?.cache_mode ?? "—"}</dd></div></dl></Panel>{(project.role === "project_owner" || project.role === "platform_owner") && <Panel title="Project details" note={`Status: ${project.status}. The slug remains stable because it appears in response metadata.`}><form className="control-form" onSubmit={updateProject}><label>Name<input name="name" required defaultValue={project.name}/></label><label className="wide">Description<input name="description" defaultValue={project.description}/></label><div className="control-form-actions"><button className="control-primary">Save details</button>{project.status === "active" && <button type="button" className="control-danger" onClick={() => void archiveProject()}>Archive project</button>}</div></form></Panel>}</div>}

        {project && section === "providers" && <><Panel title="Provider connections" note="Credentials are encrypted with the deployment master key and never returned."><Table heads={["Name","Type","Endpoint","Credential","Check"]} rows={providers.map((item) => [item.name,item.type,item.base_url,item.has_credential ? `saved · ${item.credential_fingerprint}` : "none",<button className="control-action" key={item.id} onClick={() => void testProvider(item.id)}>Test connection</button>])} /></Panel><Panel title="Add provider"><form className="control-form" onSubmit={(event) => { event.preventDefault(); const d=new FormData(event.currentTarget); void mutate(() => control(`/projects/${projectID}/providers`,{method:"POST",body:JSON.stringify({name:d.get("name"),type:d.get("type"),base_url:d.get("base_url"),credential:d.get("credential"),timeout_seconds:120,breaker_policy:{}})}),"Provider saved."); event.currentTarget.reset(); }}><label>Name<input name="name" required placeholder="Primary OpenAI"/></label><label>Type<select name="type"><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="ollama">Ollama</option><option value="openai_compatible">OpenAI-compatible</option></select></label><label>Base URL<input name="base_url" required defaultValue="https://api.openai.com"/></label><label>Credential<input name="credential" type="password" autoComplete="off" placeholder="Not required for Ollama"/></label><button className="control-primary">Save provider</button></form></Panel><Panel title="Rotate a provider credential" note="The old value is replaced transactionally. Only fingerprints enter the audit log."><form className="control-form" onSubmit={(event)=>{event.preventDefault();const d=new FormData(event.currentTarget);void mutate(()=>control(`/projects/${projectID}/providers/${d.get("provider")}/credential`,{method:"PUT",body:JSON.stringify({credential:d.get("credential")})}),"Provider credential rotated.");event.currentTarget.reset()}}><label>Provider<select name="provider" required><option value="">Choose…</option>{providers.map((item)=><option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label>New credential<input name="credential" type="password" required autoComplete="new-password"/></label><button className="control-primary">Rotate credential</button></form></Panel></>}

        {project && section === "profiles" && <><Panel title="Model profiles" note="Generation profiles work with Chat Completions and Responses. Embedding profiles produce vectors."><Table heads={["Profile","Workload","Strategy","Targets","State"]} rows={profiles.map((item) => [item.name,item.kind,item.strategy,item.targets.map((target) => `${target.provider_name} / ${target.upstream_model_id}`).join(", "),item.enabled ? "enabled" : "disabled"])} /></Panel><Panel title="Define a profile"><form className="control-form" onSubmit={(event) => { event.preventDefault(); const d=new FormData(event.currentTarget); void mutate(() => control(`/projects/${projectID}/profiles`,{method:"POST",body:JSON.stringify({name:d.get("name"),description:d.get("description"),kind:d.get("kind"),strategy:d.get("strategy"),enabled:true,max_attempts:1,targets:[{provider_connection_id:d.get("provider"),upstream_model_id:d.get("model"),priority:1,weight:1,input_price_per_million:0,output_price_per_million:0}]})}),"Profile created."); event.currentTarget.reset(); }}><label>Profile name<input name="name" required placeholder="fast or search"/></label><label>Workload<select name="kind"><option value="generation">Text generation</option><option value="embedding">Embeddings</option></select></label><label>Provider<select name="provider" required><option value="">Choose…</option>{providers.map((item)=><option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label>Upstream model<input name="model" required placeholder="gpt-4o-mini or text-embedding-3-small"/></label><label>Strategy<select name="strategy"><option value="priority">Priority / failover</option><option value="round_robin">Round robin</option><option value="weighted">Weighted</option></select></label><label className="wide">Description<input name="description" placeholder="Low-latency default for interactive features"/></label><button className="control-primary" disabled={!providers.length}>Create profile</button></form></Panel></>}

        {project && section === "keys" && <><Panel title="Application keys" note="Use separate keys for each environment or component. Revocation takes effect on the next request."><Table heads={["Label","Environment","Prefix","Profiles","Last used"]} rows={keys.map((item) => [item.label,item.environment,item.prefix,item.allowed_profiles.join(", ") || "all enabled",item.revoked_at ? "revoked" : item.last_used_at ? new Date(item.last_used_at).toLocaleString() : "never"])} /></Panel>{revealedKey && <div className="control-secret"><strong>Copy this key now. It will not be shown again.</strong><code>{revealedKey}</code><button onClick={() => navigator.clipboard.writeText(revealedKey)}>Copy</button></div>}<Panel title="Issue an application key"><form className="control-form" onSubmit={async(event)=>{event.preventDefault();const d=new FormData(event.currentTarget);try{const created=await control<{plaintext:string}>(`/projects/${projectID}/keys`,{method:"POST",body:JSON.stringify({label:d.get("label"),environment:d.get("environment"),allowed_profiles:d.getAll("profiles")})});setRevealedKey(created.plaintext);await loadProject(projectID);event.currentTarget.reset();}catch(cause){setError(cause instanceof Error?cause.message:String(cause))}}}><label>Label<input name="label" required placeholder="Support API production"/></label><label>Environment<select name="environment"><option>development</option><option>staging</option><option>production</option><option>test</option></select></label><fieldset><legend>Allowed profiles</legend>{profiles.map((item)=><label className="check" key={item.id}><input type="checkbox" name="profiles" value={item.name}/>{item.name}</label>)}</fieldset><button className="control-primary">Create key</button></form></Panel></>}

        {project && section === "policies" && policy && <Panel title="Project policy" note="These controls protect the whole project; key-specific limits may only narrow them."><form className="control-form" onSubmit={(event)=>{event.preventDefault();const d=new FormData(event.currentTarget);const updated={...policy,requests_per_minute:Number(d.get("rpm")),tokens_per_minute:Number(d.get("tpm")),cache_mode:d.get("cache"),require_end_user:d.get("end_user")==="on",analytics_retention_days:Number(d.get("retention"))};void mutate(()=>control(`/projects/${projectID}/policy`,{method:"PUT",body:JSON.stringify(updated)}),"Policy updated.")}}><label>Requests / minute<input name="rpm" type="number" min="1" defaultValue={policy.requests_per_minute}/></label><label>Tokens / minute<input name="tpm" type="number" min="1" defaultValue={policy.tokens_per_minute}/></label><label>Cache mode<select name="cache" defaultValue={policy.cache_mode}><option>disabled</option><option>exact</option><option>semantic</option></select></label><label>Analytics retention<select name="retention" defaultValue={policy.analytics_retention_days}><option value="7">7 days</option><option value="30">30 days</option><option value="90">90 days</option></select></label><label className="check wide"><input name="end_user" type="checkbox" defaultChecked={policy.require_end_user}/>Require X-Conduit-End-User attribution</label><button className="control-primary">Save policy</button></form></Panel>}

        {project && section === "usage" && <><div className="control-grid thirds"><Metric label="Requests · 24h" value={usage?.requests ?? 0} detail={`${usage?.errors ?? 0} errors`}/><Metric label="Estimated cost · 24h" value={`$${(usage?.cost_usd ?? 0).toFixed(4)}`} detail={`${(usage?.prompt_tokens ?? 0) + (usage?.completion_tokens ?? 0)} recorded tokens`}/><Metric label="P95 latency" value={`${(usage?.p95_ms ?? 0).toFixed(0)} ms`} detail={`${((usage?.cache_hit_rate ?? 0) * 100).toFixed(1)}% cache hit rate`}/></div><Panel title="Find an application user" note="Enter the same opaque ID the application sends. Conduit HMACs it on the server; the raw value is not stored or placed in the URL."><form className="control-form" onSubmit={searchEndUser}><label className="wide">Opaque end-user ID<input name="end_user" required autoComplete="off" placeholder="internal-user-42"/></label><div className="control-form-actions"><button className="control-primary">Search usage</button>{usageFilter && <button type="button" className="control-action" onClick={() => void loadProject(projectID)}>Clear filter</button>}</div></form></Panel><Panel title="Recent request metadata" note={usageFilter ? "Filtered by the server-derived end-user pseudonym." : "Prompts and responses are never present in this table."}><Table heads={["When","API","Environment","End user","Profile → upstream","Provider","Status","Tokens","Cost","Cache","Latency"]} rows={requests.map((item)=>[new Date(item.timestamp).toLocaleString(),item.operation,item.environment,item.end_user_pseudonym ? `${item.end_user_pseudonym.slice(0,12)}…` : "unattributed",`${item.profile} → ${item.upstream_model}`,item.provider,item.status,item.prompt_tokens+item.completion_tokens,`$${item.cost_usd.toFixed(6)}`,item.cache_status,`${item.duration_ms} ms`])}/></Panel></>}

        {project && section === "members" && <><Panel title="Project members"><Table heads={["Administrator","Email","Role"]} rows={members.map((item)=>[item.display_name,item.email,item.role.replaceAll("_"," ")])}/></Panel><Panel title="Add an existing administrator"><form className="control-form" onSubmit={(event)=>{event.preventDefault();const d=new FormData(event.currentTarget);void mutate(()=>control(`/projects/${projectID}/members`,{method:"POST",body:JSON.stringify({email:d.get("email"),role:d.get("role")})}),"Membership saved.");event.currentTarget.reset()}}><label>Email<input name="email" type="email" required/></label><label>Role<select name="role"><option value="project_viewer">Viewer</option><option value="project_admin">Admin</option><option value="project_owner">Owner</option></select></label><button className="control-primary">Add member</button></form></Panel></>}
        {project && section === "audit" && <Panel title="Audit history" note="Configuration mutations record who changed what. Secrets are sanitized before persistence."><Table heads={["When","Administrator","Action","Resource"]} rows={audit.map((item)=>[new Date(item.created_at).toLocaleString(),item.actor_name,item.action,item.resource_type])}/></Panel>}
      </section>
    </main>
  );
}

function Panel({title,note,children}:{title:string;note?:string;children:React.ReactNode}) { return <section className="control-card control-panel"><header><div><h2>{title}</h2>{note&&<p>{note}</p>}</div></header>{children}</section>; }
function Metric({label,value,detail}:{label:string;value:string|number;detail:string}) { return <section className="control-card control-metric"><span>{label}</span><strong>{value}</strong><p>{detail}</p></section>; }
function Table({heads,rows}:{heads:string[];rows:React.ReactNode[][]}) { return <div className="control-table-wrap"><table><thead><tr>{heads.map((head)=><th key={head}>{head}</th>)}</tr></thead><tbody>{rows.length?rows.map((row,index)=><tr key={index}>{row.map((cell,cellIndex)=><td key={cellIndex}>{cell}</td>)}</tr>):<tr><td colSpan={heads.length}>Nothing here yet.</td></tr>}</tbody></table></div>; }
