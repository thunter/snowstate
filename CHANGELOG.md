# Changelog

## Unreleased

- Add the Foundation, a Terraform module for an Install's account-level objects, and the Seed that starts it.
- Add runbook procedures for recovering and removing an Install.
- Add `snowstate seed`, which prints the Seed SQL with a public key filled in.
- Add `snowstate install`, which creates an Install with the real Terraform or OpenTofu and moves its state into snowstate.
- `snowstate doctor` warns when secondary roles mean it can't prove the configured role's grants.
- `snowstate install` is safe to run again: it resumes where it stopped, applies later Foundation changes through snowstate, and shows any difference from the shipped code.

## 0.1.0 - 2026-11-01

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
- Stable releases need a matching `CHANGELOG.md` section.
- Structured logs, Prometheus metrics, health and readiness endpoints.
