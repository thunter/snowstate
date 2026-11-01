# Changelog

## Unreleased

- HTTP state backend protocol over a frontend-neutral core, with an in-memory store for tests.
- Token, loopback-secret and mTLS authentication, with scope-based authorization.
- `snowstate exec` sidecar mode.
- Snowflake store: hybrid locking, versioned writes, inline and stage blobs, audit, inventory view.
- `migrate`, `doctor`, token, state, lock, project and gc commands, and the admin HTTP API.
- Dockerfile, Kubernetes example, multi-arch release build, and operator documentation.
- GitHub Actions: workload identity federation, CI context in audit rows, step summaries, `action/setup`.
- Release tags are validated as semver, prereleases are marked, and the image is multi-arch.
- CI skips long jobs for documentation-only changes.
- `action/setup` works with major tags such as `@v0`.
- Structured logs, Prometheus metrics, health and readiness endpoints.
