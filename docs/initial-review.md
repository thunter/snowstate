# Terraform State in Snowflake — Initial Review

| | |
|---|---|
| **Author** | Todd Hunter (prepared with Claude) |
| **Date** | 31 October 2026 |
| **Status** | Draft for decision |
| **Question** | Is there a Terraform backend that stores state in Snowflake? If not, what are the options? |

---

## 1. Summary

**No native Snowflake backend exists for Terraform or OpenTofu, and neither lets you add a backend as a plugin in a stable release.** (Correction, 31 Oct: Terraform has an alpha-only experiment, Pluggable State Storage, that lets providers implement state storage. See `docs/pluggable-state-storage.md`. It can't be used on stable Terraform today, and `SPEC.md` v1.1 is designed so it can be adopted later.) Backends are compiled into the Terraform binary (`local`, `remote`/`cloud`, `s3`, `gcs`, `azurerm`, `pg`, `http`, `consul`, `kubernetes`, `cos`, `oss`). OpenTofu has an open discussion about pluggable backends ([issue #382](https://github.com/opentofu/opentofu/issues/382), [Meta-RFC #2157](https://github.com/opentofu/opentofu/pull/2157)), but its own Meta-RFC says *"This document does not actually propose to write any code yet."* I found no community project that stores Terraform state in Snowflake.

There are two realistic paths that stay inside Snowflake:

1. **Snowflake Postgres + the built-in `pg` backend (no code to write).** Snowflake Postgres became generally available on **24 Feb 2026**. It is real PostgreSQL (versions 16/17/18) running inside your Snowflake account, so Terraform's stock `pg` backend should work against it unchanged, including locking (via Postgres advisory locks). This is the cheapest option to try, **if** your account lets you create a Postgres instance.
2. **Build a small HTTP backend service that stores state in Snowflake tables.** Terraform's built-in `http` backend talks to any REST server that follows its simple protocol (GET/POST/DELETE plus LOCK/UNLOCK). A service that speaks that protocol on one side and Snowflake SQL on the other gives you a Snowflake backend without forking Terraform. `SPEC.md` describes this build.

**Recommendation:** Spend half a day checking Option 1 (can you get `CREATE POSTGRES INSTANCE`, and can your runners reach it through a network policy?). If yes, use it. You're adopting a supported backend instead of maintaining one. If not, build Option 2 from `SPEC.md`. It's a contained build (one Go binary and four tables) and it also gives you a few things Option 1 doesn't: state history, SQL-queryable resource inventory, and Snowflake-native RBAC and auditing.

---

## 2. Background: how Terraform backends work

- A backend stores the state file and, optionally, provides a **lock** so two runs can't change the same state at once.
- Backends are part of the Terraform core binary. In stable releases you **cannot** install a third-party backend the way you install a provider. The experimental `state_store` feature changes this in alpha builds only.
- The **`http` backend** is the official extension point. Terraform sends:
  - `GET <address>` to read state (`200` with the body, or `204`/`404` when there is no state yet)
  - `POST <address>?ID=<lock-id>` to write state (method configurable via `update_method`)
  - `DELETE <address>` to delete state
  - `LOCK <lock_address>` / `UNLOCK <unlock_address>` with a JSON lock-info body (methods configurable). Conflicts return `423 Locked` or `409 Conflict` with the current lock holder's JSON.
  - Auth is **HTTP Basic only** (`username`/`password`, or `TF_HTTP_USERNAME`/`TF_HTTP_PASSWORD`), plus optional mTLS client certs. You can't send custom headers.
  - There is **no workspace support** in the `http` backend, so each workspace or environment gets its own `address`.
- GitLab's managed Terraform state is a well-known production implementation of this same protocol, which shows the approach holds up.

---

## 3. Options considered

### Option 1: Snowflake Postgres + native `pg` backend

Create a Snowflake Postgres instance (`CREATE POSTGRES INSTANCE ...`), open it to your CI runners with a network policy, and configure:

```hcl
terraform {
  backend "pg" {
    conn_str    = "postgres://application:<pw>@<host>/postgres?sslmode=require"
    schema_name = "terraform_remote_state"
  }
}
```

| Pros | Cons |
|---|---|
| Zero code. Uses a backend HashiCorp maintains and tests. | Needs the `CREATE POSTGRES INSTANCE` privilege, which **only ACCOUNTADMIN has by default**, so it may be a political or approval hurdle. |
| Locking via Postgres advisory locks, a mature mechanism. | The instance is always on and storage is provisioned (min 10 GB), so it's a standing cost even when nobody is running Terraform. |
| Supports Terraform workspaces natively. | An instance is unreachable until you attach a **network policy**. CI runners need allow-listed egress IPs. |
| Works with Terraform and OpenTofu. | Credentials are Postgres users/passwords, not Snowflake RBAC. Connection-pooler gotcha: session advisory locks break behind a transaction-mode pooler. |
| GA product with HA option. | No state version history (the `pg` backend overwrites in place). You'd rely on backups. |
| | Availability may vary by cloud or region. **Verify for your account.** |

**Verdict:** The best option if the privilege and networking are approved. Validate with a spike before anything else.

### Option 2: Custom HTTP backend service backed by Snowflake tables (the build)

A small service (`snowstate`) implements the Terraform `http` protocol and persists state, versions, locks and an audit log in Snowflake.

| Pros | Cons |
|---|---|
| Uses only standard Snowflake (database, schema, tables, warehouse) that you already have access to. | **You own it.** Code, upgrades, security patches and on-call for something on Terraform's critical path. |
| Full **version history** of every state write, restore to any version, plus Snowflake Time Travel. | Needs somewhere to run (a container, VM, or a **local sidecar per run**, see below). |
| **State is queryable with SQL.** A view can flatten every managed resource across all states (inventory, drift reporting, cost tagging). | Every Terraform operation runs Snowflake queries, so a **warehouse must resume**. Expect ~0.2–1 s latency per call and a 60-second minimum billing on each resume (≈0.017 credits on an X-Small). Cheap, but not free, and slower than S3/pg. |
| Snowflake RBAC, network policies, encryption at rest, and an audit table you control. | Safe locking depends on **hybrid tables** (enforced primary key, row-level locks). They're GA on AWS/Azure but in **preview on GCP**. Standard tables only offer READ COMMITTED isolation and table-level DML locks, so a lock built on them needs careful testing. |
| Works with Terraform and OpenTofu, and supports `terraform_remote_state`. | `http` backend limits: Basic auth only, no native workspaces (one URL per workspace). |
| Can run as a **local sidecar** (`snowstate exec -- terraform apply`) using the caller's own Snowflake credentials, so no shared server is needed. | State contains secrets. You must lock down the tables and the inventory view carefully. |

**Verdict:** Viable and fairly low-risk to build if Option 1 is blocked. Full specification in `SPEC.md`.

### Option 3: Host the HTTP backend inside Snowflake (Snowpark Container Services)

Running Option 2's service in SPCS with a public endpoint sounds attractive (everything stays in Snowflake), but SPCS public endpoints require an `Authorization: Snowflake Token="<PAT or OAuth token>"` header. Terraform's `http` backend can only send HTTP Basic auth, so Terraform **can't call an SPCS endpoint directly**. You'd still need a local proxy to add the header, and at that point the sidecar mode from Option 2 is simpler.

**Verdict:** Not viable for v1. Revisit if Terraform/OpenTofu add custom header support to the `http` backend.

### Option 4: Local backend + wrapper scripts that sync state to a Snowflake stage

Run with `backend "local"` and have a script `GET`/`PUT` the file to an internal stage before and after each run.

| Pros | Cons |
|---|---|
| A trivial script. | **No locking.** Concurrent runs silently overwrite each other, which is the main thing a backend exists to prevent. |
| | Fragile: a crash mid-run leaves local and remote out of sync. `terraform_remote_state` doesn't work. |

**Verdict:** Rejected.

### Option 5: Reuse an existing open-source HTTP backend

Projects such as [terraform-generic-http-backend](https://github.com/Sedona-Solutions/terraform-generic-http-backend) and [orange-cloudfoundry/terraform-secure-backend](https://github.com/orange-cloudfoundry/terraform-secure-backend) implement the protocol but store state elsewhere (files, CredHub). They're useful references for protocol edge cases, but none store state in Snowflake. Swapping their storage layer is about as much work as Option 2, and you'd inherit a codebase you didn't design.

**Verdict:** Use as reference material only.

### Excluded by constraint

HCP Terraform/Terraform Cloud, S3+DynamoDB, GCS, Azure Blob and self-hosted Postgres are all standard choices, but they fall outside the "Snowflake only" constraint.

---

## 4. Comparison

| Criterion | 1. Snowflake Postgres + `pg` | 2. `snowstate` HTTP service | 3. SPCS-hosted | 4. Stage sync script |
|---|---|---|---|---|
| Code to maintain | None | ~3–5k lines of Go | Same as 2, plus a proxy | Small script |
| Safe locking | Yes | Yes (hybrid tables) | Yes | **No** |
| State history | No | Yes | Yes | Partial |
| Query state with SQL | Via Postgres | Yes, native view | Yes | Awkward |
| Workspaces | Native | One URL per workspace | One URL per workspace | N/A |
| Needs ACCOUNTADMIN help | Yes (grant) | Only for initial DB/role setup | Yes (compute pool) | No |
| Standing cost | Always-on instance | Warehouse per call (auto-suspend) | Compute pool | Negligible |
| Time to first use | ~Half a day | ~1–2 weeks with an agent | Longer | Hours |

---

## 5. Snowflake-specific design considerations (for Option 2)

- **Size limits:** VARCHAR and VARIANT max 128 MB, BINARY max 64 MB (2025_03 behaviour bundle). State is stored gzip-compressed in BINARY, which comfortably covers even very large states, with an internal-stage fallback for anything bigger.
- **Byte fidelity:** Don't store state only as VARIANT. Snowflake normalises JSON (key order, number formatting), which would break Terraform's MD5 checks. Store the exact bytes, and expose a parsed view on top.
- **Locking:** Use a hybrid table with `STATE_KEY` as an enforced primary key. Lock acquisition is then a single `INSERT` that fails atomically if a lock exists. `SELECT ... FOR UPDATE` is hybrid-table-only.
- **Transactions:** A single transaction can span hybrid and standard tables, so "check lock + write version + move pointer" is atomic.
- **Auth to Snowflake:** Key-pair (JWT) for services, programmatic access tokens (PATs) as an alternative, external browser SSO for developers in sidecar mode.
- **Cost:** Use a dedicated X-Small warehouse with `AUTO_SUSPEND = 60`. A typical `plan` is about 3–4 queries.

---

## 6. Open questions to resolve before building

1. Can we get `CREATE POSTGRES INSTANCE` (Option 1)? If yes, does that settle it?
2. Which cloud and region is the Snowflake account on? (This decides hybrid table availability: GA on AWS/Azure, preview on GCP.)
3. Where will Terraform run? CI (which system?), laptops, or both? This decides between a shared server and sidecar mode.
4. Terraform or OpenTofu, and which versions?
5. Who administers Snowflake roles and grants for the new database?
6. How many teams or states? Is per-team isolation needed (token scopes)?

---

## Sources

- [Terraform `http` backend documentation](https://developer.hashicorp.com/terraform/language/backend/http)
- [OpenTofu HTTP backend guide (OneUptime, Mar 2026)](https://oneuptime.com/blog/post/2026-03-20-opentofu-http-backend/view)
- [OpenTofu: Backends as plugins, issue #382](https://github.com/opentofu/opentofu/issues/382)
- [OpenTofu: Meta-RFC "The future of Backends", PR #2157](https://github.com/opentofu/opentofu/pull/2157)
- [Snowflake Postgres GA release note (24 Feb 2026)](https://docs.snowflake.com/en/release-notes/2026/other/2026-02-24-snowflake-postgres-ga)
- [Creating a Snowflake Postgres instance](https://docs.snowflake.com/en/user-guide/snowflake-postgres/postgres-create-instance)
- [Snowflake hybrid tables](https://docs.snowflake.com/en/user-guide/tables-hybrid)
- [Snowflake `FOR UPDATE` (hybrid tables only)](https://docs.snowflake.com/sql-reference/constructs/for-update)
- [Snowflake new maximum object sizes (BCR 2025_03)](https://docs.snowflake.com/release-notes/bcr-bundles/2025_03/bcr-1942)
- [Snowflake DML locking and transactions overview](https://www.certsafari.com/snowflake/snowpro-advanced-administrator-c02/learn/snowflake-dml-locking-transactions-and-concurrency)
- [SPCS: accessing a public endpoint programmatically](https://docs.snowflake.com/en/developer-guide/snowpark-container-services/tutorials/advanced/tutorial-8-access-public-endpoint-programmatically)
- [Sedona-Solutions/terraform-generic-http-backend](https://github.com/Sedona-Solutions/terraform-generic-http-backend)
- [orange-cloudfoundry/terraform-secure-backend](https://github.com/orange-cloudfoundry/terraform-secure-backend)
