# Conduit dashboard

The Next.js application contains the project overview at `/` and the
authenticated admin dashboard at `/dashboard`.

```bash
cp .env.example .env.local
npm ci
npm run dev
```

Set `CONDUIT_URL` to the Go gateway. The dashboard is a backend-for-frontend:
its route handlers forward the administrator's HTTP-only session cookie and
CSRF token to `/control/v1`. Provider credentials and application-key hashes
never reach browser JavaScript.

There is no public registration. Bootstrap the first platform owner from the
repository root, then sign in at `http://localhost:3000/login`:

```bash
docker compose exec gateway conduit admin bootstrap \
  --email owner@example.test \
  --name "Local Owner" \
  --password 'choose-a-long-password'
```

Dashboard queries are project-scoped by the Go API. The browser cannot widen
its access by changing filters or project IDs.

```text
browser -> Next.js route handler -> gateway /control/v1 -> PostgreSQL / Redis / ClickHouse
             forwards session          enforces membership
```
