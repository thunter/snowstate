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

## Recovering an Install

The Foundation's state lives in the Install it describes, so Terraform can only help once snowstate can reach that state
again. Each procedure below was run on a real Install. `<DB>` is the database name, and the examples use the derived role
names and the `BACKEND` schema, so substitute any you overrode.

During an outage you can't read `terraform output`, because that needs the state. Set the values by hand instead:

```bash
export SNOWSTATE_SNOWFLAKE__ACCOUNT=<org>-<account>
export SNOWSTATE_SNOWFLAKE__USER=SNOWSTATE_SEED
export SNOWSTATE_SNOWFLAKE__ROLE=<DB>_SVC
export SNOWSTATE_SNOWFLAKE__WAREHOUSE=<DB>_WH
export SNOWSTATE_SNOWFLAKE__DATABASE=<DB>
export SNOWSTATE_SNOWFLAKE__SCHEMA=BACKEND
export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH=~/.snowflake/keys/snowstate_seed.p8
export SNOWSTATE_KEY=snowstate/foundation/default
```

Afterwards, `snowstate doctor` should pass, and `snowstate exec -- terraform plan` should show no changes. Check the Seed
user with `DESC USER SNOWSTATE_SEED` if `doctor` looks wrong: with secondary roles on, a session also has the user's other
roles, and `doctor` can pass while the runtime role is broken. `doctor` doesn't touch the stage, so a stage problem only
shows up when a large state is written.

To read the state while the runtime role is broken:

```bash
SNOWSTATE_SNOWFLAKE__ROLE=<DB>_ADMIN snowstate state pull snowstate/foundation/default
```

### The warehouse was dropped

`doctor` and `snowstate exec` fail with `390201 ... requested warehouse does not exist`. As the Seed role, recreate it under
the same name:

```sql
CREATE WAREHOUSE <DB>_WH WAREHOUSE_SIZE = XSMALL AUTO_SUSPEND = 60 AUTO_RESUME = TRUE INITIALLY_SUSPENDED = TRUE;
GRANT USAGE ON WAREHOUSE <DB>_WH TO ROLE <DB>_SVC;
```

Then `snowstate exec -- terraform apply` restores the grants for the other roles.

### The runtime role lost its schema access

`doctor` fails with `390201 ... requested schema does not exist`. Apply as the admin role:

```bash
SNOWSTATE_SNOWFLAKE__ROLE=<DB>_ADMIN snowstate exec -- terraform apply
```

### The runtime role lost its table or stage grants

`doctor` fails its `migrations` and `privileges` checks, and Terraform reports `HTTP remote state endpoint invalid auth`.
**Terraform cannot fix this:** it manages future grants, which only cover new objects, so a plan as `<DB>_ADMIN` shows no
changes. `<DB>_ADMIN` cannot regrant either, because in a managed-access schema that takes the schema owner. As the Seed
role:

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_SVC;
GRANT READ, WRITE ON ALL STAGES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_SVC;
```

### The admin role was dropped

`doctor` fails with `390186 ... Role '<DB>_SVC' ... is not granted to this user`, because the Seed user reaches the runtime
role through the admin role. Dropping a role also removes the grants it issued, and hands what it owned to the role that
dropped it. As the Seed role:

```sql
CREATE ROLE <DB>_ADMIN;
GRANT ROLE <DB>_SVC TO ROLE <DB>_ADMIN;
GRANT ROLE <DB>_ADMIN TO ROLE SNOWSTATE_SEED;
GRANT ROLE <DB>_ADMIN TO USER SNOWSTATE_SEED;
GRANT USAGE ON WAREHOUSE <DB>_WH TO ROLE <DB>_ADMIN;
GRANT USAGE ON DATABASE <DB> TO ROLE <DB>_ADMIN;
GRANT USAGE ON SCHEMA <DB>.BACKEND TO ROLE <DB>_ADMIN;
```

As the role that dropped it, here `ACCOUNTADMIN`, give back what it owned:

```sql
GRANT OWNERSHIP ON ALL TABLES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_ADMIN COPY CURRENT GRANTS;
GRANT OWNERSHIP ON ALL STAGES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_ADMIN COPY CURRENT GRANTS;
GRANT OWNERSHIP ON ALL VIEWS IN SCHEMA <DB>.BACKEND TO ROLE <DB>_ADMIN COPY CURRENT GRANTS;
```

Then apply as `<DB>_ADMIN`, and finish as the Seed role with the existing-object grants:

```bash
SNOWSTATE_SNOWFLAKE__ROLE=<DB>_ADMIN snowstate exec -- terraform apply
```

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_SVC;
GRANT READ, WRITE ON ALL STAGES IN SCHEMA <DB>.BACKEND TO ROLE <DB>_SVC;
GRANT SELECT ON ALL VIEWS IN SCHEMA <DB>.BACKEND TO ROLE <DB>_READER;
```

### The Seed role was dropped

`doctor` fails with the same `390186`. This is the worst case: every grant the role issued is gone, and everything it owned
now belongs to the role that dropped it. It affects every Install in the account, so repeat the per-Install steps for each.
If you run a service user, also transfer it with `GRANT OWNERSHIP ON USER <DB>_SERVICE`, a step the drill did not include.
As `ACCOUNTADMIN`:

```sql
CREATE ROLE SNOWSTATE_SEED;
GRANT CREATE WAREHOUSE, CREATE DATABASE, CREATE ROLE, CREATE USER, MANAGE GRANTS ON ACCOUNT TO ROLE SNOWSTATE_SEED;
GRANT ROLE SNOWSTATE_SEED TO ROLE SYSADMIN;
GRANT ROLE SNOWSTATE_SEED TO USER SNOWSTATE_SEED;

GRANT OWNERSHIP ON DATABASE <DB> TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
GRANT OWNERSHIP ON SCHEMA <DB>.BACKEND TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
GRANT OWNERSHIP ON WAREHOUSE <DB>_WH TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
GRANT OWNERSHIP ON ROLE <DB>_ADMIN TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
GRANT OWNERSHIP ON ROLE <DB>_SVC TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
GRANT OWNERSHIP ON ROLE <DB>_READER TO ROLE SNOWSTATE_SEED COPY CURRENT GRANTS;
```

Then, as the Seed role, rebuild the path to the state:

```sql
GRANT ROLE <DB>_SVC TO ROLE <DB>_ADMIN;
GRANT ROLE <DB>_ADMIN TO ROLE SNOWSTATE_SEED;
GRANT ROLE <DB>_ADMIN TO USER SNOWSTATE_SEED;
GRANT USAGE ON WAREHOUSE <DB>_WH TO ROLE <DB>_ADMIN;
GRANT USAGE ON DATABASE <DB> TO ROLE <DB>_ADMIN;
GRANT USAGE ON SCHEMA <DB>.BACKEND TO ROLE <DB>_ADMIN;
```

Finish as you do for a dropped admin role: apply as `<DB>_ADMIN`, then run the existing-object grants as the Seed role.

### Disabling the Seed user

Normal stacks don't use `SNOWSTATE_SEED`, so you can keep it disabled between Foundation changes. It logs in nowhere while
disabled, so re-enable it before any change to the Foundation's state, and before any procedure above. As `ACCOUNTADMIN`:

```sql
ALTER USER SNOWSTATE_SEED SET DISABLED = TRUE;
ALTER USER SNOWSTATE_SEED SET DISABLED = FALSE;
```

## Removing an Install

**This drops the database, so every state in the Install goes with it.** There is no `snowstate` command for this, on
purpose. `terraform destroy` with state in snowstate would drop the database holding its own state partway through, so move
the state out first. In the Foundation directory, with the settings loaded:

```bash
mv backend.tf backend.tf.disabled
snowstate exec -- terraform init -migrate-state
```

Delete the two `prevent_destroy` lifecycle blocks in `main.tf`, then:

```bash
terraform destroy
rm terraform.tfstate terraform.tfstate.backup
```

Nothing from the Install is left behind. The Seed stays, because other Installs in the account may use it.
