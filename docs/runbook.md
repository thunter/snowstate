# Runbook

## A lock is stuck

Find it, and find out why it is held before releasing anything:

```bash
snowstate lock list
```

`WHO` names the person or host and `BY` names the principal. For GitHub Actions, `BY` is
`github:<repository>#<run_id>/<attempt>`, which identifies the run.

A lock left behind after a crash may mean a partial apply. Check the run's logs first. Then release it, by whichever is
convenient:

```bash
terraform force-unlock <lock-id>        # from the module, needs the lock permission
snowstate lock release <key>            # shows the holder and asks
snowstate lock release <key> --force    # for scripts
```

Each writes a `FORCE_UNLOCK` audit row containing the removed lock.

`tofu force-unlock` requires the correct ID, and a wrong one fails with `Unexpected HTTP response code 409`.
`terraform force-unlock` sends no ID to the server, so it removes whatever is held.

### A GitHub job was killed mid-apply

The lock stays held on purpose, because a partial apply must not be silently unlocked. Terraform may have written
`errored.tfstate`, which the reference workflow uploads as an artifact.

1. Open the run URL from the audit row or from `snowstate lock list`.
2. Download the `errored-state` artifact, if there is one.
3. Push it through a sidecar. The state is locked, so hold the lock ID from `lock list` and release first, or use a
   maintenance window so nothing else can start:

   ```bash
   snowstate lock release platform/network/prod
   snowstate exec --key platform/network/prod -- terraform state push errored.tfstate
   ```

4. Run `terraform plan` and reconcile.

### Lock TTL

`locking.ttl` lets a new `LOCK` steal an old lock, audited as `FORCE_UNLOCK` with reason `ttl`. It is off by default,
because Terraform has no heartbeat and long applies are legitimate.

## Snowflake is down

Clients get `503` with `Retry-After: 2`, and Terraform retries, by default twice. `/readyz` turns `503`. `/healthz` stays
`200`, so an orchestrator will not restart healthy pods.

- Terraform commands fail cleanly once retries run out. Nothing is lost: a write either committed or did not.
- If a `COMMIT` outcome is unknown, snowstate re-reads and reports success only if the version exists.
- Buffered audit rows for reads and denials wait in memory and retry. Audit rows for changes commit with the change.
- A failed `terraform apply` writes `errored.tfstate`. Push it once Snowflake is back.

Check `snowstate doctor`, and look at `snowstate_snowflake_errors_total` by `code`.

## Rolling back a bad state

```bash
snowstate state versions platform/network/prod --limit 20
snowstate state pull platform/network/prod --version <id> | jq '.resources | length'
snowstate state restore platform/network/prod --version <id>
terraform plan
```

## Rotating credentials

**Snowflake key.** Set `RSA_PUBLIC_KEY_2` on the user, deploy the new private key, confirm with `snowstate doctor`, then
unset `RSA_PUBLIC_KEY`.

**API token.** Create the replacement, update the client, then revoke the old one. Revocation takes effect within 60 s.

**Token pepper.** Changing it invalidates every token. Reissue them all first, in a maintenance window.

## Reading the audit log

```sql
-- Everything that happened to one state
SELECT EVENT_TIME, ACTION, RESULT, PRINCIPAL, VERSION_ID, LOCK_ID
FROM TF_STATE.BACKEND.AUDIT_LOG
WHERE STATE_KEY = 'platform/network/prod'
ORDER BY EVENT_TIME DESC LIMIT 100;

-- Force unlocks, with the lock each removed
SELECT EVENT_TIME, PRINCIPAL, STATE_KEY, DETAIL:reason::string AS REASON,
       PARSE_JSON(DETAIL:removed_lock::string):Who::string AS HOLDER
FROM TF_STATE.BACKEND.AUDIT_LOG
WHERE ACTION = 'FORCE_UNLOCK' ORDER BY EVENT_TIME DESC;

-- Denied requests
SELECT EVENT_TIME, PRINCIPAL, REMOTE_ADDR, ACTION, STATE_KEY
FROM TF_STATE.BACKEND.AUDIT_LOG WHERE RESULT = 'DENIED' ORDER BY EVENT_TIME DESC;

-- Which GitHub run wrote each version
SELECT VERSION_ID, EVENT_TIME, DETAIL:run_url::string AS RUN
FROM TF_STATE.BACKEND.AUDIT_LOG
WHERE ACTION = 'PUT' AND STATE_KEY = 'platform/network/prod' ORDER BY EVENT_TIME DESC;
```

## Tracing a request to Snowflake

Each statement's `QUERY_TAG` is `snowstate:<op>:<request_id>`, and `X-Request-Id` is on every response:

```sql
SELECT QUERY_TEXT, TOTAL_ELAPSED_TIME, ERROR_MESSAGE
FROM SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY
WHERE QUERY_TAG LIKE 'snowstate:%:<request_id>' ORDER BY START_TIME;
```

## Lock errors under heavy contention

A hybrid row allows about 20 waiting transactions. snowstate retries such conflicts for up to 30 s, then returns `503`,
which clients retry. This is safe, because a retry can only lose to the primary key.
