# Operator guide

## Setting up an Install

An Install is one independent snowstate deployment, named by its database, and an account can hold several. Setting one
up takes two steps:

1. **The Seed, once per account.** An admin runs it as `ACCOUNTADMIN`. It creates the `SNOWSTATE_SEED` role and service
   user, with secondary roles off, and nothing else. Get the SQL from `snowstate seed --public-key-file <file>`, or from
   `deploy/snowflake/seed.sql` with the public key pasted in.
2. **`snowstate install --database <name>`.** It applies the Foundation with the real Terraform or OpenTofu, after you
   approve the plan. It then runs `migrate` as the admin role and `doctor` as the runtime role, and only then moves the
   Foundation's own state into snowstate. Running it again is safe, and is how you make later Foundation changes.

```bash
openssl genrsa 2048 | openssl pkcs8 -topk8 -nocrypt -out ~/.snowflake/keys/snowstate_seed.p8
openssl rsa -in ~/.snowflake/keys/snowstate_seed.p8 -pubout -out snowstate_seed.pub
snowstate seed --public-key-file snowstate_seed.pub > seed.sql   # an admin runs it as ACCOUNTADMIN

export SNOWSTATE_SNOWFLAKE__ACCOUNT=myorg-myaccount
export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH=~/.snowflake/keys/snowstate_seed.p8
snowstate install --database TF_STATE
```

### By hand

Every step `install` runs can also be run by hand, in the directory it writes to. With a `terraform.tfvars` holding
`organization_name`, `account_name`, `private_key_path` and `database_name`:

```bash
terraform init && terraform apply                           # local state
eval "$(terraform output -raw snowstate_env)"               # settings for the runtime role
eval "$(terraform output -raw migrate_command)"             # migrate as <DB>_ADMIN
snowstate doctor                                            # as <DB>_SVC
mv backend.tf.disabled backend.tf
snowstate exec -- terraform init -migrate-state             # move the state into snowstate
snowstate exec -- terraform plan                            # should show no changes
rm terraform.tfstate terraform.tfstate.backup
```

`deploy/terraform/foundation/README.md` explains each step, and `docs/runbook.md` covers recovering and removing an
Install.

The Foundation creates these, with names derived from the database name, which you can override in `terraform.tfvars`:

| Object | Purpose |
|---|---|
| `<DB>` and `<DB>.BACKEND` | The database, and a managed-access schema. Both have `prevent_destroy`. |
| `<DB>_ADMIN` | Owns the objects in the schema and runs `migrate`. Granted to the Seed role, so the Install sits under `SYSADMIN`. |
| `<DB>_SVC` | Runtime role: DML on the tables and access to the stage, no DDL. Granted to `<DB>_ADMIN`. |
| `<DB>_READER` | `SELECT` on views only, so it reaches `STATE_RESOURCES` with no raw state. |
| `<DB>_WH` | X-Small warehouse with `AUTO_SUSPEND = 60`. |
| `<DB>_SERVICE` | Optional service user for `snowstate serve`, created only when you set `service_public_key`. |

Grants to the runtime and reader roles are future grants, made before `migrate` creates anything. Grant `<DB>_SVC` to the
users who run Terraform, and `<DB>_READER` to anyone who only needs the inventory.

Hybrid tables are required for production. `snowstate migrate` probes for them and stops with guidance if they are
unavailable. They are GA on AWS and Azure and in preview on GCP.

### Which role each command needs

| Command | Role |
|---|---|
| `seed` | None. It only prints SQL, which an admin runs as `ACCOUNTADMIN`. |
| `install` | It logs in as `SNOWSTATE_SEED` and sets the role for each step itself. |
| `migrate`, `project create`, `gc`, `token create`, `token revoke` | `<DB>_ADMIN` |
| `doctor` | The role you want to check, usually `<DB>_SVC`, with secondary roles off |
| `serve`, `exec` | `<DB>_SVC`, or a project role in `schema_per_project` |
| `state *`, `lock *`, `token list`, `project list` | Any role that can read or change the tables involved, such as `<DB>_SVC` |
| Inventory queries | `<DB>_READER` |

### Choosing a tenancy mode

| | `shared` (default) | `schema_per_project` |
|---|---|---|
| Layout | One schema | `P_<PROJECT>` per project, plus `CONTROL` |
| Access enforced by | snowstate token scopes | Snowflake grants |
| Use for | A shared server, or a sidecar where everyone may see every state | Sidecars for teams that must be isolated |

`schema_per_project` is the only way to isolate teams that connect with their own credentials, because schema grants
also restrict `INSERT`. Row access policies do not, so they are not used for isolation. It isn't in the Foundation yet,
so it is set up by hand: see below.

Project roles cannot update or delete `STATE_VERSIONS` or `AUDIT_LOG`, so a client with direct credentials cannot erase
history. In this mode, audit rows are written by the client and are advisory. The authoritative trail is
`SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY` and `ACCESS_HISTORY`, filtered on a `QUERY_TAG` starting with `snowstate:`.

## Setup not in the Foundation yet

**These are unverified.** `schema_per_project` has a known login failure and its role isolation has not been tested, and
the GitHub identities have never had a real login. Use them with care, and don't treat them as production-ready.

### `schema_per_project` tenancy

Each team gets a role, and `snowstate project create` makes the project's schema and grants it. As the Seed role:

```sql
CREATE ROLE IF NOT EXISTS <DB>_P_PLATFORM COMMENT = 'read,write,lock on project platform';
CREATE ROLE IF NOT EXISTS <DB>_P_PLATFORM_PLAN COMMENT = 'read,lock on project platform (plan jobs)';
GRANT USAGE ON WAREHOUSE <DB>_WH TO ROLE <DB>_P_PLATFORM;
GRANT USAGE ON WAREHOUSE <DB>_WH TO ROLE <DB>_P_PLATFORM_PLAN;
GRANT ROLE <DB>_P_PLATFORM TO ROLE <DB>_ADMIN;
GRANT ROLE <DB>_P_PLATFORM_PLAN TO ROLE <DB>_ADMIN;
-- project create makes schemas, which the Foundation doesn't let the admin role do.
GRANT CREATE SCHEMA ON DATABASE <DB> TO ROLE <DB>_ADMIN;
```

Then, as `<DB>_ADMIN`, with `tenancy.mode=schema_per_project`:

```bash
snowstate migrate
snowstate project create platform --role <DB>_P_PLATFORM --plan-role <DB>_P_PLATFORM_PLAN
snowstate project list
```

GC, restore-overwrite and token admin stay with `<DB>_ADMIN`, so a client with direct credentials cannot erase history.

### GitHub Actions identities

One service user per repository and environment, with `SUBJECT` equal to the token's `sub` claim. Snowflake provider 2.22
has no workload identity setting for service users, which is one reason this isn't in the Foundation. As the Seed role:

```sql
CREATE USER IF NOT EXISTS GH_PLATFORM_NETWORK_PROD
  TYPE = SERVICE
  DEFAULT_ROLE = <DB>_P_PLATFORM          -- a project role, or <DB>_SVC for shared tenancy
  DEFAULT_WAREHOUSE = <DB>_WH
  DEFAULT_SECONDARY_ROLES = ()
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod'
  );
GRANT ROLE <DB>_P_PLATFORM TO USER GH_PLATFORM_NETWORK_PROD;

-- The plan identity: read,lock only, for plan-on-PR jobs.
CREATE USER IF NOT EXISTS GH_PLATFORM_NETWORK_PLAN
  TYPE = SERVICE
  DEFAULT_ROLE = <DB>_P_PLATFORM_PLAN
  DEFAULT_WAREHOUSE = <DB>_WH
  DEFAULT_SECONDARY_ROLES = ()
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod-plan'
  );
GRANT ROLE <DB>_P_PLATFORM_PLAN TO USER GH_PLATFORM_NETWORK_PLAN;
```

`docs/github-actions.md` covers the rest of the setup.

### Network policy

A sample. Adjust the list, then attach it to the service user:

```sql
CREATE NETWORK POLICY <DB>_NP ALLOWED_IP_LIST = ('203.0.113.0/24');
ALTER USER <DB>_SERVICE SET NETWORK_POLICY = <DB>_NP;
```

## Deployment

### Docker

```bash
make image
docker run --read-only --tmpfs /tmp -p 8443:8443 \
  -v "$PWD/snowstate.yaml:/snowstate.yaml:ro" -v "$PWD/tls:/tls:ro" -v "$PWD/key:/key:ro" \
  -e SNOWSTATE_TOKEN_PEPPER snowstate:dev serve --config /snowstate.yaml
```

The image is distroless, runs as UID 65532, and works on a read-only root filesystem. The Snowflake driver buffers stage
transfers in `/tmp`, so that must be writable.

### Kubernetes

`deploy/k8s/snowstate.yaml` is a working example. Replicas are safe, because all correctness lives in Snowflake.

### systemd

```ini
[Unit]
Description=snowstate
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/snowstate serve --config /etc/snowstate/snowstate.yaml
EnvironmentFile=/etc/snowstate/env
Restart=on-failure
DynamicUser=yes
ProtectSystem=strict
NoNewPrivileges=yes
TimeoutStopSec=40

[Install]
WantedBy=multi-user.target
```

## TLS

TLS is mandatory on non-loopback listeners, with a minimum of TLS 1.2, and the server refuses to start without it. Set
`tls.cert_file` and `tls.key_file`. Set `tls.client_ca_file` to require client certificates. With
`auth.mtls_principal_from_cn: true`, the certificate CN is the principal, Basic auth becomes optional, and its scopes are
the union of that principal's live tokens.

## Tokens and scopes

```bash
snowstate token create --principal ci-platform \
  --scope 'platform/*:read,write,lock' --scope 'shared/*:read' \
  --expires 90d --description 'platform CI'
snowstate token list
snowstate token revoke <token-id>
```

The token prints once. Only an HMAC is stored, keyed with the pepper, so `SNOWSTATE_TOKEN_PEPPER` must stay stable:
changing it invalidates every token. Revocation takes effect within 60 s, the cache TTL.

In patterns, `*` matches any characters including `/`, so `platform/*` covers `platform/network/prod`. `?` matches one
character other than `/`.

| Permission | Allows |
|---|---|
| `read` | `GET` |
| `write` | `POST`, `PUT`, `DELETE` |
| `lock` | `LOCK`, `UNLOCK`, force-unlock |
| `admin` | The admin API |

Failed logins are limited per IP, by default to 10 per minute, then `429`.

## Backup and restore

Every write is an immutable version, and Time Travel covers the tables beyond that. To go back:

```bash
snowstate state versions platform/network/prod --limit 20
snowstate state pull platform/network/prod --version <id> -o old.json
snowstate state restore platform/network/prod --version <id>
```

A restore creates a new version with the old bytes, so history is never rewritten. It refuses while a lock is held
unless you pass `--force`. Restored states carry the old serial, so Terraform rejects any saved plan made in between.

## Retention

`snowstate gc` applies `retention.*`, purges soft-deleted states past `deleted_days`, and sweeps orphan stage blobs. Run
it on a schedule as `<DB>_ADMIN`, and try `--dry-run` first.

A version is removed only if it fails both `keep_versions` and `keep_days`. The current version always stays, and locked
states are never purged. Orphan blobs must be an hour old, since an upload precedes its metadata transaction.

## Inventory

```sql
SELECT RESOURCE_TYPE, COUNT(*) FROM TF_STATE.BACKEND.STATE_RESOURCES GROUP BY 1 ORDER BY 2 DESC;
```

The view excludes instance attributes, because they can contain secrets. It covers states stored inline, which is those
that compress to `storage.inline_max_bytes` or less, and it omits states over 128 MiB uncompressed. OpenTofu's encrypted
states store fine and are simply absent from the view.

## Security

State contains secrets. Restrict `STATE_VERSIONS`, `AUDIT_LOG` and the `STATE_BLOBS` stage to the roles above, and give
everyone else `<DB>_READER`. OpenTofu's client-side state encryption is compatible defence in depth, because
snowstate stores whatever bytes it receives.

## Cost

Every operation needs a running warehouse. A dedicated X-Small with `AUTO_SUSPEND = 60` is enough for the expected
scale. A cold resume bills at least 60 s, about 0.017 credits, and a typical `plan` is a handful of queries.

## Observability

Logs are one JSON line per request with `request_id`, `principal`, `method`, `state_key`, `status`, `duration_ms`,
`sf_query_ids` and `bytes`. State bodies, tokens and `Authorization` headers are never logged.

`/metrics` serves Prometheus metrics, optionally on a separate `metrics.listen`. Useful alerts:
`snowstate_lock_conflicts_total` rising, `snowstate_snowflake_errors_total` by `code`, and
`snowstate_audit_buffer_depth` growing. `/healthz` never touches Snowflake. `/readyz` runs `SELECT 1`, cached for 10 s.

## Upgrading

Stop nothing in advance. Run `snowstate migrate` as `<DB>_ADMIN` with the new binary, then roll the servers. Running
`snowstate install` again does the same, and also applies any change to the Foundation.
Migrations are forward-only and idempotent. `snowstate doctor` confirms the schema is current.
