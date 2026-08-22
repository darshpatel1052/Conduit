# Local operations

## Start and bootstrap

```bash
docker compose up --build -d
docker compose ps

docker compose exec gateway /app/conduit admin bootstrap \
  --email owner@example.test --name "Platform Owner" \
  --password 'replace-with-a-long-password' \
  --postgres-dsn 'postgres://conduit:conduit@postgres:5432/conduit?sslmode=disable'
```

Set a random 32-byte base64 `CONDUIT_MASTER_KEY` outside local development:

```bash
openssl rand -base64 32
```

Changing that environment value without re-encrypting provider rows makes their credentials unreadable. Back up the master key separately from PostgreSQL.

## Observe

```bash
curl -fsS localhost:8080/livez
curl -fsS localhost:8080/readyz
docker compose logs -f gateway
docker compose exec redis redis-cli XLEN conduit:analytics:requests
curl -u conduit:conduit 'localhost:8123/?query=SELECT%20count()%20FROM%20conduit.conduit_requests'
```

The dashboard is at `http://localhost:3000`. Its browser session is an HTTP-only cookie; mutations also require the CSRF value held by the UI.

## Reset local state

`docker compose down` stops the stack and preserves data. `docker compose down -v` also deletes the development databases and cannot be recovered unless the volumes were backed up.

## Production boundaries

Put the gateway and dashboard behind TLS, set `control.secure_cookies: true`, inject the master key and store credentials through a secret manager, restrict `/metrics` and health endpoints to the service network, and back up PostgreSQL and Redis persistence. The included stack demonstrates the architecture; it does not claim compliance certification or provide enterprise SSO.
