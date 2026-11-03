# snowstate: Terraform/OpenTofu HTTP State Backend on Snowflake

**Build specification v1.3** · 2 November 2026 · Owner: Todd Hunter

> **v1.3 changes:** The install path moves from a SQL script to a Seed, a Terraform-managed Foundation and `snowstate install`.
> - The Seed and Foundation replace `bootstrap.sql` (§8.5, §10).
> - Account-level names derive from the database name, so an account can hold several Installs (§10).
> - `schema_per_project` and GitHub identity setup move to the operator guide, marked unverified (§5.3, §8.4.1).
> - M7 is Deferred, and new milestone M8 is required for v1.0 (§14).

> **v1.2 changes:**
> - GitHub Actions is a first-class target (§8.4): workload identity federation auth (§5.2), saved-plan portability and abort behaviour in sidecar mode (§8.1), a setup action, reference workflows, and new e2e scenarios 9–11.
> - New milestone M6.5.

> **v1.1 changes:** The design is now switchable to Terraform's experimental Pluggable State Storage (`state_store`, see `docs/pluggable-state-storage.md`). Changes:
> - The core is reusable and frontend-neutral (§3).
> - State keys follow a strict `<prefix>/<workspace>` convention (§4.2).
> - Lock storage works for both frontends (§6.1, §6.5).
> - Authorization works both when access goes through the server and when clients connect to Snowflake directly (§5.3).
> - A new experimental milestone M7 adds the provider (§14).

> **For the implementing agent:** This document is the source of truth. Read all of it before writing code. Work through the milestones in §14 in order and don't skip acceptance criteria. Where this spec says **VERIFY**, confirm the behaviour against the real source or a real system before relying on it, and record what you found in `docs/decisions.md`. If something here is ambiguous or turns out to be wrong, pick the safest reasonable behaviour, document the decision in `docs/decisions.md`, and carry on. Don't stop and wait.

---

## 1. Purpose

Terraform and OpenTofu have no Snowflake backend. Neither supports third-party backend plugins in stable releases. (Terraform's provider-based `state_store` is alpha-only, see `docs/pluggable-state-storage.md`.) Both ship a generic **`http` backend** that talks to any REST server implementing a small protocol. `snowstate` is that server: it implements the protocol and stores state, state history, locks and an audit trail in Snowflake.

### 1.1 Goals

- G1. Drop-in Terraform (≥ 1.5) and OpenTofu (≥ 1.6) backend using only the stock `http` backend config.
- G2. **Correct, race-free locking.** Two concurrent `apply`s against the same state must never both hold the lock.
- G3. Every successful write is kept as an immutable version. You can list, download and restore any version.
- G4. Two deployment modes from one binary:
  - **Server mode:** a long-running shared service (container, VM, Kubernetes).
  - **Sidecar mode:** `snowstate exec -- terraform <args>` starts an ephemeral loopback server, injects the backend env vars, runs the command, and shuts down. No shared infrastructure is needed.
- G5. Snowflake-native operations: least-privilege role, idempotent schema bootstrap and migrations, an audit table, and a SQL view that exposes managed resources for inventory queries.
- G6. Never corrupt or lose state. Byte-for-byte fidelity, MD5 verification, atomic writes.
- G7. **Switchable frontend.** All storage, locking, versioning and authorization logic lives in a transport-neutral core (`pkg/`). A Terraform provider implementing a `state_store` (M7) must be able to use the same core and the **same Snowflake tables and keys**, without a data migration. The HTTP and provider frontends must interoperate: a lock taken through one blocks the other.

### 1.2 Non-goals (v1)

- A web UI.
- Running Terraform operations remotely (it's a state backend only, not like HCP Terraform runs).
- Hosting inside Snowpark Container Services (SPCS ingress requires a `Snowflake Token` header, and Terraform's `http` backend can only send Basic auth). Leave a seam for this, but don't build it.
- Multi-account or cross-region replication (rely on Snowflake replication if needed).
- Shared or read-only locks.
- A Terraform provider. M7 is **Deferred** because `state_store` works only in Terraform alpha builds, and it stays a future option (§14).

---

## 2. Technology choices (fixed)

| Area | Choice | Notes |
|---|---|---|
| Language | Go, latest stable (≥ 1.24) | Single static binary, good Snowflake driver. |
| Snowflake driver | `github.com/snowflakedb/gosnowflake` (latest v1.x, **≥ v1.16.0**) | Supports key-pair JWT, PAT, OAuth, external browser, and workload identity federation (≥ 1.16.0, needed for GitHub Actions OIDC, §8.4). |
| HTTP | Go stdlib `net/http` | **Route on `r.Method` manually inside the handler**, because `LOCK`/`UNLOCK` are non-standard verbs. Don't depend on `ServeMux` method patterns for them. |
| CLI | `github.com/spf13/cobra` | |
| Config | `github.com/knadh/koanf/v2` (YAML file + env + flags) | |
| Logging | `log/slog`, JSON handler by default | |
| Metrics | `github.com/prometheus/client_golang` | |
| Tests | stdlib `testing` + `github.com/stretchr/testify` | |
| Lint | `golangci-lint` (config in repo) | |
| Container | Distroless static base, non-root | |
| License | Apache-2.0 | |

Keep dependencies to this list unless there's a strong reason. Record any additions in `docs/decisions.md`.

---

## 3. Architecture

```
┌───────────────┐   HTTP(S) + Basic auth       ┌──────────────────────────┐   gosnowflake   ┌────────────────────────────┐
│ terraform /   │ ───────────────────────────▶ │ snowstate                │ ──────────────▶ │ Snowflake                  │
│ tofu (http    │  GET / POST / DELETE         │  ├ api      (protocol)   │   SQL over TLS  │  DB TF_STATE               │
│ backend)      │  LOCK / UNLOCK               │  ├ auth     (tokens)     │                 │   SCHEMA BACKEND           │
└───────────────┘                              │  ├ service  (rules)      │                 │    STATES          (std)   │
                                               │  ├ store    (interface)  │                 │    STATE_VERSIONS  (std)   │
                                               │  │   ├ snowflake         │                 │    LOCKS           (hybrid)│
                                               │  │   └ memory (tests)    │                 │    TOKENS          (std)   │
                                               │  └ obs (logs, metrics)   │                 │    AUDIT_LOG       (std)   │
                                               └──────────────────────────┘                 │    SCHEMA_MIGRATIONS (std) │
                                                                                            │    STAGE STATE_BLOBS       │
                                                                                            └────────────────────────────┘
```

**Layering rule:** frontend (`internal/api` today, `provider/` in M7) → `pkg/snowstate/service` → `pkg/snowstate/store` (interface). Only `store/snowflake` imports gosnowflake. `service` holds every business rule (lock checks, write fencing, MD5, size limits, versioning, authorization hooks). `store/memory` is a full in-process implementation used by unit tests and the e2e suite when no Snowflake is configured.

**Frontend-neutrality rules (G7), enforced by a test:**
- Nothing under `pkg/` imports `net/http`, `internal/api`, or any Terraform plugin library. Add `TestImportBoundaries`, which runs `go list -deps ./pkg/...` and fails on violations.
- The service API uses plain Go types and typed errors. Each frontend maps those errors to its own protocol: HTTP status codes in `api`, Terraform diagnostics in the provider. The minimum API:

```go
type Principal struct { Name string; Source string /* "token" | "mtls" | "loopback" | "snowflake" */ }

type LockRequest struct {
    Key, Operation, Who, Info, TFVersion, Path string
    LockID  string // HTTP: Terraform-supplied. PSS: empty, so the service generates a UUID
    RawJSON []byte // HTTP only: original LockInfo body, stored verbatim
    Source  string // "HTTP" | "PSS"
}
type Lock struct { Key, LockID, Operation, Who, Info, TFVersion, Path, Source, AcquiredBy string; Created, AcquiredAt time.Time; RawJSON []byte }

type StateService interface {
    Get(ctx context.Context, p Principal, key string) (*StateBlob, error)            // nil, nil = no state
    Put(ctx context.Context, p Principal, key string, lockID string, body []byte, md5 []byte) (*Version, error)
    Delete(ctx context.Context, p Principal, key string, lockID string) error
    Lock(ctx context.Context, p Principal, req LockRequest) (*Lock, error)           // ErrLocked{Current *Lock}
    Unlock(ctx context.Context, p Principal, key, lockID string, force bool) (*Lock, error) // ErrLockMismatch{Current}
    ListWorkspaces(ctx context.Context, p Principal, prefix string) ([]string, error) // §4.2
    // admin: ListStates, ListVersions, GetVersion, Restore, ListLocks, GC
}
// typed errors: ErrNotFound, ErrLocked, ErrLockMismatch, ErrLockNotHeld, ErrLockRequired,
// ErrInvalidKey, ErrInvalidState, ErrMD5Mismatch, ErrTooLarge, ErrForbidden, ErrUnavailable
```

### 3.1 Repository layout

```
/go.mod                     # module github.com/<owner>/snowstate
/cmd/snowstate/main.go
/pkg/snowstate/service/     # StateService (frontend-neutral), authorization hooks
/pkg/snowstate/store/       # Store interface + shared types + typed errors
/pkg/snowstate/store/memory/
/pkg/snowstate/store/snowflake/             # SQL, migrations (embedded), blob handling
/pkg/snowstate/store/snowflake/migrations/0001_init.sql ...
/pkg/snowstate/keys/        # key + workspace convention (§4.2)
/pkg/snowstate/sfconn/      # Snowflake connection config + auth modes (shared with provider)
/internal/api/              # HTTP handlers, routing, protocol encoding, middleware
/internal/auth/             # basic-auth token verification, scopes, loopback one-time secret
/provider/                  # M7 only: separate module terraform-provider-snowstate (own go.mod, `replace` to ../ in dev)
/internal/config/
/internal/sidecar/        # `exec` subcommand implementation
/internal/install/        # `install` step order and resume logic
/internal/obs/            # logging, metrics, request IDs
/test/e2e/                # runs real terraform + tofu binaries
/test/integration/        # runs against real Snowflake (build tag `integration`)
/deploy/docker/Dockerfile
/deploy/k8s/              # example manifests
/deploy/snowflake/seed.sql        # admin-run once per account: the Seed role and user
/deploy/terraform/foundation/     # Terraform root module for the Foundation, embedded in the binary
/examples/terraform/      # sample root module using the backend
/docs/                    # operator guide, decisions.md, runbook.md
Makefile, .golangci.yml, README.md, CHANGELOG.md
```

---

## 4. Terraform `http` backend protocol contract

### 4.0 VERIFY first (Milestone 0)

Before implementing, read the current client source and confirm each behaviour below:

- Terraform: `hashicorp/terraform` → `internal/backend/remote-state/http/` (`backend.go`, `client.go`)
- OpenTofu: `opentofu/opentofu` → `internal/backend/remote-state/http/`

Things to confirm and record in `docs/decisions.md`: status codes treated as "no state" on GET; whether `Content-MD5` is sent on POST and read on GET; lock and unlock body contents; **what body `force-unlock` sends** (believed to be empty, because a fresh process never called Lock); retry behaviour; and any new config args (for example, header support in newer OpenTofu).

### 4.1 Client configuration users will write

```hcl
terraform {
  backend "http" {
    address        = "https://snowstate.example.com/v1/states/platform/network/prod"
    lock_address   = "https://snowstate.example.com/v1/states/platform/network/prod"
    unlock_address = "https://snowstate.example.com/v1/states/platform/network/prod"
    # update_method = "POST"   (default)  lock_method = "LOCK"  unlock_method = "UNLOCK"
    username = "ci-platform"          # or TF_HTTP_USERNAME
    # password via TF_HTTP_PASSWORD
  }
}
```

Known client args: `address`, `update_method` (default POST), `lock_address`, `lock_method` (default LOCK), `unlock_address`, `unlock_method` (default UNLOCK), `username`, `password`, `skip_cert_verification`, `retry_max` (default 2), `retry_wait_min` (default 1), `retry_wait_max` (default 30), `client_ca_certificate_pem`, `client_certificate_pem`, `client_private_key_pem`. Env equivalents are `TF_HTTP_*`.

### 4.2 Routes

| Path | Purpose |
|---|---|
| `/v1/states/{key...}` | State resource. Methods: `GET`, `POST`, `PUT`, `DELETE`, `LOCK`, `UNLOCK`. |
| `/v1/states/{key...}/lock` | Alternate lock resource for proxies or load balancers that reject custom verbs. `POST`/`PUT`/`LOCK` = lock, `DELETE`/`UNLOCK` = unlock. Document using `lock_method = "POST"` and `unlock_method = "DELETE"` with this path. |
| `/v1/admin/...` | Admin API (§8.3). |
| `/healthz` | Liveness, no auth, no Snowflake call. |
| `/readyz` | Readiness, no auth. Runs `SELECT 1` (cached for 10 s). Returns 503 on failure. |
| `/metrics` | Prometheus. Optionally on a separate listener (`metrics.listen`). |

**State key rules:** `{key...}` is everything after `/v1/states/` (excluding a trailing `/lock` segment). It must match `^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*){0,15}$`, with a total length ≤ 512. Reject other keys with `400`. Keys are case-sensitive. Treat the first segment as the **project** for authorization scoping. The key `lock` can't be the final segment.

**Key and workspace convention (strict, required for G7).** It lives in `pkg/snowstate/keys`, and every frontend and CLI must use it:

- Every state key is `<prefix>/<workspace>`.
  - `<prefix>` is 1–15 segments. The first segment is the **project**.
  - `<workspace>` is exactly one segment: the last one.
- The Terraform **default workspace is the literal segment `default`**. Example: `platform/network/default`.
- Workspace names must match the segment regex above, which is stricter than Terraform's own rules. Return a clear error for names that don't fit.
- A key needs at least two segments, so a project-only key like `platform` is invalid.
- `ListWorkspaces(prefix)` returns the workspace segment of every **non-deleted** key that is exactly `<prefix>/<one segment>`. Deeper keys aren't workspaces of that prefix. If `default` has no state yet, still include it, because Terraform expects it to exist.
- HTTP users point `address` at the full key, one URL per workspace. Document this mapping:

  | HTTP | Provider |
  |---|---|
  | `address = ".../v1/states/platform/network/prod"` | `state_store` with `prefix = "platform/network"` + workspace `prod` |

  Both use the same row.
- `snowstate state list --prefix` shows the prefix/workspace split.
- Because a provider can list workspaces, `/v1/admin/states?prefix=` returns `{"key","prefix","workspace"}` objects.

### 4.3 Method semantics

All bodies are `application/json`. Lock-info JSON is the Terraform `LockInfo` shape:
`{"ID":"…","Operation":"…","Info":"…","Who":"…","Version":"…","Created":"RFC3339","Path":"…"}`.
For locks taken over HTTP, store the raw lock JSON as received and return it verbatim. Don't mutate Terraform's fields, and only add server fields under a separate key. If the current lock was taken by the **provider frontend** (`SOURCE='PSS'`, so no raw JSON), build a Terraform `LockInfo` JSON from the normalised columns (`ID`, `Operation`, `Who`, `Created`, with `Info` = `"held via snowstate state_store"`) so the HTTP client still shows a useful message.

| Request | Condition | Response |
|---|---|---|
| `GET` state | State exists | `200`, body = exact stored bytes, `Content-Type: application/json`, `Content-MD5: <base64 md5>`, `ETag: "<version_id>"` |
| | No state, or soft-deleted | `204 No Content` (VERIFY 204 vs 404; both believed accepted, so prefer 204) |
| `POST`/`PUT` state (`?ID=<lockid>` optional) | Body not valid JSON, or missing `version`/`serial`/`lineage` | `400` |
| | `Content-MD5` header present and doesn't match the body | `400` |
| | Body > `limits.max_state_bytes` | `413` |
| | Lock held, and `ID` missing or different | `423 Locked`, body = current lock JSON |
| | No lock held, and `ID` supplied | `409 Conflict`, `{"error":"lock not held"}` (the client thinks it holds a lock it doesn't) |
| | No lock held, no `ID`, `require_lock_for_write=true` | `423`, `{"error":"lock required"}` |
| | Body bytes identical (MD5 equal) to the current version | `200`, no new version (idempotent) |
| | Otherwise | `200`. A new version is written atomically and becomes current. |
| `DELETE` state | Lock held by someone else (ID via `?ID=` must match if a lock exists) | `423` + lock JSON |
| | Exists | `200`. Soft delete: mark `DELETED_AT`, keep versions (purged by GC after `retention.deleted_days`). |
| | Doesn't exist | `200` (idempotent) |
| `LOCK` | No lock | `200`, body = stored lock JSON |
| | Lock held with the same `ID` | `200` (idempotent re-lock) |
| | Lock held with a different `ID` | `423`, body = existing lock JSON |
| | Body invalid or missing `ID` | `400` |
| `UNLOCK` | Body has `ID` equal to the held lock | `200`, lock removed |
| | Body has `ID` that differs from the held lock | `409`, body = existing lock JSON |
| | No lock held | `200` (idempotent) |
| | **Empty body or no `ID` (force-unlock)** | If `locking.allow_force_unlock=true` (default) and the principal has `lock` permission: remove the lock, return `200`, and write audit action `FORCE_UNLOCK` with the removed lock JSON. Otherwise `403`. |
| Any | Missing or invalid credentials | `401` with `WWW-Authenticate: Basic realm="snowstate"` |
| | Authenticated but scope denies | `403` |
| | Snowflake unavailable or timeout | `503`, `Retry-After: 2` (the Terraform client retries) |
| | Unknown method | `405` with an `Allow` header |

Error bodies (when not lock JSON) are `{"error":"<message>","request_id":"<id>"}`. Every response carries `X-Request-Id`.

**Lock-before-write invariant:** "check lock + insert version + move current pointer" must happen in **one Snowflake transaction** (§6.4).

---

## 5. Authentication and authorization

### 5.1 Client → snowstate

Terraform only supports HTTP Basic and mTLS, so:

- **Tokens:** `username` = principal name, `password` = token. Token format is `sst_` + 43 chars of base64url (32 random bytes). Store only `HMAC-SHA256(server_pepper, token)` in `TOKENS`, compare in constant time, and cache verified tokens in memory for 60 s (bounded LRU).
- **Scopes:** each token has a list of `{key_pattern, permissions}`. `key_pattern` is a glob over the state key (`platform/*`, `*`), and permissions ⊆ `read` (GET), `write` (POST/PUT/DELETE), `lock` (LOCK/UNLOCK/force-unlock), `admin` (admin API). Deny by default.
- **Optional mTLS:** when `tls.client_ca_file` is set, require a client cert. If `auth.mtls_principal_from_cn=true`, the cert CN maps to a principal, and Basic auth may then be omitted.
- **Loopback/sidecar:** `auth.mode=loopback-secret` is permitted **only** when the listener is bound to `127.0.0.1`/`::1`. The server generates a random one-time password at startup, and any username is accepted with it. The server must refuse to start if this mode is used on a non-loopback address.
- `auth.mode=none` exists **only** for tests and must also be loopback-only. Log a loud warning when it's used.
- Never log `Authorization` headers, passwords or tokens.

### 5.2 snowstate → Snowflake (shared with the provider via `pkg/snowstate/sfconn`)

Config `snowflake.auth`:

| Mode | Use | Config |
|---|---|---|
| `keypair` (recommended for server) | Service user with an RSA key | `private_key_path` or `private_key_pem` (env), optional `private_key_passphrase` |
| `pat` | Programmatic access token | `token` (env `SNOWSTATE_SNOWFLAKE_TOKEN`) |
| `oauth` | Externally obtained OAuth token | `token` / `token_file_path` (re-read on change) |
| `externalbrowser` (sidecar only) | Developer SSO on a laptop | none. Uses the driver's browser flow. Cache the token per the driver's options. |
| `workload_identity` (recommended for CI) | Secretless login with a short-lived OIDC token, e.g. GitHub Actions | `wif.provider` (`oidc`; `aws`/`azure`/`gcp` are optional extras), `wif.token_source` (`github` \| `file` \| `env`), `wif.audience` (default `snowflakecomputing.com`), `wif.token_file_path`, `wif.token_env`. With `token_source=github`, fetch a fresh token for **each new Snowflake connection** from `$ACTIONS_ID_TOKEN_REQUEST_URL&audience=<aud>` with `Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN`. If those env vars are missing, fail with: "add `permissions: id-token: write` to the job". Set `CLIENT_SESSION_KEEP_ALIVE=true` so long applies keep their session. VERIFY the gosnowflake config field names for WIF (authenticator constant, provider, token) and record them. |
| `password` | Discouraged, but supported for trials | `user` + `password` |

Plus `account`, `user`, `role`, `warehouse`, `database`, `schema`, `query_tag` (default `snowstate`). Set `QUERY_TAG` on every session. Use a connection pool (`max_open=10`, `max_idle=2`, `conn_max_lifetime=1h`, all configurable).

`sfconn` exposes a plain struct (`sfconn.Config`) and `sfconn.Open(ctx, cfg) (*sql.DB, error)`, with no dependency on koanf or cobra. That way the provider can fill it from HCL attributes and env vars (`SNOWFLAKE_ACCOUNT`, `SNOWFLAKE_USER`, `SNOWFLAKE_PRIVATE_KEY_PATH`, … mirroring the official Snowflake provider's env names, VERIFY).

### 5.3 Authorization modes: gateway vs direct (required for G7)

snowstate is used in two trust models. The core must support both.

| Mode | Who connects to Snowflake | Who enforces per-project access | Used by |
|---|---|---|---|
| **Gateway** | The snowstate server, as one service role | snowstate token scopes (§5.1) | `serve` with `auth.mode=token` or mTLS |
| **Direct** | The person or CI job running Terraform, with **their own** Snowflake credentials | **Snowflake grants** | `exec` sidecar, the M7 provider, the admin CLI |

Implementation:

- `service` takes an `Authorizer` interface (`Authorize(ctx, Principal, action, key) error`).
  - Gateway mode plugs in the token-scope authorizer.
  - Direct mode plugs in a no-op authorizer, because Snowflake is the enforcement point. Its errors (insufficient privileges) are mapped to `ErrForbidden`.
- Config `tenancy.mode`:
  - **`shared`** (default): all projects in one schema (`TF_STATE.BACKEND`). Use this for gateway mode, or for direct mode when everyone may access every state.
  - **`schema_per_project`**: each project gets its own schema, `TF_STATE.P_<PROJECT>` (upper-cased, non-alphanumerics → `_`, VERIFY identifier rules), containing `STATES`, `STATE_VERSIONS`, `LOCKS`, `AUDIT_LOG` and `STATE_BLOBS`. The control objects (`TOKENS`, `SCHEMA_MIGRATIONS`, `PROJECTS`) stay in `TF_STATE.CONTROL`. The store resolves the schema from the key's first segment. This is the **only** way direct mode can isolate teams, because schema grants also restrict `INSERT`. Row access policies don't restrict `INSERT`, and may not be supported on hybrid tables, so they're **not** used for isolation. You may add them later, read-only, for the inventory view.
- `snowstate project create <name> --role <SF_ROLE> [--permissions read,write,lock]` (schema_per_project only). It creates the schema by applying the migrations, records the project in `CONTROL.PROJECTS`, grants the role, and regenerates a cross-project `CONTROL.STATE_RESOURCES` union view. `snowstate migrate` upgrades every project schema.
- **Direct-mode grants are append-only for history.** Project roles get:
  - `SELECT, INSERT` on `STATE_VERSIONS` and `AUDIT_LOG` (no `UPDATE`/`DELETE`);
  - `SELECT, INSERT, UPDATE` on `STATES`;
  - `SELECT, INSERT, DELETE` on `LOCKS`;
  - `READ, WRITE` on the stage.

  GC, restore-overwrite and token admin need the Install's admin role (`<DB>_ADMIN`, §10). So even a client with direct credentials can't erase history.
- **Audit in direct mode** is written by the client and is therefore advisory. Document `SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY` / `ACCESS_HISTORY`, filtered by the `snowstate:` query tag, as the authoritative trail.
- The Foundation (§10) provides `shared` tenancy. `schema_per_project` setup is in `docs/operator-guide.md`, marked unverified, until its known login failure is fixed and its role isolation is verified.

---

## 6. Snowflake data model

Default namespace is `TF_STATE.BACKEND` (configurable). All DDL lives in embedded migrations, applied by `snowstate migrate` and tracked in `SCHEMA_MIGRATIONS`. Migrations must be idempotent and forward-only.

### 6.1 Tables

```sql
-- Current pointer per state (standard table)
CREATE TABLE IF NOT EXISTS STATES (
  STATE_KEY           VARCHAR(512)  NOT NULL PRIMARY KEY,   -- informational on std tables
  PROJECT             VARCHAR(128)  NOT NULL,
  CURRENT_VERSION_ID  VARCHAR(36),
  SERIAL              NUMBER(38,0),
  LINEAGE             VARCHAR(64),
  TF_VERSION          VARCHAR(32),
  STATE_MD5           VARCHAR(32),
  SIZE_BYTES          NUMBER(38,0),
  CREATED_AT          TIMESTAMP_TZ  NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  UPDATED_AT          TIMESTAMP_TZ  NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  UPDATED_BY          VARCHAR(256),
  DELETED_AT          TIMESTAMP_TZ
);

-- Immutable history (standard table)
CREATE TABLE IF NOT EXISTS STATE_VERSIONS (
  VERSION_ID      VARCHAR(36)   NOT NULL PRIMARY KEY,       -- UUIDv7 (time-ordered)
  STATE_KEY       VARCHAR(512)  NOT NULL,
  SERIAL          NUMBER(38,0)  NOT NULL,
  LINEAGE         VARCHAR(64)   NOT NULL,
  TF_VERSION      VARCHAR(32),
  STATE_MD5       VARCHAR(32)   NOT NULL,                   -- hex md5 of uncompressed bytes
  SIZE_BYTES      NUMBER(38,0)  NOT NULL,                   -- uncompressed
  STORAGE         VARCHAR(10)   NOT NULL,                   -- 'INLINE' | 'STAGE'
  CONTENT_GZIP    BINARY(67108864),                         -- when INLINE
  STAGE_PATH      VARCHAR(1024),                            -- when STAGE
  LOCK_ID         VARCHAR(128),
  CREATED_AT      TIMESTAMP_TZ  NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  CREATED_BY      VARCHAR(256),
  RESTORED_FROM   VARCHAR(36)
) CLUSTER BY (STATE_KEY);

-- Locks (HYBRID table: enforced PK gives atomic acquisition).
-- Frontend-neutral: normalised columns are authoritative; LOCK_INFO is HTTP-only.
CREATE HYBRID TABLE IF NOT EXISTS LOCKS (
  STATE_KEY    VARCHAR(512)  NOT NULL PRIMARY KEY,
  LOCK_ID      VARCHAR(128)  NOT NULL,        -- HTTP: Terraform's ID; PSS: service-generated UUIDv4
  SOURCE       VARCHAR(8)    NOT NULL,        -- 'HTTP' | 'PSS' | 'CLI'
  OPERATION    VARCHAR(64),
  WHO          VARCHAR(512),
  INFO         VARCHAR(4096),
  TF_VERSION   VARCHAR(32),
  LOCK_PATH    VARCHAR(1024),
  LOCK_CREATED TIMESTAMP_TZ,                  -- client-reported creation time
  LOCK_INFO    VARCHAR(65536),                -- raw Terraform LockInfo JSON (HTTP only, NULL otherwise)
  ACQUIRED_AT  TIMESTAMP_TZ  NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  ACQUIRED_BY  VARCHAR(256)                   -- snowstate principal or Snowflake CURRENT_USER()
);

CREATE TABLE IF NOT EXISTS TOKENS (
  TOKEN_ID     VARCHAR(36)  NOT NULL PRIMARY KEY,
  PRINCIPAL    VARCHAR(256) NOT NULL,
  TOKEN_HMAC   VARCHAR(64)  NOT NULL,
  SCOPES       VARIANT      NOT NULL,   -- [{"key_pattern":"platform/*","permissions":["read","write","lock"]}]
  DESCRIPTION  VARCHAR(1024),
  CREATED_AT   TIMESTAMP_TZ NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  CREATED_BY   VARCHAR(256),
  EXPIRES_AT   TIMESTAMP_TZ,
  REVOKED_AT   TIMESTAMP_TZ,
  LAST_USED_AT TIMESTAMP_TZ           -- updated at most once per 5 min per token
);

CREATE TABLE IF NOT EXISTS AUDIT_LOG (
  EVENT_ID     VARCHAR(36)  NOT NULL,
  EVENT_TIME   TIMESTAMP_TZ NOT NULL DEFAULT CURRENT_TIMESTAMP(),
  REQUEST_ID   VARCHAR(64),
  PRINCIPAL    VARCHAR(256),
  REMOTE_ADDR  VARCHAR(64),
  ACTION       VARCHAR(32)  NOT NULL,  -- GET|PUT|PUT_NOOP|DELETE|LOCK|UNLOCK|FORCE_UNLOCK|RESTORE|TOKEN_CREATE|TOKEN_REVOKE|GC
  STATE_KEY    VARCHAR(512),
  VERSION_ID   VARCHAR(36),
  LOCK_ID      VARCHAR(128),
  RESULT       VARCHAR(16)  NOT NULL,  -- OK|DENIED|CONFLICT|ERROR
  HTTP_STATUS  NUMBER(3,0),
  DETAIL       VARIANT
);

CREATE TABLE IF NOT EXISTS SCHEMA_MIGRATIONS (
  VERSION NUMBER NOT NULL PRIMARY KEY, NAME VARCHAR, APPLIED_AT TIMESTAMP_TZ DEFAULT CURRENT_TIMESTAMP()
);

CREATE STAGE IF NOT EXISTS STATE_BLOBS
  ENCRYPTION = (TYPE = 'SNOWFLAKE_SSE')
  COMMENT = 'Large terraform state blobs (gzip)';
```

Audit writes for `GET` are configurable (`audit.log_reads`, default `true`). Audit inserts may be **batched asynchronously** (flush every 2 s or 100 events). The audit row for `PUT`, `DELETE`, `LOCK`, `UNLOCK`, `FORCE_UNLOCK` and `RESTORE` must instead be written **in the same transaction** as the change.

### 6.2 Inventory view (bonus value, required in v1)

```sql
CREATE OR REPLACE SECURE VIEW STATE_RESOURCES AS
SELECT s.STATE_KEY, s.PROJECT, v.VERSION_ID, v.SERIAL,
       r.value:mode::string     AS MODE,
       r.value:type::string     AS RESOURCE_TYPE,
       r.value:name::string     AS RESOURCE_NAME,
       r.value:module::string   AS MODULE,
       r.value:provider::string AS PROVIDER,
       ARRAY_SIZE(r.value:instances) AS INSTANCE_COUNT
FROM STATES s
JOIN STATE_VERSIONS v ON v.VERSION_ID = s.CURRENT_VERSION_ID
, LATERAL FLATTEN(input => PARSE_JSON(DECOMPRESS_STRING(v.CONTENT_GZIP, 'GZIP')):resources) r
WHERE s.DELETED_AT IS NULL AND v.STORAGE = 'INLINE';
```

This view deliberately **omits instance attributes**, because they can contain secrets. VERIFY that `DECOMPRESS_STRING(..., 'GZIP')` accepts Go's `gzip` output. If it doesn't, switch the inline encoding to one Snowflake can decompress and record that in `decisions.md`.

### 6.3 Blob storage

- Always gzip state bytes (`gzip.BestSpeed`). Compute MD5 and size on the **uncompressed** bytes.
- If the compressed size ≤ `storage.inline_max_bytes` (default **8 MiB**), store it INLINE in `CONTENT_GZIP`.
- Otherwise `PUT` it to `@STATE_BLOBS/<state_key>/<version_id>.json.gz` (gosnowflake stream PUT, `AUTO_COMPRESS=FALSE`, `SOURCE_COMPRESSION=GZIP`) and set `STORAGE='STAGE'`. Read it back with `GET` into a temp dir or stream. Upload the blob **before** the metadata transaction. If the transaction fails, best-effort `REMOVE` the orphan, and GC also sweeps orphans.
- VERIFY the maximum practical bind size for a BINARY parameter in gosnowflake. If binding 8 MiB is a problem, lower the default threshold.
- `limits.max_state_bytes` (uncompressed) defaults to **256 MiB**.

### 6.4 Write transaction (PUT)

```
BEGIN
  -- 1. lock check (hybrid table point read)
  SELECT LOCK_ID, LOCK_INFO FROM LOCKS WHERE STATE_KEY = ?;
     -> apply §4.3 rules; on violation ROLLBACK + respond
  -- 2. idempotency
  SELECT CURRENT_VERSION_ID, STATE_MD5 FROM STATES WHERE STATE_KEY = ?;
     -> if md5 equal and not deleted: ROLLBACK, audit PUT_NOOP, 200
  -- 3. insert version
  INSERT INTO STATE_VERSIONS (...) VALUES (...);
  -- 4. move pointer
  MERGE INTO STATES t USING (SELECT ? AS STATE_KEY ...) s ON t.STATE_KEY = s.STATE_KEY
    WHEN MATCHED THEN UPDATE SET CURRENT_VERSION_ID=..., SERIAL=..., ..., DELETED_AT=NULL
    WHEN NOT MATCHED THEN INSERT (...);
  -- 5. audit row
  INSERT INTO AUDIT_LOG (...);
COMMIT
```

Use `database/sql` transactions with gosnowflake (it supports multi-statement transactions over one connection). Also set `LOCK_TIMEOUT` for the session (default **30 s**, configurable) so a stuck transaction fails fast instead of waiting the Snowflake default of 12 h.

**Serial guard (configurable):** `state.reject_serial_regression` (default `false`). When `true`, reject with `409` a write whose `serial` is lower than the current serial with the same lineage. Always log a warning on regression or on a lineage change.

### 6.5 Locking algorithms

**Write fencing (both frontends):** `service.Put`/`Delete` always receive the caller's lock ID (HTTP: the `?ID=` query param; provider: the ID it got from its own `Lock` call, held in process because Terraform's `Write` RPC doesn't carry it). The transaction in §6.4 checks it against `LOCKS` no matter which frontend took the lock. That's what makes an HTTP lock block a provider write and the reverse.

**Mode `hybrid` (default, required for production):**

- Acquire: `INSERT INTO LOCKS (STATE_KEY, LOCK_ID, LOCK_INFO, ...) VALUES (...)` in autocommit.
  - On success, return `200`.
  - On a primary-key violation, `SELECT` the existing row. If it has the same `LOCK_ID`, return `200`. Otherwise return `423` + `LOCK_INFO`.
  - VERIFY the exact Snowflake error code or SQLSTATE for a hybrid PK violation and match on that code, not on message text.
- Release: `DELETE FROM LOCKS WHERE STATE_KEY=? AND LOCK_ID=?`. If 1 row, `200`. If 0 rows, `SELECT` to tell "no lock" (`200`) from "different holder" (`409`).
- Force: `DELETE FROM LOCKS WHERE STATE_KEY=?` and audit the removed row.

**Mode `standard` (fallback for accounts without hybrid tables, EXPERIMENTAL):**

- `LOCKS` is created as a standard table instead.
- Acquire inside one transaction:
  `MERGE` to seed an empty row if absent → `UPDATE LOCKS SET LOCK_ID=?, ... WHERE STATE_KEY=? AND LOCK_ID IS NULL` → `SELECT LOCK_ID` and confirm it equals ours → `COMMIT`. If the confirmation fails, `ROLLBACK` and return `423`.
- Snowflake only provides READ COMMITTED and table-level DML locks on standard tables, and **it isn't certain** that a blocked UPDATE re-evaluates its predicate against newly committed data. So:
  - The server refuses to start in `standard` mode unless `locking.standard_mode_acknowledged=true`.
  - The integration suite **must** include the concurrency test (§12.3) for standard mode. Record the result in `decisions.md`. If it ever lets two winners through, document `standard` as unsafe and leave it disabled.
- `snowstate migrate` detects whether hybrid tables are available (try creating a temp hybrid table) and prints clear guidance.

**Lock TTL (optional):** `locking.ttl` (default `0` = disabled). If > 0, a lock older than the TTL can be stolen by a new LOCK request, and that's audited as `FORCE_UNLOCK` with reason `ttl`. Keep it off by default, because Terraform has no heartbeat and long applies are legitimate.

---

## 7. Service behaviour details

- **Timeouts:** Each HTTP request gets a context with `server.request_timeout` (default 60 s), propagated to all SQL. Server read and write timeouts default to 5 min to allow large bodies.
- **Body handling:** Stream the request body through `io.LimitReader(max+1)`, compute MD5 while reading, and reject on overflow.
- **JSON validation:** Decode just enough to read `version`, `terraform_version`, `serial`, `lineage`. **Never re-serialize.** Store the original bytes.
- **Retries to Snowflake:** Retry transient errors (network, `390114` session expired, warehouse resume timeouts) up to 3 times with jittered backoff. Only retry a write transaction if it never reached COMMIT, and never retry after COMMIT has been sent if its outcome is unknown. In that case re-read: if the version exists, return 200.
- **Concurrency:** The server is stateless apart from caches, so several replicas behind a load balancer are safe because correctness lives in Snowflake.
- **Warehouse:** Every op needs a running warehouse. Recommend a dedicated X-Small with `AUTO_SUSPEND=60`, `AUTO_RESUME=TRUE`. Document the cost: one cold resume bills 60 s ≈ 0.017 credits.
- **Graceful shutdown:** On SIGTERM, stop accepting connections, drain in-flight requests (30 s), flush the audit buffer, then close the pool.

---

## 8. CLI

```
snowstate serve      [--config file] [flags]        # server mode
snowstate exec       [--config file] -- <cmd> [args] # sidecar mode
snowstate seed       --public-key-file file          # print the Seed SQL, never run it (§8.5)
snowstate install    --database name [--dir path] [--auto-approve] [--binary terraform|tofu]   # create or update an Install (§8.5)
snowstate migrate    [--dry-run]                     # create/upgrade schema
snowstate doctor                                     # connectivity, grants, hybrid support, warehouse, round-trip test
snowstate token create  --principal NAME --scope 'platform/*:read,write,lock' [--expires 90d] [--description ..]
snowstate token list | revoke <token-id>
snowstate state list    [--prefix p]
snowstate state versions <key> [--limit n]
snowstate state pull    <key> [--version id] [-o file]
snowstate state restore <key> --version id           # creates a NEW version copying old content (RESTORED_FROM set); refuses if locked unless --force
snowstate lock list
snowstate lock release <key> [--force]
snowstate project create <name> --role SF_ROLE [--permissions read,write,lock]   # schema_per_project only (§5.3)
snowstate project list
snowstate gc         [--dry-run]                     # retention + orphan blob sweep
snowstate version
```

The admin CLI talks to Snowflake **directly** using the same config (it doesn't go through the HTTP API). `token create` prints the token once.

### 8.1 Sidecar mode (`exec`)

0. Sidecar mode is a **direct-mode** client (§5.3). It connects with the caller's own Snowflake credentials, and Snowflake grants decide what it can touch. Token scopes don't apply.
1. Load config and connect to Snowflake (fail fast with a clear message).
2. Listen on `127.0.0.1:0` (a random free port). Auth mode is forced to `loopback-secret`.
3. Determine the state key from `--key` or `SNOWSTATE_KEY`. Error if neither is set.
4. Run the child with the inherited env plus `TF_HTTP_ADDRESS`, `TF_HTTP_LOCK_ADDRESS`, `TF_HTTP_UNLOCK_ADDRESS` (= `http://127.0.0.1:<port>/v1/states/<key>`), `TF_HTTP_USERNAME=sidecar`, `TF_HTTP_PASSWORD=<one-time secret>`. Forward stdin/stdout/stderr, forward SIGINT/SIGTERM to the child, and exit with the child's exit code.
5. The root module must declare `backend "http" {}` (empty, partial config). Document this.
6. On child exit, shut down gracefully. **If a lock is still held by this sidecar** (the child crashed), log a warning, but **don't** auto-release it. Print the `snowstate lock release` command instead. Auto-release is opt-in via `--release-stale-lock`.

7. **Saved-plan portability (required).** The sidecar passes its address and password **only** via `TF_HTTP_*` env vars and never writes them to the backend block, `-backend-config`, or `.terraform/`. Terraform's http backend reads these env vars when it connects (`internal/backend/remote-state/http/backend.go`, `GetAttrEnvDefaultFallback`). A saved plan records only the explicit `backend` block, so `terraform apply tfplan` in a different process, with a new sidecar on a different port and password, must work. This is covered by e2e scenario 9.
8. **Abort behaviour.**
   - On SIGINT or SIGTERM, forward the signal to the child and keep the sidecar serving until the child exits, so Terraform can still write state and unlock during a graceful stop.
   - Only after the child exits (or after `--shutdown-grace`, default 20 s, following a second signal) does the server stop.
   - If the child is killed while holding a lock, the lock stays held (see step 6). An unknown partial apply must not be silently unlocked.
   - Print a clear message on stderr naming the lock ID and the exact `snowstate lock release` command.
9. **CI context.** When `GITHUB_ACTIONS=true`, the sidecar:
   - adds `{repository, workflow, job, run_id, run_attempt, actor, sha, ref, run_url}` to the `DETAIL` of every audit row;
   - sets `ACQUIRED_BY` on locks it proxies to `github:<repository>#<run_id>/<run_attempt>`;
   - appends a short Markdown summary to `$GITHUB_STEP_SUMMARY`: state key, version before and after (serial, version ID), lock acquired and released, and any conflict with the holder's `run_url` when known.

   Detection is generic. A `ci` package abstraction lets GitLab and others be added later.

### 8.2 `doctor`

Checks and prints pass/fail for: config parse, Snowflake auth, role/warehouse/database/schema usage, migration version current, hybrid table support, and a privilege round trip (write and delete a temp row in a scratch key `__doctor__/<uuid>`). It exits non-zero on failure.

### 8.3 Admin HTTP API (requires `admin` permission)

`GET /v1/admin/states?prefix=`, `GET /v1/admin/states/{key...}/versions`, `GET /v1/admin/states/{key...}/versions/{id}` (raw content), `POST /v1/admin/states/{key...}/restore {"version_id":...}`, `GET /v1/admin/locks`, `DELETE /v1/admin/locks/{key...}`. All return JSON and are audited.

### 8.4 GitHub Actions integration

GitHub Actions is a first-class target. The recommended pattern is **sidecar mode + workload identity federation**: no snowstate server, and no stored Snowflake secret.

**8.4.1 Snowflake setup (in `docs/operator-guide.md`, marked unverified, and not in the Foundation yet)**

```sql
-- One service user per repo+environment (or per repo, using a customised `sub` claim)
CREATE USER GH_PLATFORM_NETWORK_PROD
  TYPE = SERVICE
  DEFAULT_ROLE = <DB>_P_PLATFORM          -- project role (§5.3) or <DB>_SVC (shared tenancy, §10)
  DEFAULT_WAREHOUSE = <DB>_WH
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod'   -- must equal the token's `sub`
  );
GRANT ROLE <DB>_P_PLATFORM TO USER GH_PLATFORM_NETWORK_PROD;
```

Document:
- how GitHub's `sub` claim is formed for branches, environments and pull requests;
- that binding to an **environment** (with required reviewers) is the safest choice for apply;
- **separate plan and apply identities.** `terraform plan` takes a lock but shouldn't write state, so plan-on-PR workflows use a **plan role** with `read,lock` permissions:
  - Snowflake grants: `SELECT` on all tables, `INSERT, DELETE` on `LOCKS`, `INSERT` on `AUDIT_LOG`, no write on `STATES`, `STATE_VERSIONS` or the stage.
  - Only the `environment: prod` apply job maps to a `read,write,lock` role.
  - A plan identity can still hold a lock and block others, but it can't change state. Note that trade-off.
  - VERIFY that `terraform plan` never calls the state write endpoint (including with `-refresh-only`), and record the result. If it does, the plan role gets `403`, and the docs must say so.
  - `project create` gains `--plan-role` to create both roles.

The key-pair alternative (private key stored as a GitHub secret, `auth=keypair`, `private_key_pem` from env) must also be documented.

**8.4.2 Network access.** GitHub-hosted runners have no fixed IPs. `docs/github-actions.md` must explain the three options when the account uses network policies:
1. allow GitHub's published Actions IP ranges (large, and they change);
2. GitHub larger runners with static IPs (paid plans, VERIFY);
3. self-hosted runners inside the corporate network.

`snowstate doctor` must detect a network-policy rejection and print this guidance.

**8.4.3 Setup action.** Ship a composite action in this repo at `action/setup/action.yml`, used as `uses: <owner>/snowstate/action/setup@v1`:
- Inputs are `version` (default: the action's own tag) and `verify-checksum` (default true).
- It downloads the release binary for the runner OS and arch (linux/darwin/windows, amd64/arm64), verifies it against `checksums.txt` (and the cosign signature if release signing is enabled), adds it to `PATH`, and outputs `path` and `version`.
- No Node runtime is needed (bash + PowerShell steps).
- Cache the binary with `actions/cache` keyed on version.

**8.4.4 Reference workflows** in `examples/github-actions/`, which the repo's own CI must exercise (§12.2 scenario 10):

`terraform.yml`, plan on PR and plan → approve → apply on `main`:

```yaml
name: terraform
on:
  pull_request:
  push: { branches: [main] }

permissions: { contents: read, id-token: write, pull-requests: write }

env:
  SNOWSTATE_KEY: platform/network/prod
  SNOWSTATE_SNOWFLAKE__ACCOUNT: ${{ vars.SNOWFLAKE_ACCOUNT }}
  SNOWSTATE_SNOWFLAKE__AUTH: workload_identity
  SNOWSTATE_SNOWFLAKE__WIF__TOKEN_SOURCE: github

jobs:
  plan:
    runs-on: ubuntu-latest
    environment: prod-plan            # bound to the read,lock Snowflake user
    concurrency: { group: tf-${{ github.workflow }}-prod, cancel-in-progress: false }
    steps:
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
        with: { terraform_wrapper: false }
      - uses: <owner>/snowstate/action/setup@v1
      - run: snowstate exec -- terraform init -input=false
      - run: snowstate exec -- terraform plan -input=false -out=tfplan
      - if: github.event_name == 'push'
        uses: actions/upload-artifact@v4
        with: { name: tfplan, path: tfplan, retention-days: 1 }   # contains sensitive values

  apply:
    if: github.event_name == 'push'
    needs: plan
    runs-on: ubuntu-latest
    environment: prod                 # required reviewers; bound to the read,write,lock user
    concurrency: { group: tf-${{ github.workflow }}-prod, cancel-in-progress: false }
    timeout-minutes: 60
    steps:
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
        with: { terraform_wrapper: false }
      - uses: <owner>/snowstate/action/setup@v1
      - uses: actions/download-artifact@v4
        with: { name: tfplan }
      - run: snowstate exec -- terraform init -input=false
      - run: snowstate exec -- terraform apply -input=false tfplan
      - if: failure() || cancelled()
        uses: actions/upload-artifact@v4
        with: { name: errored-state, path: errored.tfstate, if-no-files-found: ignore, retention-days: 7 }
```

Points the docs must explain:
- **Use `concurrency` with `cancel-in-progress: false`** for anything that applies. It queues runs per state rather than letting them race, and it avoids cancelling a running apply.
- **Saved plans are safe across jobs** (§8.1 step 7). If the state changed between plan and apply, Terraform rejects the stale plan, which is expected and correct.
- **Cancellation:**
  - GitHub stops a cancelled step with escalating signals and then a hard kill after a short grace period (VERIFY the current timings and record them).
  - The sidecar forwards signals and stays up while Terraform stops gracefully (§8.1 step 8).
  - If the job is killed mid-apply, the lock stays held on purpose, and Terraform may leave `errored.tfstate`. The runbook covers recovery: download the artifact, `terraform state push errored.tfstate` through a sidecar, then `snowstate lock release <key>`, with the run URL from `lock list` identifying the culprit.
- **`terraform_wrapper: false`** keeps exit codes and signals clean.
- **Cost:** about one warehouse resume per job (≈ 0.017 credits on X-Small).

**8.4.5 Server mode from Actions (alternative).** If a shared `snowstate serve` exists, workflows can skip the sidecar. They set `TF_HTTP_ADDRESS`/`TF_HTTP_USERNAME` and pass `TF_HTTP_PASSWORD` from a GitHub secret (a scoped token, §5.1). The runner must be able to reach the server. Document this, but recommend the sidecar.

### 8.5 Install (`seed`, `install`)

`snowstate` orchestrates the real Terraform and never reimplements it. Every step stays documented and runnable by hand, so recovery never depends on the CLI.

- **`seed`** prints the Seed SQL with the public key filled in. It never connects to Snowflake, because the Seed needs `ACCOUNTADMIN`. It rejects a private key rather than echoing it.
- **`install`** creates an Install, or updates its Foundation. The Terraform code is embedded in the binary and written to a directory the operator keeps (`--dir`, default `./snowstate-foundation`). Steps, in order:
  1. Write the code, with the backend block disabled. If the shipped files in the directory differ from the embedded code, stop and show the difference. Never overwrite them. The enabled backend block counts as its disabled form, and generated or operator files, such as `.terraform.lock.hcl` and `terraform.tfvars`, are ignored.
  2. Show the plan and apply with local state, as the Seed role. Nothing is applied without approval unless `--auto-approve` is passed.
  3. Run `migrate` as `<DB>_ADMIN`.
  4. Run `doctor` as `<DB>_SVC`, so the runtime role's grants are proven before anything depends on them.
  5. Only after `doctor` passes, enable the backend block and move the state into snowstate with `init -migrate-state`, as `<DB>_SVC`, under the key `snowstate/foundation/default`.
  6. Remove the local state file and its backup.
- **Re-running** is safe at any point. `install` works out what is done and continues. Once state is in snowstate, it plans and applies through the sidecar, so a re-run is also how an operator makes later Foundation changes.
- Each step uses its own role, with one user, `SNOWSTATE_SEED`. A failed step says which step failed and what to do next. A missing Terraform or OpenTofu binary fails before anything changes.
- **Teardown is manual on purpose:** move the state back to local, remove `prevent_destroy`, then run `terraform destroy`. Destroying with state in snowstate would remove the state mid-run.

---

## 9. Configuration

YAML file (`--config`, default `./snowstate.yaml` if present). Env vars use the `SNOWSTATE_` prefix with `__` for nesting (e.g. `SNOWSTATE_SNOWFLAKE__ACCOUNT`). Flags override env, which overrides the file. Provide `snowstate.example.yaml` with every key documented.

```yaml
server:
  listen: ":8443"
  request_timeout: 60s
  read_timeout: 5m
  write_timeout: 5m
  trusted_proxies: []          # CIDRs allowed to set X-Forwarded-For
tls:
  cert_file: ""                # required unless listen is loopback or tls.disable_for_testing
  key_file: ""
  client_ca_file: ""           # enables mTLS
  disable_for_testing: false
auth:
  mode: token                  # token | loopback-secret | none
  pepper_env: SNOWSTATE_TOKEN_PEPPER   # 32+ byte secret used for token HMAC (required in token mode)
  mtls_principal_from_cn: false
snowflake:
  account: ""
  user: ""
  role: TF_STATE_SVC           # <DB>_SVC, §10
  warehouse: TF_STATE_WH       # <DB>_WH, §10
  database: TF_STATE
  schema: BACKEND
  auth: keypair                # keypair | pat | oauth | externalbrowser | password | workload_identity
  wif:
    provider: oidc             # oidc (required) | aws | azure | gcp (optional)
    token_source: github       # github | file | env
    audience: snowflakecomputing.com
    token_file_path: ""
    token_env: ""
  client_session_keep_alive: true
  private_key_path: ""
  private_key_passphrase_env: ""
  token_env: SNOWSTATE_SNOWFLAKE_TOKEN
  token_file_path: ""
  query_tag: snowstate
  lock_timeout: 30s
  pool: { max_open: 10, max_idle: 2, conn_max_lifetime: 1h }
tenancy:
  mode: shared                 # shared | schema_per_project  (§5.3)
  control_schema: CONTROL      # used by schema_per_project
locking:
  mode: hybrid                 # hybrid | standard
  standard_mode_acknowledged: false
  allow_force_unlock: true
  ttl: 0s
state:
  require_lock_for_write: false
  reject_serial_regression: false
storage:
  inline_max_bytes: 8388608
limits:
  max_state_bytes: 268435456
retention:
  keep_versions: 100           # per state, newest kept; 0 = unlimited
  keep_days: 0                 # additionally keep everything newer than N days
  deleted_days: 30
audit:
  log_reads: true
metrics:
  listen: ""                   # empty = serve on main listener
log:
  level: info
  format: json
```

Validate on startup and fail with an aggregated list of errors.

---

## 10. Security requirements

1. TLS is required on non-loopback listeners (min TLS 1.2). Refuse to start otherwise.
2. Tokens are hashed with HMAC-SHA256 plus a pepper, compared in constant time, and never logged. Revocation takes effect within the cache TTL (60 s).
3. State bodies are never logged at any level. Log size, MD5 and serial only.
4. Rate-limit failed auth per remote IP (default 10 per minute, then `429`).
5. Set up an Install with a Seed and a Foundation (§8.5). An Install is identified by its `database_name`, so one account can hold several:
   - **Seed**, run by an admin once per account: role and service user `SNOWSTATE_SEED` (key-pair, `TYPE = SERVICE`, `DEFAULT_SECONDARY_ROLES = ()`, so a session has only the role it asked for). The role holds `CREATE WAREHOUSE`, `CREATE DATABASE`, `CREATE ROLE`, `CREATE USER` and `MANAGE GRANTS` on the account, and is granted to `SYSADMIN`. The user is for Foundation changes only and never goes on a server.
   - **Foundation**, one Terraform root module: warehouse, database, managed-access schema, roles and grants. `database_name` is required, the schema defaults to `BACKEND`, and account-level names derive from the database name, such as `<DB>_ADMIN`, and can be overridden. `prevent_destroy` is set on the database and schema from the first apply. Its outputs give the settings and commands for later steps, with no secrets.
   - Warehouse `<DB>_WH`: X-Small, auto-suspend 60.
   - Role `<DB>_ADMIN`: creates and owns schema objects, and runs `migrate`. It gets `CREATE TABLE`, `CREATE HYBRID TABLE`, `CREATE STAGE` and `CREATE VIEW` on the schema, and is granted to both the role and the user `SNOWSTATE_SEED`, which keeps the Install under `SYSADMIN`. The Seed role keeps schema ownership. `migrate` owns tables, stage and view, and Terraform never defines them.
   - Role `<DB>_SVC` (runtime): `SELECT/INSERT/UPDATE/DELETE` on the tables, `READ/WRITE` on the stage, `USAGE` on the warehouse, database and schema. No DDL. Table and stage access are future grants, applied before `migrate`. It is granted to `<DB>_ADMIN`, which is how the user `SNOWSTATE_SEED` reaches it.
   - Role `<DB>_READER`: `USAGE` plus future `SELECT` on views, so it reaches `STATE_RESOURCES` with no raw state. Any future view must also be free of raw state.
   - Service user `<DB>_SERVICE`, created only when a public key is passed, with secondary roles off. Terraform takes public keys as variables and never generates a private key.
   - A sample network policy in `docs/operator-guide.md`, not in the Foundation.
6. Container runs as non-root on a read-only root filesystem.
7. `govulncheck` passes in CI.
8. Document that state contains secrets, so access to `STATE_VERSIONS` and the stage must be restricted. Mention OpenTofu's client-side state encryption as compatible defence in depth. snowstate stores whatever bytes it receives, so encrypted states still work, and the inventory view will simply be empty for them.

---

## 11. Observability

- **Logs** (`slog` JSON): one line per request with `request_id`, `principal`, `method`, `state_key`, `status`, `duration_ms`, `sf_query_ids` (from gosnowflake context), `bytes`.
- **Metrics:**
  - `snowstate_http_requests_total{method,route,status}`
  - `snowstate_http_request_duration_seconds{method,route}`
  - `snowstate_snowflake_query_duration_seconds{op}`
  - `snowstate_snowflake_errors_total{op,code}`
  - `snowstate_lock_conflicts_total`
  - `snowstate_state_bytes{quantile}` (summary)
  - `snowstate_audit_buffer_depth`
- Every SQL statement sets `QUERY_TAG` to `snowstate:<op>:<request_id>`, so you can trace it in `QUERY_HISTORY`.

---

## 12. Testing requirements

### 12.1 Unit (no network), target ≥ 85% coverage on `service`, `api`, `auth`

- `install`: fakes behind the engine's interface cover the role at each step, resuming from each step, no apply without approval, stopping on differing code, and clear errors at each failure.
- `seed`: output with the key filled in, and errors for a bad key file.
- The embedded Foundation code matches its source.

- Protocol table tests covering **every row of §4.3**, using `store/memory`.
- Key validation, scope matching, token HMAC, config validation, MD5 handling, size limits, idempotent PUT, force-unlock paths, serial-regression guard.
- `store/memory` must implement the same semantics as Snowflake (lock atomicity via a mutex).
- `TestImportBoundaries` (§3). `pkg/snowstate/keys` tests for the prefix/workspace convention, including `default` and listing only direct children. Service-level tests that drive the **same** scenario through a fake "PSS-style" caller (service-generated lock ID, no raw JSON) and an HTTP-style caller, and check that each blocks the other's writes.

### 12.2 E2E with real Terraform and OpenTofu (`make e2e`, runs in CI, no Snowflake needed)

Use `store/memory` behind a real `snowstate serve` on loopback. Download pinned binaries: Terraform 1.5.x, the latest 1.x, and OpenTofu (the latest). The fixture module uses only the `terraform_data`, `random` and `local` providers. Scenarios:

1. `init` → `apply` → `plan` (no changes) → `apply` (change) → `state list` / `state pull` / `state push`.
2. Two concurrent `apply`s on the same key: exactly one succeeds, and the other fails with a lock error naming the holder. The test must assert on the error text.
3. Kill `apply` mid-run (SIGKILL), confirm the lock is held, `terraform force-unlock <id>` succeeds, and the audit shows `FORCE_UNLOCK`.
4. `terraform_remote_state` data source reading another key.
5. Alternate `/lock` path with `lock_method=POST`/`unlock_method=DELETE`.
6. `snowstate exec -- terraform apply` sidecar mode end-to-end.
7. Migrate from local state: `terraform init -migrate-state`.
8. A large state (≥ 20 MiB generated via many `terraform_data` instances, or by pushing a synthetic state) round-trips with an identical MD5.
9. **Split plan/apply across processes:** `snowstate exec -- terraform plan -out=tfplan` in one process, then a fresh `snowstate exec -- terraform apply tfplan` in another (different port and password, fresh `.terraform/` after `init`). Apply succeeds. A variant changes the state between the two steps and asserts Terraform reports the saved plan as stale.
10. **GitHub Actions workflow:** the repo's own CI runs `examples/github-actions/terraform.yml` logic (as a reusable workflow, or via `act`-free direct jobs) using `action/setup` built from the current commit. For this test, the sidecar uses a test-only `--store=memory` flag, hidden and refused unless `SNOWSTATE_ALLOW_TEST_STORE=1`. It checks the plan artifact handoff between jobs, the `$GITHUB_STEP_SUMMARY` output, and the audit detail containing `run_id`.
11. **Signal handling:** send SIGINT to `snowstate exec` during a slow apply (a `terraform_data` with a `local-exec` sleep). Terraform stops gracefully, state is written and the lock is released. Then repeat with SIGKILL to the child: the lock stays held, and stderr contains the release command.

### 12.3 Integration with real Snowflake (`make integration`, build tag `integration`)

Run only when `SNOWSTATE_IT_*` env vars are set. Each run uses an isolated schema `BACKEND_IT_<random>` and drops it afterwards.

- Migrations apply cleanly twice (idempotent).
- Every Store method, against both INLINE and STAGE storage (force STAGE with `inline_max_bytes=1`).
- **Concurrency test:** 25 goroutines (on separate connections) race to LOCK the same key, 10 iterations. Assert exactly one winner each round. Run it for `hybrid`, and for `standard` when enabled. **Release blocker for hybrid mode.**
- PUT transaction atomicity: inject a failure between steps 3 and 4 (test hook) and assert there's no visible new current version and no audit row.
- `DECOMPRESS_STRING` view returns resources for a known fixture.
- Restore and GC, including orphan stage blob removal.
- The full e2e scenarios 1–3 re-run against Snowflake.
- `install`: the real binary and real Terraform take an Install from nothing to Foundation state held in snowstate, including an interrupted run and a later Foundation change.
- `schema_per_project`: two projects with two roles. Role A can't read, write or lock project B (it gets `ErrForbidden`, mapped to HTTP `403`). Project roles can't `DELETE`/`UPDATE` `STATE_VERSIONS` or `AUDIT_LOG`.

### 12.4 CI

GitHub Actions workflow: lint, unit, e2e on Linux and macOS, build multi-arch binaries (linux/darwin/windows, amd64/arm64) and the container image, and `govulncheck`. Integration runs on manual dispatch, or on main when secrets are present.

Additional CI requirements:
- The release workflow publishes binaries plus `checksums.txt`, and optionally keyless cosign signatures. It moves the `v1` tag for `action/setup`.
- `fmt -check` and `validate` on the Foundation, on Terraform and OpenTofu, with no credentials.
- A `github-wif` integration job (manual dispatch) authenticates to a real Snowflake account via WIF (`token_source=github`) and runs e2e scenarios 1, 2 and 9. This dogfoods the recommended setup.

---

## 13. Documentation deliverables

- `README.md`: what it is, quick start (sidecar in 5 minutes; server), and a backend config example.
- `docs/operator-guide.md`: the Seed, the Foundation, `install` and each of its steps by hand, teardown, the roles each command needs, deployment (Docker, K8s example, systemd), TLS, tokens, scopes, backup and restore, retention, cost, upgrading.
- `docs/github-actions.md`: the §8.4 setup end to end (Snowflake WIF users and `sub` claims, the network policy options, the setup action, the reference workflow, plan/apply split, concurrency, cancellation and recovery, and the key-pair alternative).
- `docs/runbook.md`: stuck locks (including "GitHub job killed mid-apply"), Snowflake outage (clients get 503 and retry), restoring a bad state, rotating keys and tokens, reading `AUDIT_LOG`, sample SQL queries, and recovering an Install whose Foundation state is held in snowstate (lost warehouse, lost runtime grants, broken admin or Seed role).
- `docs/decisions.md`: every VERIFY outcome and every deviation from this spec.
- `examples/terraform/`: server-mode and sidecar-mode examples, and a GitHub Actions/GitLab CI snippet.
- `CHANGELOG.md`.

---

## 14. Milestones and acceptance criteria

Complete them in order, except that a Deferred milestone doesn't block later ones. Each milestone ends with green `make lint test` and a commit.

| # | Milestone | Done when |
|---|---|---|
| M0 | **Protocol verification and scaffold.** Read the Terraform and OpenTofu http client sources and write `docs/decisions.md` §Protocol. Set up the repo layout, Makefile, lint, CI skeleton and config loading. | `decisions.md` answers every §4.0 question with file and line references. `snowstate version` runs. |
| M1 | **Core over the memory store.** `Store` interface, `store/memory`, `service`, `api` with every route in §4.2–4.3, token auth, loopback auth, logging, metrics, health. | All §12.1 tests pass, and the e2e scenarios 1–5 and 7–8 pass against Terraform and OpenTofu. |
| M2 | **Sidecar mode.** `snowstate exec`. | E2E scenario 6 passes, and signals and exit codes are forwarded (tested). |
| M3 | **Snowflake store.** Migrations, `bootstrap.sql`, hybrid locking, PUT transaction, INLINE + STAGE blobs, audit, `doctor`, `migrate`. | §12.3 passes on a real account, including the concurrency test. |
| M4 | **Admin features.** Token CLI, state list, versions, pull and restore, lock list and release, GC, admin API, inventory view. | Each has a unit or integration test, and restore round-trips. |
| M5 | **Standard-mode locking (experimental)** and the hybrid-availability check in `migrate`. | The concurrency test result is recorded in `decisions.md`, and the gating flag works. |
| M6 | **Hardening and packaging.** Rate limiting, graceful shutdown, Dockerfile, K8s example, multi-arch release build, govulncheck, all docs in §13. | A fresh reader can follow the README quick start successfully, the image runs as non-root, and CI is fully green. |
| M6.5 | **GitHub Actions integration (§8.4).** `workload_identity` auth, sidecar saved-plan portability, abort behaviour, CI context and step summary, `action/setup`, reference workflows, `docs/github-actions.md`, and doctor network-policy hints. | E2E scenarios 9–11 pass in CI, and the `github-wif` job passes against a real account. |
| M7 | **DEFERRED: `terraform-provider-snowstate` (Pluggable State Storage).** Not planned. Revisit if `state_store` reaches a stable Terraform release, and re-check the pinned alpha in §14.2 first. | §14.2 acceptance passes, and the CI job is non-blocking. |
| M8 | **One-command install.** The Seed, the Foundation, `snowstate seed`, `snowstate install`, teardown, the recovery runbook, and removing `bootstrap.sql` (§8.5, §10). | The full flow works on a test Install from an empty start, all three recovery drills pass, teardown leaves nothing behind, and CI checks the Foundation on Terraform and OpenTofu. |

The M1–M6 acceptance criteria also include the G7 requirements: `TestImportBoundaries` passes, every frontend goes through `pkg/snowstate/service`, keys follow §4.2, and `LOCKS` follows §6.1.

### 14.2 M7: experimental state store provider

**Context:** read `docs/pluggable-state-storage.md` first. Terraform's `state_store` works only in alpha builds (latest at the time of writing: `v1.17.0-alpha20260827`), enabled with `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` on `init`. Stable releases reject it. Before starting, VERIFY the current alpha, the protocol (`tfplugin6.proto` state store RPCs), and the `terraform-plugin-framework` `statestore` package API (v1.18+), and record them in `decisions.md`.

**Build:**
- Module `/provider` (`terraform-provider-snowstate`), plugin protocol v6, `terraform-plugin-framework` ≥ 1.18. It imports `pkg/snowstate/...` and doesn't duplicate any storage logic.
- **Provider schema:** the Snowflake connection fields from `sfconn` (`account`, `user`, `role`, `warehouse`, `authenticator` = keypair|pat|oauth|externalbrowser, `private_key_path`, `private_key` (sensitive), `token` (sensitive)), all optional with env-var fallbacks.
- **State store `snowstate_snowflake`**, with attributes `database` (default `TF_STATE`), `schema` (default `BACKEND`, ignored in schema_per_project), `tenancy_mode`, `prefix` (required, validated by `pkg/snowstate/keys`), and `chunk_size` (optional, default 8 MiB, max 128 MiB). It must not define any attribute or block named `provider` (core rejects that).

  Method mapping:

  | Method | Implementation |
  |---|---|
  | `Initialize` | `sfconn.Open` plus a schema-version check (fail if migrations are behind, with the message "run snowstate migrate") |
  | `GetStates` | `service.ListWorkspaces(prefix)` |
  | `Read` | `service.Get(prefix/id)`. Empty bytes with no diagnostics if not found. |
  | `Write` | `service.Put(prefix/id, heldLockID[id], body, md5(body))` |
  | `Lock` | `service.Lock({Source:"PSS", Operation, Who: user@host})`. Store the returned ID in an in-process `map[stateID]lockID` and return it. |
  | `Unlock` | `service.Unlock(prefix/id, lockID, force=false)`. A mismatch or no lock is an error diagnostic. `terraform force-unlock <id>` arrives here with the user-supplied ID. Treat a matching ID as a normal unlock. |
  | `DeleteState` | `service.Delete` (soft delete). Refuse `default`. |

  Lock conflicts return the framework's `WorkspaceAlreadyLockedDiagnostic`-style error, including `Who`/`Operation`/`Created`/`Source` from the current lock.
- **Data source `snowstate_outputs`** (the `terraform_remote_state` substitute, since that data source only reads `backend` configs). Inputs are `key`, or `prefix` + `workspace`. It returns `outputs` (dynamic map) and `values_sensitive`, and is read-only, using `service.Get`.
- Release builds go to a filesystem mirror or private registry. Document the `.terraform.lock.hcl` requirement and `-state-provider-lock-file`.

**Acceptance:**
1. `terraform-plugin-testing` state store mode: `StateStore: true` and `VerifyStateStoreLock: true` pass against `store/memory` and against Snowflake (integration tag), skipped unless the Terraform version is ≥ 1.15 and a prerelease.
2. **Interop e2e:** a lock taken via the HTTP backend makes provider `apply` fail with a lock error, and the reverse.
3. **Switch e2e:** start a stack on `backend "http"` at `.../platform/net/default`. Switch the config to `state_store "snowstate_snowflake" { prefix = "platform/net" }`, then run `terraform init` (or `terraform state migrate` with a `.tfmigrate.hcl`). The next `plan` shows no changes, and Snowflake shows **no new rows other than a possible identical-MD5 no-op**.
4. `terraform workspace new/list/select/delete` work and map to keys under the prefix.
5. CI job `pss-experimental` downloads the pinned alpha and runs 1–4. It's marked `continue-on-error: true`.

### 14.3 Definition of done (v1.0)

- Every M0–M6.5 and M8 acceptance criterion above is met (M7 isn't required for v1.0), and no TODOs remain in code paths for §4.3.
- E2E passes for Terraform 1.5.x, the latest Terraform 1.x, and the latest OpenTofu.
- Integration passes on a real Snowflake account in hybrid mode.
- Documentation is complete, and `decisions.md` lists all VERIFY outcomes.

---

## 15. Assumptions and open items (confirm with the owner; proceed with defaults if unanswered)

| # | Item | Default if unanswered |
|---|---|---|
| A1 | Snowflake account is on AWS or Azure (hybrid tables GA) | AWS |
| A2 | Where Terraform runs (CI system, laptops) | Support both: server + sidecar |
| A3 | Snowflake auth for the service | Key-pair service user |
| A4 | Terraform vs OpenTofu | Test both |
| A5 | Expected scale | < 1,000 states, < 50 MiB each, < 10 req/s |
| A6 | Retention | 100 versions per state + Time Travel |
| A7 | Whether Snowflake Postgres + `pg` backend was ruled out (see `docs/initial-review.md`) | postgres backend is rulled out |
| A8 | Whether teams need isolation from each other when clients connect to Snowflake directly | Implement both tenancy modes. Default `shared`. |
| A9 | Whether to build M7 now or wait for `state_store` to reach stable Terraform | Wait for `state_store` to reach stable Terraform. M7 is Deferred. |
| A10 | GitHub-hosted vs self-hosted runners, and whether the Snowflake account enforces network policies | Support both. Document all three network options (§8.4.2). |
| A11 | Snowflake auth from GitHub Actions | Workload identity federation via GitHub OIDC, with key-pair as the documented fallback |
