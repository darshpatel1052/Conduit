# Contributing

Conduit is easiest to change when the local product story stays green.

## Before changing code

1. Read [docs/architecture.md](docs/architecture.md) and the package comment for the area you are changing.
2. Start the required stores with `docker compose up -d`.
3. Keep provider-specific wire translation inside `internal/provider`; routing and policy code should not branch on vendor names.

## Checks

```bash
make test
make validate
make demo

cd web
npm run lint
npx tsc --noEmit
npm run build
```

Add a focused unit or contract test for behavior changes. Use the deterministic mock provider in integration tests. Real provider credentials and live model availability must not be required by CI.

## Pull requests

Explain the user-visible behavior, the invariant or tradeoff behind it, and how it was tested. Keep prompts, outputs, credentials, generated binaries, local databases, and `.env` files out of commits.
