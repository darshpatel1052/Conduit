# Conduit

**An LLM gateway for application teams.**

Conduit gives a team one API for several model providers. Applications request a stable profile such as `fast`, `quality`, or `search`; project administrators decide which provider models sit behind that name, how failures are handled, who may call it, what it may cost, and which request metadata is kept.

The practical result is simple: changing a provider is a project setting, not an application release.

```mermaid
flowchart LR
    App[Application] -->|model: fast| Gateway[Conduit gateway]
    Gateway --> Policy[Project key, limits, budget]
    Policy --> Profile[fast profile]
    Profile -->|primary| Local[Ollama / llama]
    Profile -. failover .-> Hosted[OpenAI / GPT]
    Gateway --> Events[Metadata-only usage events]
```

Conduit is intended for one company or team to run for its own applications. It is not a model marketplace, credit reseller, or public signup service.

## What works

- OpenAI-compatible Chat Completions, Embeddings, model listing, and a portable subset of the Responses API
- streaming for Chat Completions and Responses, with failover only before the first response byte
- OpenAI, Anthropic, Ollama, and OpenAI-compatible provider connections
- generation and embedding profiles with priority, round-robin, or smooth weighted routing
- project application keys, profile allow-lists, expiry metadata, and immediate revocation
- local administrator accounts with project owner, admin, and viewer roles
- encrypted provider credentials, project quotas, monthly budgets, end-user attribution, and audit history
- project-isolated exact and semantic caching for Chat Completions
- durable Redis Stream delivery into ClickHouse for metadata-only usage analytics

Prompts, model outputs, plaintext application keys, provider credentials, and raw end-user IDs are not stored in analytics.

## Run it locally

Requirements: Docker, Docker Compose, `curl`, and `jq`.

```bash
docker compose up --build -d
docker compose ps
make demo
```

`make demo` creates a project, connects two deterministic providers, defines generation and embedding profiles, issues an application key, calls all three inference APIs, changes the provider behind a profile, verifies attributed ClickHouse rows, archives the project, revokes the key, and checks the audit trail.

Open [http://localhost:3000/login](http://localhost:3000/login). The local demo login is:

```text
owner@conduit.local
ConduitLocal!2026
```

For a new installation, bootstrap your own owner:

```bash
docker compose exec gateway /app/conduit admin bootstrap \
  --email you@example.test \
  --name "Your Name" \
  --password 'a-long-local-password' \
  --postgres-dsn 'postgres://conduit:conduit@postgres:5432/conduit?sslmode=disable'
```

The Compose key is for development only. Generate a different 32-byte base64 key before saving real provider credentials:

```bash
openssl rand -base64 32
export CONDUIT_MASTER_KEY='the-generated-value'
```

## Use it from an application

Create a project, provider connection, model profile, and application key in the admin dashboard. The application uses that Conduit key—not the provider key.

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="ck_publicid_secret",
)

chat = client.chat.completions.create(
    model="fast",
    messages=[{"role": "user", "content": "Summarize this ticket."}],
    extra_headers={"X-Conduit-End-User": "internal-user-42"},
)

response = client.responses.create(
    model="fast",
    instructions="Answer in one paragraph.",
    input="Explain this incident.",
    extra_headers={"X-Conduit-End-User": "internal-user-42"},
)

vectors = client.embeddings.create(
    model="search",
    input=["first document", "second document"],
    extra_headers={"X-Conduit-End-User": "internal-user-42"},
)
```

Generation profiles work with Chat Completions and Responses. Embedding profiles work with Embeddings. A key can be limited to either kind.

The Responses endpoint is intentionally stateless and text-focused. It accepts `input`, `instructions`, generation settings, and streaming. It rejects `store: true`, `previous_response_id`, conversations, and provider-hosted tools because provider-owned state cannot be moved safely during failover.

## Who manages what?

| Role or credential | Purpose |
| --- | --- |
| Platform owner | Bootstraps the installation, invites administrators, and can reach every project. |
| Project owner | Manages project members as well as providers, profiles, keys, policies, usage, and project archival. |
| Project admin | Manages providers, profiles, keys, and policies, but cannot change ownership or archive the project. |
| Project viewer | Reads configuration, usage, and audit history without changing anything. |
| Application key | Lets an application call approved profiles; it cannot open the admin dashboard. |

Provider keys stay encrypted in Conduit. The application receives a separate project key and sends it in the normal bearer header:

```http
Authorization: Bearer ck_publicid_secret
```

## Repository map

```text
cmd/gateway/          executable and admin CLI
internal/provider/    provider adapters and wire contracts
internal/router/      profile resolution and balancing
internal/proxy/       retries, breakers, failover, and stream handling
internal/server/      inference HTTP API and request policy
internal/control/     projects, administrators, keys, policy, and runtime loading
internal/controlapi/  authenticated admin HTTP API
internal/cache/       exact and pgvector semantic cache
internal/analytics/   Redis Stream and ClickHouse event pipeline
web/                  Next.js admin dashboard and BFF routes
scripts/              deterministic demo and load checks
docs/                 architecture, request flow, data, operations, and OpenAPI
```

Start with the [documentation index](docs/README.md), then read the [architecture](docs/architecture.md) and [request lifecycle](docs/request-lifecycle.md). The [OpenAPI contract](docs/openapi.yaml) documents both inference and admin endpoints.

## Why three stores?

| Store | Job | If it is unavailable |
| --- | --- | --- |
| PostgreSQL + pgvector | projects, administrators, encrypted providers, profiles, keys, policy, audit, and cache | admin changes stop; inference may use a short-lived loaded snapshot |
| Redis | sessions, distributed limits, budgets, invalidation, and durable request queue | new inference fails closed because access and spend rules cannot be trusted |
| ClickHouse | request metadata and usage aggregates | inference continues while events wait in Redis |

## Development checks

```bash
make test
make validate

cd web
npm ci
npm run lint
npx tsc --noEmit
npm run build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the change workflow. The deterministic Compose environment is the integration target; paid-provider checks stay optional so CI tests Conduit rather than someone else’s uptime.

## Deliberate boundaries

Conduit does not currently provide image, audio, Realtime, batch, prompt guardrails, automatic prompt classification, public billing, enterprise SSO, or compliance certification. Pricing is entered by administrators and is always presented as an estimate.
