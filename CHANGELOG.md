# Changelog

## Unreleased

- HTTP state backend protocol over a frontend-neutral core, with an in-memory store for tests.
- Token, loopback-secret and mTLS authentication, with scope-based authorization.
- `snowstate exec` sidecar mode.
- Snowflake store: hybrid locking, versioned writes, inline and stage blobs, audit, inventory view.
- `migrate`, `doctor`, token, state, lock, project and gc commands, and the admin HTTP API.
- Structured logs, Prometheus metrics, health and readiness endpoints.
