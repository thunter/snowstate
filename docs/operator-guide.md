# Operator guide

## Snowflake bootstrap

Run `deploy/snowflake/bootstrap.sql` as an admin. It creates:

| Object | Purpose |
|---|---|
| `SNOWSTATE_ADMIN` | Owns objects. Runs `migrate`, `gc`, `project create` and token admin. |
| `SNOWSTATE_SVC` | Runtime role for the server: DML and stage access, no DDL. |
| `SNOWSTATE_READER` | `SELECT` on `STATE_RESOURCES` only, with no access to raw state. |
| `SNOWSTATE_WH` | X-Small warehouse with `AUTO_SUSPEND = 60`. |
| `TF_STATE.BACKEND` | Managed access schema. |

The script has three parts. Part A is shared tenancy, part B is `schema_per_project`, and part C is GitHub Actions
identities. In part A, run `snowstate migrate` as `SNOWSTATE_ADMIN` before the grants that name the migrated tables.

Hybrid tables are required for production. `snowstate migrate` probes for them and stops with guidance if they are
unavailable. They are GA on AWS and Azure and in preview on GCP.

### Choosing a tenancy mode

| | `shared` (default) | `schema_per_project` |
|---|---|---|
| Layout | One schema | `P_<PROJECT>` per project, plus `CONTROL` |
| Access enforced by | snowstate token scopes | Snowflake grants |
| Use for | A shared server, or a sidecar where everyone may see every state | Sidecars for teams that must be isolated |

`schema_per_project` is the only way to isolate teams that connect with their own credentials, because schema grants
also restrict `INSERT`. Row access policies do not, so they are not used for isolation.

```bash
snowstate project create platform --role SNOWSTATE_P_PLATFORM --plan-role SNOWSTATE_P_PLATFORM_PLAN
snowstate project list
```

Project roles cannot update or delete `STATE_VERSIONS` or `AUDIT_LOG`, so a client with direct credentials cannot erase
history. In this mode, audit rows are written by the client and are advisory. The authoritative trail is
`SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY` and `ACCESS_HISTORY`, filtered on a `QUERY_TAG` starting with `snowstate:`.

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
it on a schedule as `SNOWSTATE_ADMIN`, and try `--dry-run` first.

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
everyone else `SNOWSTATE_READER`. OpenTofu's client-side state encryption is compatible defence in depth, because
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

Stop nothing in advance. Run `snowstate migrate` as `SNOWSTATE_ADMIN` with the new binary, then roll the servers.
Migrations are forward-only and idempotent. `snowstate doctor` confirms the schema is current.
