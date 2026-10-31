# snowstate

A Terraform and OpenTofu state backend that stores state in Snowflake.

Neither tool supports third-party backends in a stable release, but both ship a generic `http` backend. snowstate is
that server: it keeps state, full version history, locks and an audit trail in Snowflake tables, and works with stock
Terraform 1.5+ and OpenTofu 1.6+.

- **Correct locking.** A hybrid table's enforced primary key decides every race.
- **Every write is an immutable version.** List, download and restore any of them.
- **Byte-exact storage**, checked against MD5 on every read.
- **Two deployment modes** from one binary: a shared server, or `snowstate exec`, a sidecar with no shared
  infrastructure.
- **Query your infrastructure with SQL** through the `STATE_RESOURCES` view, which omits attributes because they can
  hold secrets.

## Quick start: sidecar

The sidecar runs a loopback server for the length of one command, using your own Snowflake credentials.

**1. Build.** This needs Go 1.27.

```bash
make build     # writes bin/snowstate
export PATH="$PWD/bin:$PATH"
```

**2. Configure.** You need a role that can create objects in a schema. `deploy/snowflake/bootstrap.sql` sets up a
production footprint.

```bash
export SNOWSTATE_SNOWFLAKE__ACCOUNT=myorg-myaccount
export SNOWSTATE_SNOWFLAKE__USER=ME
export SNOWSTATE_SNOWFLAKE__ROLE=SNOWSTATE_ADMIN
export SNOWSTATE_SNOWFLAKE__WAREHOUSE=SNOWSTATE_WH
export SNOWSTATE_SNOWFLAKE__DATABASE=TF_STATE
export SNOWSTATE_SNOWFLAKE__SCHEMA=BACKEND
export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH=~/.snowflake/keys/me.p8
```

Laptop users can set `SNOWSTATE_SNOWFLAKE__AUTH=externalbrowser` instead of a key.

**3. Create the schema and check it.**

```bash
snowstate migrate
snowstate doctor
```

**4. Point a module at it.** The backend block stays empty:

```hcl
terraform {
  backend "http" {}
}
```

**5. Run Terraform through the sidecar.**

```bash
export SNOWSTATE_KEY=platform/network/prod
snowstate exec -- terraform init
snowstate exec -- terraform apply
```

## Quick start: server

```bash
export SNOWSTATE_TOKEN_PEPPER="$(openssl rand -base64 48)"   # keep this stable and secret
snowstate token create --principal ci-platform --scope 'platform/*:read,write,lock' --expires 90d
snowstate serve --config snowstate.yaml
```

```hcl
terraform {
  backend "http" {
    address        = "https://snowstate.example.com/v1/states/platform/network/prod"
    lock_address   = "https://snowstate.example.com/v1/states/platform/network/prod"
    unlock_address = "https://snowstate.example.com/v1/states/platform/network/prod"
    username       = "ci-platform"   # the token goes in TF_HTTP_PASSWORD
  }
}
```

Keys are `<prefix>/<workspace>`. The http backend has no workspace support, so each workspace gets its own address, and
the default workspace is the literal segment `default`: `platform/network/default`.

## Documentation

| | |
|---|---|
| [`docs/tutorial-snowflake-database.md`](docs/tutorial-snowflake-database.md) | Tutorial: create a database with the Snowflake provider |
| [`docs/operator-guide.md`](docs/operator-guide.md) | Bootstrap, deployment, TLS, tokens, retention, cost, upgrades |
| [`docs/github-actions.md`](docs/github-actions.md) | Workload identity, the setup action, plan and apply across jobs |
| [`docs/runbook.md`](docs/runbook.md) | Stuck locks, outages, bad states, key rotation |
| [`docs/decisions.md`](docs/decisions.md) | Every verified behaviour and every deviation from the spec |
| [`snowstate.example.yaml`](snowstate.example.yaml) | Every configuration key |
| [`examples/`](examples) | Module and CI examples |

## Development

```bash
make lint test     # unit tests, no network
make e2e           # real Terraform and OpenTofu against a memory store
make integration   # real Snowflake; needs SNOWSTATE_IT_*, skipped otherwise
```

Apache-2.0.
