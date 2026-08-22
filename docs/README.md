# Documentation

This folder explains the decisions behind Conduit, not just its configuration knobs.

- [Architecture](architecture.md): services, stores, ownership, and failure boundaries
- [Request lifecycle](request-lifecycle.md): what happens to Chat, Responses, and Embeddings calls
- [Data model](data-model.md): project isolation, profile kinds, stored metadata, and privacy
- [Security model](security.md): administrator sessions, application keys, secrets, SSRF controls, and trust boundaries
- [Local operations](operations.md): startup, bootstrap, health, secrets, backups, and reset behavior
- [OpenAPI](openapi.yaml): inference and administrator HTTP contracts

The quickest proof that the pieces work together is `make demo`. It uses the same HTTP APIs as the dashboard and a normal application client; it does not inspect private Go state to declare success.
