# Security policy

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
security advisory flow for this repository:

`https://github.com/darshpatel1052/conduit/security/advisories/new`

Include the affected version, reproduction steps, impact, and any suggested
mitigation. Please allow a reasonable remediation window before disclosure.

## Security boundaries

- Inference endpoints require project application keys. PostgreSQL stores an
  indexed public identifier and a one-way hash; plaintext is returned once.
- The control API requires an administrator session, project membership, and a
  CSRF token for mutations. There is no public registration.
- Provider credentials are encrypted with AES-256-GCM. Ciphertext is bound to
  its project and provider IDs and cannot be copied safely between rows.
- `X-Conduit-End-User` is stripped before forwarding. Only a project-scoped
  HMAC pseudonym is retained in usage records.
- Prompts and model responses are forwarded, not persisted.
- `/metrics` and health probes are intentionally unauthenticated and must remain
  on a private network when Conduit is deployed beyond a developer machine.
- `CONDUIT_MASTER_KEY` and infrastructure credentials belong in deployment
  secrets. Provider credentials belong in the authenticated project control
  plane, never in route files or source control.
- Local HTTP provider URLs are permitted for Docker and Ollama development.
  Public deployments should restrict management access and use HTTPS provider
  endpoints.

## Supported versions

Until the first tagged stable release, security fixes are made on the `main`
branch only.
