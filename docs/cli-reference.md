# CLI reference

Every `snowstate` command and flag. Configuration keys are covered in full in `snowstate.example.yaml`.

| Command | Purpose |
|---|---|
| [`serve`](#snowstate-serve) | Run the shared HTTP state backend |
| [`exec`](#snowstate-exec) | Run a command with a temporary local backend (sidecar mode) |
| [`seed`](#snowstate-seed) | Print the Seed SQL for an admin to run |
| [`install`](#snowstate-install) | Create an Install, from Terraform to its state in snowstate |
| [`migrate`](#snowstate-migrate) | Create or upgrade the Snowflake schema |
| [`doctor`](#snowstate-doctor) | Check connectivity, grants and setup |
| [`token create`](#snowstate-token-create), [`list`](#snowstate-token-list), [`revoke`](#snowstate-token-revoke) | Manage API tokens |
| [`state list`](#snowstate-state-list), [`versions`](#snowstate-state-versions), [`pull`](#snowstate-state-pull), [`restore`](#snowstate-state-restore) | Inspect and restore states |
| [`lock list`](#snowstate-lock-list), [`release`](#snowstate-lock-release) | Inspect and release locks |
| [`project create`](#snowstate-project-create), [`list`](#snowstate-project-list) | Manage `schema_per_project` tenancy |
| [`gc`](#snowstate-gc) | Apply retention and sweep orphan blobs |
| [`version`](#snowstate-version) | Print version information |
| [`completion`](#snowstate-completion) | Generate shell completion scripts |
| [`help`](#snowstate-help) | Show help for any command |

## Common behaviour

### Global flags

These work on every command.

| Flag | Default | Description |
|---|---|---|
| `--config <file>` | `./snowstate.yaml`, if it exists | Config file to load. A file named here must exist, or the command fails. |
| `-h`, `--help` | | Show help for the command. |

### Configuration

Settings are applied in this order, with later ones winning:

1. Built-in defaults
2. The YAML config file
3. `SNOWSTATE_*` environment variables, with `__` for nesting
4. Command-line flags

For example, `SNOWSTATE_SNOWFLAKE__ROLE=TF_STATE_ADMIN` sets `snowflake.role`. You can set a value for one command:

```bash
SNOWSTATE_SNOWFLAKE__ROLE=TF_STATE_ADMIN snowstate migrate
```

Only `serve` and `exec` have flags that change configuration. Everything else is set through the file or the environment.

These variables hold values directly rather than configuring a key:

| Variable | Used for |
|---|---|
| `SNOWSTATE_KEY` | The state key for `exec`, if `--key` is not given |
| `SNOWSTATE_TOKEN_PEPPER` | The 32+ byte secret for token hashing. Needed by `serve` in token mode and by `token create`. |
| `SNOWSTATE_SNOWFLAKE_TOKEN` | Token for `auth: pat` or `auth: oauth` |
| `SNOWSTATE_SNOWFLAKE_PASSWORD` | Password for `auth: password` |
| `SNOWSTATE_SNOWFLAKE_PRIVATE_KEY_PEM` | PEM private key, as an alternative to `private_key_path` |

### Which role to use

Roles are named after the Install's database, `<DB>`. `snowflake.role` defaults to `TF_STATE_SVC`, the runtime role of a
`TF_STATE` Install, which has no DDL rights, so set the role when a command needs more.

The operator guide lists the role each command needs, under "Which role each command needs".

Admin commands talk to Snowflake directly, not through a running server.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | The command failed. The reason is printed to stderr as `error: ...`. |
| Child's code | `exec` exits with the code of the command it ran. |
| `128 + n` | `exec` only: the command was killed by signal `n`, so SIGKILL gives `137`. |

### State keys

A key is `<prefix>/<workspace>`, such as `platform/network/prod`. It needs at least two segments and at most 16, and 512
characters at most. Each segment starts with a letter or digit, then allows letters, digits, `.`, `_` and `-`. The final
segment cannot be `lock`. The first segment is the project, and keys are case-sensitive.

### Test store

`serve`, `exec`, `token`, `state`, `lock` and `gc` have a hidden `--store=memory` flag, for tests only. It is refused
unless `SNOWSTATE_ALLOW_TEST_STORE=1` is set, and its data is not durable. Never use it for real state.

---

## snowstate serve

Runs the long-lived HTTP backend for Terraform and OpenTofu.

```text
snowstate serve [flags]
```

| Flag | Overrides | Description |
|---|---|---|
| `--listen <addr>` | `server.listen` (default `:8443`) | Address to listen on. |
| `--auth-mode <mode>` | `auth.mode` (default `token`) | `token`, `loopback-secret` or `none`. |
| `--log-level <level>` | `log.level` (default `info`) | `debug`, `info`, `warn` or `error`. |

The auth modes:

| Mode | Meaning | Where it is allowed |
|---|---|---|
| `token` | Basic auth with tokens from `token create`, limited by their scopes. Needs `SNOWSTATE_TOKEN_PEPPER`. | Anywhere |
| `loopback-secret` | A random password made at startup. `exec` uses this mode internally. `serve` does not print the password, so no client can log in: **do not use this mode with `serve`**. | Loopback only |
| `none` | No authentication, for tests. It logs a warning. | Loopback only |

Startup checks all settings and reports every problem at once. It refuses to start if:

- a non-loopback address has no TLS (`tls.cert_file` and `tls.key_file`);
- `loopback-secret` or `none` is used on a non-loopback address;
- token mode has no pepper, or one shorter than 32 bytes.

`:8443` listens on all interfaces, so it counts as non-loopback.

On SIGINT or SIGTERM the server stops accepting connections, drains requests for up to `server.shutdown_timeout`
(default 30 s), writes buffered audit rows, and closes its Snowflake connections.

```bash
# Production, with TLS and tokens from the config file
export SNOWSTATE_TOKEN_PEPPER="$(openssl rand -base64 48)"
snowstate serve --config /etc/snowstate/snowstate.yaml

# Local, with tokens and no TLS
snowstate serve --listen 127.0.0.1:8080

# Debug logging for one run
snowstate serve --config snowstate.yaml --log-level debug
```

```text
$ snowstate serve --auth-mode=none --listen=0.0.0.0:8080
error: invalid configuration:
auth.mode=none is only allowed on a loopback listener, not "0.0.0.0:8080"
tls.cert_file: TLS is required on non-loopback listener "0.0.0.0:8080"
```

## snowstate exec

Runs one command with a temporary backend on `127.0.0.1`, then shuts it down. It needs no server, and it uses your own
Snowflake credentials, so Snowflake grants decide what you can reach. Token scopes do not apply.

```text
snowstate exec [flags] -- <command> [args]
```

| Flag | Default | Description |
|---|---|---|
| `--key <key>` | `SNOWSTATE_KEY` | State key. One of the two must be set. |
| `--release-stale-lock` | off | If the command exits while still holding its lock, release it. Audited as `FORCE_UNLOCK`. |
| `--shutdown-grace <duration>` | `20s` | How long after a **second** signal to wait for the command before killing it. |

The module must declare an empty backend, because `exec` supplies the connection:

```hcl
terraform {
  backend "http" {}
}
```

The command gets your environment plus `TF_HTTP_ADDRESS`, `TF_HTTP_LOCK_ADDRESS`, `TF_HTTP_UNLOCK_ADDRESS`,
`TF_HTTP_USERNAME` and `TF_HTTP_PASSWORD`. These go only into the environment, so saved plans work in a later `exec` with a
different port and password.

- **Flags:** anything after the first non-flag word goes to the command, so `--` is optional but clearer.
- **Exit code:** `exec` exits with the command's code.
- **Signals:** SIGINT and SIGTERM are forwarded once, and the server stays up so Terraform can write state and unlock.
  The grace timer starts at the second signal.
- **Stale locks:** if the command dies holding its lock, the lock stays held and `exec` prints the release command. Add
  `--release-stale-lock` only if you accept that a partial apply may go unnoticed.
- **Logging:** only warnings are shown, on stderr, unless you set `log.level` to something other than `info`.
- **GitHub Actions:** with `GITHUB_ACTIONS=true`, run details go in audit rows, locks are marked
  `github:<repository>#<run_id>/<run_attempt>`, and a summary goes to `$GITHUB_STEP_SUMMARY`.

```bash
# With the key in the environment
export SNOWSTATE_KEY=platform/network/prod
snowstate exec -- terraform init
snowstate exec -- terraform apply

# With the key as a flag
snowstate exec --key platform/network/dev -- terraform plan -out=tfplan

# In unattended jobs, clear the lock if the command dies
snowstate exec --release-stale-lock -- terraform apply -input=false tfplan

# OpenTofu
snowstate exec -- tofu plan

# Wait longer for Terraform after a second signal
snowstate exec --shutdown-grace 60s -- terraform apply
```

If the command dies holding its lock:

```text
snowstate: the command exited while still holding lock 6f1c... on platform/network/prod.
Check for a partial apply, then release it with:
  snowstate lock release platform/network/prod
```

## snowstate seed

Prints the SQL for the Seed: the `SNOWSTATE_SEED` role and user that Terraform starts from, with your public key filled in.
An admin runs it once per account, as `ACCOUNTADMIN`. New Installs don't need it again.

It never connects to Snowflake and needs no configuration, so the person running it doesn't need the admin's credentials.
It refuses a private key without printing any of it, and only accepts an RSA public key, which Snowflake key-pair auth needs.

```text
snowstate seed --public-key-file <file>
```

| Flag | Required | Description |
|---|---|---|
| `--public-key-file <file>` | Yes | PEM public key for the `SNOWSTATE_SEED` user, as written by `openssl rsa -pubout`. |

```bash
openssl genrsa 2048 | openssl pkcs8 -topk8 -nocrypt -out ~/.snowflake/keys/snowstate_seed.p8
openssl rsa -in ~/.snowflake/keys/snowstate_seed.p8 -pubout -out snowstate_seed.pub
snowstate seed --public-key-file snowstate_seed.pub > seed.sql
```

```text
$ snowstate seed --public-key-file ~/.snowflake/keys/snowstate_seed.p8
error: /Users/me/.snowflake/keys/snowstate_seed.p8: this is a private key: pass the public key, from openssl rsa -in <key> -pubout
```

## snowstate install

Creates an Install. It runs the real Terraform or OpenTofu, never a reimplementation, and each step below can also be run by
hand, as `deploy/terraform/foundation/README.md` shows.

1. Writes the Foundation's Terraform code, embedded in the binary, to `--dir`, plus a `terraform.tfvars`. A file already
   there must match, so your code is never overwritten: if one differs, `install` stops and shows the difference.
2. Runs `init` and `plan`, shows the plan, and applies it only once you type `yes`, unless you pass `--auto-approve`. A
   plan with no changes skips the apply.
3. Runs `migrate` as `<DB>_ADMIN`, then `doctor` as `<DB>_SVC`, which proves the runtime role's grants.
4. Only once `doctor` passes, moves the Foundation's own state into snowstate under `snowstate/foundation/default`, checks
   that a plan through snowstate shows no changes, and removes the local state files.

**Running it again is safe at any point.** Before the move, it carries on from where it stopped, and a plan with no changes
means an earlier run already applied. After the move, it plans and applies through snowstate, so it is also how you make a
later Foundation change: edit `terraform.tfvars` and run `install` again. It saves the settings for snowstate commands in
`snowstate.env` in the directory, so you can `source` it, and you don't need `terraform output` during an outage.

Only the files `install` ships are compared. `backend.tf` counts as its disabled form, and files such as `terraform.tfvars`
and `.terraform.lock.hcl` are yours. `install` rewrites `snowstate.env` on every run, so don't edit it. If a change renames
the runtime role or warehouse, run that change by hand, as the names in `snowstate.env` are the old ones until it finishes.

It logs in as `SNOWSTATE_SEED`, which [`seed`](#snowstate-seed) creates, so configure the account and the Seed key. It sets
the role, warehouse, database and schema itself for each step, from the Foundation's outputs. A failed step says which step
failed and what to do next, and a missing binary fails before anything changes.

```text
snowstate install --database <name> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--database <name>` | required | Database for the Install. Account-level names derive from it, such as `<name>_ADMIN`. |
| `--dir <path>` | `snowstate-foundation` | Directory for the Terraform code. Keep it, and commit it if you like. |
| `--binary <name>` | `terraform` | Terraform or OpenTofu binary, such as `tofu`. |
| `--auto-approve` | off | Apply the plan without asking. |
| `--write-only` | off | Write the Terraform code and `terraform.tfvars`, then stop. Nothing runs, and Snowflake isn't contacted. |

```bash
export SNOWSTATE_SNOWFLAKE__ACCOUNT=myorg-myaccount
export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH=~/.snowflake/keys/snowstate_seed.p8
snowstate install --database TF_STATE
```

```text
==> write the Foundation to snowstate-foundation
==> terraform init
==> terraform plan
...
Apply this plan? Only 'yes' will be accepted: yes
==> terraform apply
==> migrate as TF_STATE_ADMIN
==> doctor as TF_STATE_SVC
==> move the state into snowstate
==> done: the Foundation's state is in snowstate under snowstate/foundation/default
```

## snowstate migrate

Creates or upgrades the Snowflake objects: the schema if it is missing, then tables, stage and views. It is safe to run
repeatedly, and it only moves forward. Run it as `<DB>_ADMIN`. If the role can't create objects, the error says so.

It also tests for hybrid tables. In `hybrid` locking mode it stops with guidance if they are unavailable. With
`tenancy.mode=schema_per_project`, it upgrades `CONTROL` and every project schema.

```text
snowstate migrate [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | off | List migrations without running them. This skips the hybrid table test. |

```bash
snowstate migrate --dry-run
SNOWSTATE_SNOWFLAKE__ROLE=TF_STATE_ADMIN snowstate migrate
```

```text
$ snowstate migrate
hybrid tables: available = true
applied control/0001_control.sql
applied BACKEND/0001_init.sql

$ snowstate migrate
hybrid tables: available = true
schema is up to date
```

## snowstate doctor

Checks the setup and prints `PASS` or `FAIL` for each item:

- configuration and Snowflake login;
- the session's role, warehouse, database and schema;
- whether migrations are current;
- whether `LOCKS` is a hybrid table, in `hybrid` locking mode;
- a real write and delete of a lock row, under a scratch key.

If a network policy blocked the login, it lists the options for fixing that. It exits `1` if anything failed.

```text
snowstate doctor
```

It has no flags of its own.

```text
$ snowstate doctor
PASS  config                 parsed
PASS  connection config
PASS  snowflake auth         connected
PASS  session                user=TODD role=SNOWSTATE_DEV
PASS  role                   SNOWSTATE_DEV
PASS  warehouse              SNOWSTATE_DEV_WH
PASS  database               TF_STATE_DEV
PASS  schema                 BACKEND
PASS  migrations             version 1
PASS  hybrid tables          LOCKS is hybrid
PASS  privileges             lock row written and deleted
```

## snowstate token create

Creates an API token for `serve` in token mode. **The token is printed once**, and only its HMAC is stored.

`SNOWSTATE_TOKEN_PEPPER` must hold the same value the server uses, or the token will not work.

```text
snowstate token create --principal <name> --scope <scope> [flags]
```

| Flag | Required | Description |
|---|---|---|
| `--principal <name>` | Yes | Principal name, which is also the HTTP username. |
| `--scope <scope>` | Yes | `<key pattern>:<perm>[,<perm>]`. Repeat it for more scopes. |
| `--expires <lifetime>` | No | Lifetime, such as `90d`, `12h` or `30m`. With no value, the token does not expire. |
| `--description <text>` | No | Free text, shown by `token list`. |

| Permission | Allows |
|---|---|
| `read` | `GET` |
| `write` | `POST`, `PUT`, `DELETE` |
| `lock` | `LOCK`, `UNLOCK` and force-unlock |
| `admin` | The admin HTTP API |

In key patterns, `*` matches any characters, including `/`, so `platform/*` covers `platform/network/prod`. `?` matches one
character other than `/`. Access is denied unless a scope allows it. Quote scopes so the shell does not expand `*`.

```bash
snowstate token create --principal ci-platform \
  --scope 'platform/*:read,write,lock' --scope 'shared/*:read' \
  --expires 90d --description 'platform CI'
```

```text
id:    fd2c1c72-94dc-402f-8477-93ebe1769ff7
token: sst_...
Store it now: it will not be shown again.
```

Terraform then uses `username = "ci-platform"` and the token in `TF_HTTP_PASSWORD`.

## snowstate token list

Lists tokens with their principal, scopes, expiry, revocation, last use and description. It never shows token values.
`LAST_USED` updates at most every 5 minutes.

```text
snowstate token list
```

It has no flags of its own.

```text
$ snowstate token list
ID                                    PRINCIPAL    SCOPES                                    EXPIRES               REVOKED  LAST_USED  DESCRIPTION
fd2c1c72-94dc-402f-8477-93ebe1769ff7  ci-platform  platform/*:read,write,lock shared/*:read  2027-01-29T23:44:23Z  -        -          platform CI
```

## snowstate token revoke

Revokes a token by the ID from `token list`. Servers cache verified tokens, so it takes effect within 60 s.

```text
snowstate token revoke <token-id>
```

It has no flags of its own.

```bash
snowstate token revoke fd2c1c72-94dc-402f-8477-93ebe1769ff7
```

## snowstate state list

Lists current states, showing how each key splits into prefix and workspace. Deleted states are left out.

```text
snowstate state list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--prefix <prefix>` | All states | Only keys equal to this prefix, or under it. |

```bash
snowstate state list
snowstate state list --prefix platform/network
```

```text
KEY                    PREFIX            WORKSPACE  SERIAL  SIZE  UPDATED                    BY
platform/network/prod  platform/network  prod       1       863   2026-10-30T23:15:22-07:00  TODD
```

## snowstate state versions

Lists a state's versions, newest first. `RESTORED_FROM` shows which version a restore copied.

```text
snowstate state versions <key> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--limit <n>` | `0` | Maximum versions to show. `0` shows all of them. |

```bash
snowstate state versions platform/network/prod --limit 20
```

```text
VERSION                               SERIAL  SIZE  STORAGE  CREATED                    BY    RESTORED_FROM
01a19099-35e9-710b-b7b2-8a6e547e245a  1       863   INLINE   2026-10-30T23:15:22-07:00  TODD
```

## snowstate state pull

Downloads a state's exact stored bytes. It checks them against the stored MD5 first. It fails if the state has no current
content, which includes soft-deleted states, but `--version` can still read any kept version of those.

**State contains secrets.** Files written with `-o` are readable only by you (mode `0600`). Output to stdout has no such
protection.

```text
snowstate state pull <key> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--version <id>` | The current version | Version to download. |
| `-o`, `--output <file>` | stdout | Write to this file. |

```bash
snowstate state pull platform/network/prod -o state.json
snowstate state pull platform/network/prod --version 01a19099-35e9-710b-b7b2-8a6e547e245a -o old.json
snowstate state pull platform/network/prod | jq '.resources | length'
```

## snowstate state restore

Makes a **new** version with the bytes of an old one, so history is never rewritten. If those bytes are already current,
nothing changes and the current version is reported. It also restores a soft-deleted state.

It refuses while a lock is held, unless you pass `--force`. The restored state keeps the old serial, so Terraform rejects
any saved plan made in between.

```text
snowstate state restore <key> --version <id> [flags]
```

| Flag | Required | Description |
|---|---|---|
| `--version <id>` | Yes | Version to restore. |
| `--force` | No | Restore even while a lock is held. Make sure nothing is running first. |

```bash
snowstate state restore platform/network/prod --version 01a19099-35e9-710b-b7b2-8a6e547e245a
```

```text
restored as version 01a190c5-dab3-7000-b754-bbe55ff269ee
```

## snowstate lock list

Lists held locks. For GitHub Actions locks, `BY` is `github:<repository>#<run_id>/<run_attempt>`, which identifies the run.

```text
snowstate lock list
```

It has no flags of its own.

```text
KEY                    ID                                    OPERATION           WHO      SOURCE  ACQUIRED              BY
platform/network/prod  6f1c2d1e-8a44-4e0b-9d7c-1a2b3c4d5e6f  OperationTypeApply  me@host  HTTP    2026-10-31T10:15:00Z  TODD
```

## snowstate lock release

Releases a lock, whoever holds it, and audits it as `FORCE_UNLOCK`. It shows the holder and asks first. If there is no
answer, as in a script with no input, it refuses and changes nothing.

It works even with `locking.allow_force_unlock: false`, which only restricts the state API, so that operators can always
clear a stuck lock. **A lock left by a crash may mean a partial apply**, so find out why it is held first.

```text
snowstate lock release <key> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--force` | off | Do not ask for confirmation. Use this in scripts. |

```text
$ snowstate lock release platform/network/prod
Lock 6f1c2d1e-8a44-4e0b-9d7c-1a2b3c4d5e6f on platform/network/prod
  who: me@host
  operation: OperationTypeApply
  acquired: 2026-10-31T10:15:00Z by TODD
Release it? Check for a partial apply first. [y/N] y
released lock 6f1c2d1e-8a44-4e0b-9d7c-1a2b3c4d5e6f on platform/network/prod
```

```bash
snowstate lock release platform/network/prod --force
```

If nothing is held, it prints `no lock held on <key>` and exits `0`.

## snowstate project create

Creates a project's schema in `schema_per_project` tenancy, applies the migrations, grants the roles, records the project
and rebuilds the cross-project inventory view. Run it as `<DB>_ADMIN`. The roles must already exist.

The schema is `P_<PROJECT>`, upper-cased, with anything non-alphanumeric changed to `_`. A name that would land on another
project's schema is refused: `my-app` and `my_app` cannot both exist. It fails unless `tenancy.mode` is `schema_per_project`.

```text
snowstate project create <name> --role <role> [flags]
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--role <role>` | Yes | | Snowflake role for this project. |
| `--permissions <list>` | No | `read,write,lock` | Permissions for `--role`, comma-separated or repeated. |
| `--plan-role <role>` | No | | A second role with `read,lock` only, for plan jobs. |

No permission set can update or delete `STATE_VERSIONS` or `AUDIT_LOG`, so project roles cannot erase history.

```bash
snowstate project create platform --role TF_STATE_P_PLATFORM \
  --plan-role TF_STATE_P_PLATFORM_PLAN
```

```text
project platform uses schema P_PLATFORM
```

## snowstate project list

Lists projects with their schema, roles and permissions.

```text
snowstate project list
```

It has no flags of its own.

```text
PROJECT   SCHEMA      ROLE                 PLAN_ROLE                 PERMISSIONS
platform  P_PLATFORM  TF_STATE_P_PLATFORM  TF_STATE_P_PLATFORM_PLAN  read,write,lock
```

## snowstate gc

Removes old data, following `retention.*`:

- **Old versions:** a version goes only if it is beyond both `keep_versions` (default 100) and `keep_days` (default 0). The
  current version always stays, and `keep_versions: 0` keeps everything.
- **Deleted states:** soft-deleted states older than `deleted_days` (default 30) are purged with all their versions. Locked
  states are skipped.
- **Orphan blobs:** stage files no version points to, once they are an hour old.

**This permanently deletes data.** Run `--dry-run` first.

```text
snowstate gc [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | off | Report what would go, without deleting anything. |

```text
$ snowstate gc --dry-run
would remove 0 versions, 0 states, 0 orphan blobs

$ snowstate gc
removed 0 versions, 0 states, 0 orphan blobs
```

## snowstate version

Prints the version, commit and build time.

```text
$ snowstate version
snowstate 0956f95 (commit 0956f95, built 2026-10-31T23:44:00Z)
```

It has no flags of its own.

## snowstate completion

Prints a shell completion script.

```text
snowstate completion <bash|zsh|fish|powershell> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--no-descriptions` | off | Leave command descriptions out of completions. |

```bash
# zsh, current session
source <(snowstate completion zsh)

# bash, current session
source <(snowstate completion bash)

# fish
snowstate completion fish > ~/.config/fish/completions/snowstate.fish

# PowerShell
snowstate completion powershell | Out-String | Invoke-Expression
```

## snowstate help

Shows help for any command. It is the same as `--help`.

```bash
snowstate help
snowstate help token create
snowstate state restore --help
```
