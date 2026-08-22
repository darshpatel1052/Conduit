import Link from "next/link";

const request = `import os
from openai import OpenAI

client = OpenAI(
    base_url="https://llm.internal/v1",
    api_key=os.environ["CONDUIT_API_KEY"],
)

client.chat.completions.create(
    model="fast",
    messages=[{"role": "user", "content": "Summarize this incident."}],
)`;

const responsibilities = [
  ["Application team", "Chooses an approved model name and uses a normal OpenAI client."],
  ["Conduit", "Authenticates, applies limits, resolves the route, handles failure, and records the outcome."],
  ["Project administrator", "Owns provider credentials, model profiles, application keys, policy, and membership."],
];

const capabilities = [
  ["Routing", "Stable project profiles with priority, round-robin, or weighted provider targets."],
  ["Failure handling", "Retry classification, provider failover, circuit breakers, and streaming cancellation."],
  ["Access", "Hashed project keys, administrator RBAC, profile allow-lists, and shared Redis limits."],
  ["Caching", "Project-isolated exact or semantic caching, guarded by generation settings and policy versions."],
  ["Operations", "Prometheus health, durable ClickHouse analytics, pseudonymous users, and audit history."],
  ["Deployment", "One Compose stack with the gateway, dashboard, PostgreSQL/pgvector, Redis, and ClickHouse."],
];

export default function ProjectHome() {
  return (
    <main className="showcase-shell min-h-screen overflow-hidden">
      <div className="showcase-grid" />
      <div className="relative mx-auto max-w-[1180px] px-5 py-6 md:px-8">
        <nav className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <span className="brand-mark" aria-hidden="true">C</span>
            <div>
              <p className="text-sm font-semibold">Conduit</p>
              <p className="text-[10px] uppercase tracking-[0.2em]" style={{ color: "var(--text-muted)" }}>
                LLM gateway for application teams
              </p>
            </div>
          </div>
          <div className="flex items-center gap-2 text-xs">
            <a href="https://github.com/darshpatel1052/conduit" className="showcase-link showcase-nav-source px-3 py-2" target="_blank" rel="noreferrer">
              Source ↗
            </a>
            <Link href="/dashboard" className="showcase-cta px-3 py-2">Admin dashboard →</Link>
          </div>
        </nav>

        <section className="grid items-center gap-12 pb-16 pt-20 lg:grid-cols-[1.02fr_0.98fr] lg:pb-24 lg:pt-28">
          <div>
            <p className="mb-5 font-mono text-xs uppercase tracking-[0.18em]" style={{ color: "var(--series-2)" }}>
              One inference boundary for a team
            </p>
            <h1 className="max-w-3xl text-5xl font-semibold leading-[1] tracking-[-0.05em] sm:text-6xl">
              Keep provider policy out of application code.
            </h1>
            <p className="mt-7 max-w-2xl text-base leading-7" style={{ color: "var(--text-secondary)" }}>
              Applications call an OpenAI-compatible endpoint. Conduit decides which configured provider and model should serve the request, then applies the same access, reliability, and observability rules everywhere.
            </p>
            <div className="mt-8 flex flex-wrap gap-3">
              <a href="https://github.com/darshpatel1052/conduit#run-it-locally" className="showcase-cta px-4 py-2 text-sm" target="_blank" rel="noreferrer">Run locally ↗</a>
              <a href="#boundary" className="showcase-link px-4 py-2 text-sm">Understand the boundary ↓</a>
            </div>
          </div>

          <section className="decision-card" aria-label="OpenAI client example">
            <div className="flex items-center justify-between border-b pb-4" style={{ borderColor: "var(--border)" }}>
              <div>
                <p className="text-xs font-medium">Application integration</p>
                <p className="mt-1 text-xs" style={{ color: "var(--text-muted)" }}>The provider is not selected in this code.</p>
              </div>
              <span className="rounded border px-2 py-1 font-mono text-[10px]" style={{ borderColor: "var(--border)", color: "var(--series-2)" }}>Python</span>
            </div>
            <pre className="mt-4 overflow-x-auto whitespace-pre-wrap font-mono text-[11px] leading-5" style={{ color: "var(--text-secondary)" }}>
              <code>{request}</code>
            </pre>
          </section>
        </section>

        <section id="boundary" className="border-y py-14" style={{ borderColor: "var(--border)" }}>
          <div className="grid gap-8 lg:grid-cols-[0.75fr_1.25fr]">
            <div>
              <p className="text-[11px] uppercase tracking-[0.2em]" style={{ color: "var(--series-2)" }}>Product boundary</p>
              <h2 className="mt-3 text-3xl font-semibold tracking-[-0.035em]">Infrastructure a team operates.</h2>
              <p className="mt-4 text-sm leading-6" style={{ color: "var(--text-secondary)" }}>
                Conduit is not a customer signup and billing service. It is a gateway a team runs for its own applications when it wants one managed path to several model providers.
              </p>
            </div>
            <div className="grid gap-3">
              {responsibilities.map(([title, copy], index) => (
                <article key={title} className="grid gap-3 rounded-lg border p-4 sm:grid-cols-[9rem_1fr]" style={{ borderColor: "var(--border)", background: "var(--surface)" }}>
                  <div className="flex items-center gap-3">
                    <span className="font-mono text-[10px]" style={{ color: "var(--series-1)" }}>0{index + 1}</span>
                    <h3 className="text-sm font-medium">{title}</h3>
                  </div>
                  <p className="text-sm leading-6" style={{ color: "var(--text-muted)" }}>{copy}</p>
                </article>
              ))}
            </div>
          </div>
        </section>

        <section className="py-20">
          <div className="mb-10 max-w-2xl">
            <p className="text-[11px] uppercase tracking-[0.2em]" style={{ color: "var(--series-2)" }}>Implemented scope</p>
            <h2 className="mt-3 text-3xl font-semibold tracking-[-0.035em]">Small enough to understand, useful enough to run.</h2>
            <p className="mt-4 text-sm leading-6" style={{ color: "var(--text-secondary)" }}>
              The project supports Chat Completions, stateless Responses, and Embeddings. Provider connections, model profiles, policies, and keys are explicit project configuration, so routing remains inspectable.
            </p>
          </div>
          <div className="grid gap-px overflow-hidden rounded-xl border sm:grid-cols-2 lg:grid-cols-3" style={{ borderColor: "var(--border)", background: "var(--border)" }}>
            {capabilities.map(([title, copy], index) => (
              <article key={title} className="feature-cell">
                <span className="font-mono text-[10px]" style={{ color: index % 2 ? "var(--series-2)" : "var(--series-1)" }}>{String(index + 1).padStart(2, "0")}</span>
                <h3 className="mt-5 text-base font-medium">{title}</h3>
                <p className="mt-2 text-sm leading-6" style={{ color: "var(--text-muted)" }}>{copy}</p>
              </article>
            ))}
          </div>
        </section>

        <section className="mb-16 grid gap-6 rounded-xl border p-7 lg:grid-cols-[1fr_auto] lg:items-center" style={{ borderColor: "var(--border)", background: "var(--surface)" }}>
          <div>
            <h2 className="text-lg font-medium">Operate a project, not a pile of environment variables.</h2>
            <p className="mt-2 max-w-2xl text-sm leading-6" style={{ color: "var(--text-muted)" }}>
              Sign in to manage encrypted provider connections, stable profiles, application keys, quotas, caching, usage, members, and the audit trail.
            </p>
          </div>
          <Link href="/dashboard" className="showcase-cta px-4 py-2 text-sm">Open admin dashboard →</Link>
        </section>

        <footer className="flex flex-wrap justify-between gap-3 border-t py-6 text-xs" style={{ borderColor: "var(--border)", color: "var(--text-muted)" }}>
          <span>Go · Redis · Postgres/pgvector · ClickHouse · Next.js</span>
          <span>Built by Darsh Patel</span>
        </footer>
      </div>
    </main>
  );
}
