# GitHub Actions

The recommended setup is **sidecar mode with workload identity federation**: no snowstate server, and no stored
Snowflake secret. Each job gets a short-lived OIDC token from GitHub and logs in to Snowflake with it.

## Snowflake setup

Create one service user per repository and environment. `deploy/snowflake/bootstrap.sql` part C has the full version:

```sql
CREATE USER GH_PLATFORM_NETWORK_PROD
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_P_PLATFORM          -- project role, or SNOWSTATE_SVC for shared tenancy
  DEFAULT_WAREHOUSE = SNOWSTATE_WH
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod'
  );
GRANT ROLE SNOWSTATE_P_PLATFORM TO USER GH_PLATFORM_NETWORK_PROD;
```

`SUBJECT` must equal the token's `sub` claim exactly:

| The job runs on | `sub` |
|---|---|
| An environment | `repo:ORG/REPO:environment:NAME` |
| A branch, no environment | `repo:ORG/REPO:ref:refs/heads/BRANCH` |
| A tag, no environment | `repo:ORG/REPO:ref:refs/tags/TAG` |
| A pull request, no environment | `repo:ORG/REPO:pull_request` |

If a job references an environment, the environment form always applies, whatever the trigger. A colon in a value becomes
`%3A`.

**Bind apply to an environment with required reviewers.** That is the safest choice, because a branch-bound identity can
be used by anything that runs on that branch.

### Separate plan and apply identities

`terraform plan` takes a lock but never writes state, so plan jobs get a weaker role. Create both at once:

```bash
snowstate project create platform --role SNOWSTATE_P_PLATFORM --plan-role SNOWSTATE_P_PLATFORM_PLAN
```

The plan role gets `SELECT` on the tables, `INSERT, DELETE` on `LOCKS` and `INSERT` on `AUDIT_LOG`, with no write on
`STATES`, `STATE_VERSIONS` or the stage. Only the `prod` apply job maps to a `read,write,lock` role.

A plan identity can't change state, but it can still hold a lock and block others. That is the trade-off.

`terraform plan` does not call the write endpoint, including with `-out` and `-refresh-only`.

## Network access

GitHub-hosted runners have no fixed IPs. If the account uses a network policy, choose one:

1. **Allow GitHub's published Actions ranges.** The list from `https://api.github.com/meta` is large and changes, so it
   needs automation, and it admits every GitHub customer's runners.
2. **Larger runners with static IPs.** These need GitHub Enterprise Cloud. Larger runners themselves are available on Team
   too, but static IPs are not. Static ranges carry no extra charge, are released after 90 days unused, and are not
   available on macOS or with Azure private networking.
3. **Self-hosted runners** inside the corporate network.

`snowstate doctor` recognises a network-policy rejection and prints these options.

## The setup action

```yaml
- uses: thunter/snowstate/action/setup@v0
```

`@v0` installs the newest stable `v0` release, so each run can pick up a newer one. To stay on one version, pin it:
`@v0.1.0`. Prereleases are only installed by their full version.

| Input | Default | |
|---|---|---|
| `version` | The action's own tag | A release such as `v1.2.3`, or `v1` for the newest `v1` release |
| `verify-checksum` | `true` | Verify against `checksums.txt` |

It installs the right binary for the runner (linux, darwin or windows; amd64 or arm64), adds it to `PATH`, caches it by
version, and outputs `path` and `version`. It needs no Node runtime.

## Reference workflow

`examples/github-actions/terraform.yml` plans on pull requests, and plans, waits for approval and applies on `main`.
The module declares `backend "http" {}`, and `snowstate exec` supplies the rest.

Set `terraform_wrapper: false` on `hashicorp/setup-terraform`, which keeps exit codes and signals clean.

### Concurrency

Use `concurrency` with `cancel-in-progress: false` on anything that applies. Runs queue per state rather than racing, and
a running apply is not cancelled. The lock is the safety net, and concurrency is what stops people hitting it.

### Saved plans are safe across jobs

The sidecar passes its address and password only through `TF_HTTP_*` variables. They never reach the backend block,
`.terraform/` or the plan file, so a plan from one job applies in another with a different port and password.

If state changed between plan and apply, Terraform rejects the plan as stale. That is correct, so re-run the plan.

The plan file contains sensitive values. Keep artifact retention short.

### Cancellation

When a run is cancelled, the runner sends the step's entry process SIGINT, then SIGTERM after 7.5 s, then kills the
process tree 2.5 s later. The server force-terminates anything left after 5 minutes.

The sidecar forwards each signal to Terraform and keeps serving until Terraform exits, so Terraform can write state and
unlock during a graceful stop. It must finish within those 10 seconds. If the job is killed mid-apply:

- The lock stays held on purpose, because a partial apply must not be silently unlocked.
- Terraform may have left `errored.tfstate`, which the workflow uploads.
- The sidecar's message names the lock ID and the release command, when it survives to print them.

`docs/runbook.md` covers recovery.

### What the sidecar adds in Actions

When `GITHUB_ACTIONS=true`, the sidecar:

- adds `repository`, `workflow`, `job`, `run_id`, `run_attempt`, `actor`, `sha`, `ref` and `run_url` to the `DETAIL` of
  every audit row;
- sets `ACQUIRED_BY` on locks to `github:<repository>#<run_id>/<run_attempt>`;
- appends a summary to `$GITHUB_STEP_SUMMARY`: the state key, the version before and after, the lock acquired and
  released, and any conflict.

### Cost

About one warehouse resume per job, or roughly 0.017 credits on an X-Small.

## Alternative: key pair

If workload identity is not an option, store a dedicated key as a secret:

```yaml
env:
  SNOWSTATE_SNOWFLAKE__ACCOUNT: ${{ vars.SNOWFLAKE_ACCOUNT }}
  SNOWSTATE_SNOWFLAKE__USER: GH_PLATFORM_NETWORK_PROD
  SNOWSTATE_SNOWFLAKE__AUTH: keypair
  SNOWSTATE_SNOWFLAKE_PRIVATE_KEY_PEM: ${{ secrets.SNOWFLAKE_PRIVATE_KEY }}
```

Use a key made for CI, not a personal one.

## Alternative: a shared server

With a `snowstate serve` the runner can reach, workflows can skip the sidecar and use a scoped token:

```yaml
env:
  TF_HTTP_ADDRESS: https://snowstate.example.com/v1/states/platform/network/prod
  TF_HTTP_LOCK_ADDRESS: https://snowstate.example.com/v1/states/platform/network/prod
  TF_HTTP_UNLOCK_ADDRESS: https://snowstate.example.com/v1/states/platform/network/prod
  TF_HTTP_USERNAME: ci-platform
  TF_HTTP_PASSWORD: ${{ secrets.SNOWSTATE_TOKEN }}
```

The sidecar is still recommended: it needs no server, and no stored secret.
