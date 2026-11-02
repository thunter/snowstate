# Decisions and verification log

Every VERIFY outcome and every deviation from `SPEC.md` is recorded here.

Sources read for the protocol section: `hashicorp/terraform` tags `v1.17.1` (ff6329e) and `v1.5.7`,
`opentofu/opentofu` `main` (6356fbe, 2026-10-30, `tofu` 1.13.2 is the installed binary), and
`hashicorp/go-retryablehttp` `v0.7.8` (the version both pin). Paths below are relative to
`internal/backend/remote-state/http/` unless stated. `TF` = Terraform 1.17.1, `OTF` = OpenTofu main.

## Answers to SPEC §15

| # | Answer | Source |
|---|---|---|
| A1 | AWS: `AWS_AP_SOUTHEAST_6`, Snowflake 10.39.101. Hybrid tables expected GA. | `SELECT CURRENT_REGION()`, spec owner edit |
| A7 | Postgres backend ruled out. | spec owner edit |
| A10 | The account has no account-level network policy. | `SHOW PARAMETERS LIKE 'NETWORK_POLICY' IN ACCOUNT` |
| Others | Defaults from §15. | |

BCR bundle `2025_03` status is `RELEASED`, so the 64 MB BINARY / 128 MB VARCHAR limits apply.

## Protocol (§4.0)

| Question | Finding | Reference |
|---|---|---|
| Status codes treated as "no state" on GET | `204` and `404`, plus a `200` with an empty body. We send `204`. Both clients treat `401`, `403`, `500` as errors with specific messages; anything else is "Unexpected HTTP response code". | TF `client.go:154-169`, `:183`; OTF `client.go:192-207` |
| `Content-MD5` on POST | Sent on every request that has a body, as base64 of the MD5. | TF `client.go:61-69`; OTF `client.go:71-77` |
| `Content-MD5` on GET response | Read when present. If absent the client computes its own. A malformed value is an error. | TF `client.go:188-200`; OTF `client.go:227-238` |
| Lock body | `statemgr.LockInfo` JSON with exactly `ID, Operation, Info, Who, Version, Created, Path`. The ID is client-generated. | TF `client.go:86-87`, `internal/states/statemgr/locker.go:118-141`, `:180-186` |
| Lock success and conflict | `200` is success. `409` or `423` is a conflict, and the body must be `LockInfo` JSON or the error message loses the holder. `401`/`403` have their own messages. | TF `client.go:93-123` |
| Normal unlock body | The same `LockInfo` JSON sent at lock time. Only `200` is success. | TF `client.go:131-142`; OTF `client.go:148-180` |
| **Terraform `force-unlock` body** | **Empty body**, confirmed. A fresh process has nil `jsonLockInfo`, so the request has no body. `Content-MD5` of the empty string is still sent because the pointer is non-nil. The ID typed by the user is **not sent**; Terraform 1.5.7 behaves the same. | TF `client.go:61`, `:131`; TF 1.5.7 `client.go:133`; `internal/command/unlock.go:107` |
| **OpenTofu `force-unlock` body** | **Not empty, which differs from the spec's belief.** OTF builds a zero `LockInfo`, sets only `ID` to the user-supplied value and sends it: `{"ID":"<id>","Operation":"","Info":"","Who":"","Version":"","Created":"0001-01-01T00:00:00Z","Path":""}`. | OTF `client.go:148-166`, `internal/command/unlock.go:135` |
| Write URL | `?ID=<lockID>` is added only if this process took the lock. | TF `client.go:209-215` |
| Write success | `200`, `201` or `204`. Anything else is "HTTP error: N" and the body is ignored. | TF `client.go:237-242` |
| Delete | No body and no `?ID=`. Only `200` is success. The client never sends `?ID=` on delete. | TF `client.go:249-261` |
| Workspaces | Not supported by the http backend in either client. Only `default` works. | TF `backend.go:309-331` |
| Retry | `go-retryablehttp`: retries connection errors, `429`, and `5xx` other than `501`. It does not retry `4xx`. `Retry-After` is honoured on `429` and `503`. Defaults: `retry_max=2`, `retry_wait_min=1`, `retry_wait_max=30`. | go-retryablehttp `client.go:495-542`, `:553`; TF `backend.go:187-219` |
| Env vars for connection config | `TF_HTTP_ADDRESS`, `TF_HTTP_LOCK_ADDRESS`, `TF_HTTP_UNLOCK_ADDRESS`, `TF_HTTP_USERNAME`, `TF_HTTP_PASSWORD` and the method/retry variants are read at Configure time. | TF `backend.go:121-246` |
| New config args | OTF adds `headers` (a map; `Authorization` conflicts with `username`). TF has none beyond §4.1. `headers` would make the SPCS non-goal in §1.2 reachable for OpenTofu only. | OTF `backend.go:124-148`, `:254-273` |
| `terraform plan` writes state? | No write path in `opPlan`. It only reads and locks. `plan -refresh-only` is also a plan; only `terraform refresh` persists. Runtime proof is e2e scenario 9. | TF `internal/backend/local/backend_plan.go`, `backend_refresh.go:110` |

### Deviations that follow from the protocol findings

**D1. OpenTofu force-unlock is recognised by shape.** §4.3 treats only an empty body as force-unlock. With that rule,
`tofu force-unlock <id>` would look like a normal unlock and never write the `FORCE_UNLOCK` audit row that scenario 3
requires, and it would bypass `locking.allow_force_unlock`. So an UNLOCK whose body has an `ID` but empty `Who` and
`Operation` is treated as a **force unlock conditional on the ID**. A matching ID releases the lock and audits
`FORCE_UNLOCK`. A wrong ID returns `409` and changes nothing, which is safer than Terraform's unconditional path.
Normal unlocks from both clients always carry `Who` and `Operation`.

**D2. A DELETE with a lock held always returns 423.** The client never sends `?ID=` on delete, so a holder cannot prove
ownership unless it adds the parameter by hand. This is the §4.3 rule applied literally. It is called out because
`terraform workspace delete` does not apply to http.

## Scaffold decisions

- Module path is `github.com/thunter/snowstate`, from the `origin` remote.
- gosnowflake started on v1.19.1, the newest v1.x, as §2 requires. See D36 for the later move to v2.
- `koanf` loads flag values with `k.Set` rather than through the `posflag` provider. That lets each command expose only
  the flags it needs, and the precedence stays flags > env > file.
- Only `SNOWSTATE_*` variables containing `__` are config keys, so `SNOWSTATE_KEY`, `SNOWSTATE_TOKEN_PEPPER` and
  `SNOWSTATE_IT_*` cannot leak into the config tree.

## M1 decisions

**D3. OpenTofu encrypted states do not need `version`.** §4.3 rejects a body without `version`, but §10.8 requires
encrypted states to work, and OpenTofu's encrypted payload is
`{"serial":N,"lineage":"…","meta":…,"encrypted_data":…,"encryption_version":"v0"}`
(`internal/encryption/base.go:107`, `state.go:68-69`). A body with `encryption_version` is exempt from the `version`
check. `serial` and `lineage` are still required.

**D4. `Lock` audit rows share the lock's transaction.** §6.5 shows acquire as an autocommit `INSERT`, while §6.1 says
LOCK audit rows are written in the same transaction as the change. §6.1 is the stronger guarantee, so acquire runs as
`BEGIN; INSERT LOCKS; INSERT AUDIT_LOG; COMMIT`.

**D5. `store.Tx` keeps the rules in `service`.** The service runs the §6.4 steps as a closure inside `Store.InTx`, so
both stores get identical logic and neither contains rules.

**D6. `*` in a scope pattern crosses `/`.** `platform/*` has to cover `platform/network/prod`, as §5.1's examples
intend, so `path.Match` semantics would not work. `?` does not match `/`.

**D7. mTLS principals get their scopes from `TOKENS`.** §5.1 maps a CN to a principal but does not say where its scopes
come from. They are the union of that principal's live tokens, so deny-by-default still holds.

**D8. `ListStates`-style endpoints authorize with an empty key.** The authorizer treats `""` as "has this permission on
any key", and results are then filtered per key.

**D9. Test store persistence.** `--store=memory` also honours `SNOWSTATE_TEST_STORE_FILE`, which snapshots the store to
JSON after each change. Separate `exec` processes need this to share state in scenarios 9 and 10, and it is how e2e
tests read the audit log. It sits behind the same `SNOWSTATE_ALLOW_TEST_STORE=1` gate.

**D10. Additional typed errors.** `ErrInvalidLock`, `ErrSerialRegression` and `ErrCommitUnknown` extend the list in §3.

E2E findings: `terraform state pull` prints through a helper that appends one newline, so byte comparison must allow
for it. `state push` makes the client bump `serial` again when it persists.

## M2 decisions

**D11. The sidecar's child runs in its own process group.** §8.1 says to forward SIGINT and SIGTERM, but a terminal
Ctrl-C signals the whole foreground group, so forwarding alone delivers it twice, and Terraform treats a second SIGINT as
"abort immediately". With `Setpgid` the child only sees the forwarded signal. On a terminal the child's group is also
made the foreground group (`Foreground`/`Ctty`), otherwise its prompts stop with SIGTTIN. Verified on a real pty:
`read` works and Ctrl-C arrives once. `TestSidecarSignalReachesChildOnce` covers the non-tty case by signalling the group.

**D12. Sidecar logging defaults to warnings on stderr.** Terraform owns the terminal, so `log.level=info` (the default)
is lowered to `warn` for `exec`, in text format. Any other configured level is honoured.

**D13. Audit principal in sidecar mode is `snowflake.user`.** The Basic username is the fixed value `sidecar` from §8.1,
which would make audit rows useless, so `LoopbackSecret.Identity` replaces it with the Snowflake user and `Source` is
`snowflake`.

**D14. After `--shutdown-grace` following a second signal, the child is killed.** §8.1 says the server stops then, and
leaving a child running against a dead backend would only produce confusing errors. The lock stays held and the release
command is printed.

## Snowflake verification (M3)

Verified on a real account in region `AWS_AP_SOUTHEAST_6`, Snowflake 10.39.101. First run on gosnowflake v1.19.1, and re-run in full on v2.3.0 (D36).

| VERIFY | Outcome |
|---|---|
| Hybrid PK violation code (§6.5) | Number **200001**, SQLSTATE 22000. Matched on the number. |
| Single transaction across hybrid and standard tables | Works: `INSERT` into both then `COMMIT`. `FOR UPDATE` on a hybrid row also works inside it. |
| `DECOMPRESS_STRING(..., 'GZIP')` (§6.2) | **Does not work.** Error 100194 `Unknown compression method 'gzip'`. See D15. |
| Maximum BINARY bind (§6.3) | An 8.2 MB (7.8 MiB) payload binds and round-trips, so the 8 MiB default stands. `TestInlineMaxBinds` keeps this honest. |
| Key length on a hybrid PK | 512 characters works. |
| BCR 2025_03 | `RELEASED`; `BINARY(67108864)` is accepted. |
| Concurrency test, hybrid (§12.3) | **Passed**: exactly one winner in each of 10 rounds of 25 goroutines. |
| Standard-table `MERGE` with binds | Needs explicit casts (`?::VARCHAR`), used throughout. |
| WIF field names (§5.2) | `Authenticator = AuthTypeWorkloadIdentityFederation`, `WorkloadIdentityProvider`, `Token`. Exercised in M6.5. |
| Stream PUT/GET | `WithFileStream` for PUT; `WithFileTransferOptions{GetFileToStream:true}` + `WithFileGetStream` for GET. The object name comes from the base name of the file URL. With driver v2, GET uses `SnowflakeConnection.DownloadStream`, and a missing object is error 264006 (see D36). |

Error numbers acted on (`store/snowflake/errors.go`): 200001 (duplicate key), 200010 and 200012 (hybrid row contention),
264006 (no file to GET), 2003 and 3001 (access), 390114 (session expired), 606 (warehouse), 625/628/630 (timeouts).

**D15. Inline blobs are zlib, not gzip.** §6.2 said to switch encoding if gzip did not work. Snowflake's `COMPRESS` supports
`SNAPPY`, `ZLIB`, `ZSTD` and `BZ2`. Go's `compress/zlib` output (RFC 1950) decompresses unchanged, with no framing
prefix. Inline bytes are now zlib, and the column is `CONTENT_ZLIB` rather than `CONTENT_GZIP`, since nothing had been
deployed. Stage blobs stay gzip with `.json.gz`, as §6.3 specifies.

**D16. The inventory view is guarded.** There is no `TRY_DECOMPRESS_STRING`, and one bad row would fail the view for
every state. The view uses `IFF(SIZE_BYTES <= 134217728, DECOMPRESS_STRING(...), NULL)` and `TRY_PARSE_JSON`. A state
beyond VARCHAR's 128 MiB maximum is absent from the view rather than breaking it. A 120 MiB state was tested and is listed.

**D17. Row contention is retried for up to 30 s.** Inside a transaction, a statement that would wait on a hybrid row lock
is aborted (200010), and a row's wait queue is capped at about 20 (200012). `InTx` rolls back and retries with jitter for
up to `LOCK_TIMEOUT`'s default of 30 s, then returns `ErrUnavailable`, which clients see as `503` and retry. This is safe
because a retried transaction only ever loses to the primary key.

**D18. `Lock` reads first, and the lock row is the last statement.** §6.5 inserts and reads on violation. Doing that in a
transaction caused a priority inversion on the real account: blocked `INSERT`s occupy warehouse concurrency slots, the
winner's next statement queues behind them, and everyone stalls until `LOCK_TIMEOUT`. The order is now plain read, audit
insert, lock insert, `COMMIT`, so the winner holds the row only through `COMMIT`, and a loser's rollback removes its audit
row. A visibly held lock is reported with no write. The primary key still decides a true race. This costs one extra
`SELECT` per LOCK and took the 25-goroutine, 10-round test from 307 s to 38 s with no stalls.

**D19. The write fence uses `SELECT ... FOR UPDATE`.** §6.4 uses a plain read. Under READ COMMITTED a force-unlock plus a
new acquire could otherwise land between a holder's fence check and its commit. The row lock makes them wait. Conflict
reporting uses `Tx.PeekLock`, which takes no row lock, otherwise losers queue up just to report a conflict.

**D20. `net/http` in `sfconn`.** §3's rule says nothing under `pkg/` imports `net/http`, and that `TestImportBoundaries`
runs `go list -deps ./pkg/...`. gosnowflake itself depends on `net/http`, so that literal check cannot pass. The test
instead enforces three things: nothing in `pkg/` imports `internal/` or Terraform libraries, no package except `sfconn`
imports `net/http` directly (§5.2 needs it to fetch GitHub OIDC tokens), and the core (`service`, `keys`, `store/memory`)
is free of `net/http` and gosnowflake transitively.

**D21. `SCHEMA_MIGRATIONS` has a `SCOPE` column.** `schema_per_project` tracks versions per schema in one control table,
so the key is `(SCOPE, VERSION)`, not `VERSION` as in §6.1. `AUDIT_LOG` also exists in the control schema for rows with no
state key (token admin, GC). `PROJECTS` always exists, though it is only used in `schema_per_project`.

**D22. New dependency: `github.com/youmark/pkcs8`.** The stdlib cannot read encrypted PKCS#8 keys, which `private_key_passphrase`
needs.

**D23. `doctor` checks that `LOCKS` is hybrid, rather than creating a hybrid table.** The runtime role has no DDL, by design
(§10.5). `HybridSupported`, which does create one, is for `migrate`.

**D24. Driver logging is off.** `sfconn` sets gosnowflake's log level to `fatal` so that SQL text cannot reach logs.

**Integration environment note.** `SNOWSTATE_IT_ACCOUNT` in `~/.config/snowstate/it.env` still held the `dev_setup.md`
placeholder `orgname-accountname` during this build. Every other value matched the `snowstate_dev` connection, so test
runs set the account from that connection for the process only. `it.env` was not changed.

## M4 decisions

**D25. `lock release` asks first.** §8 gives `snowstate lock release <key> [--force]` without saying what `--force`
changes. Without it the holder is shown and the command asks for confirmation, and with no answer (a script, closed stdin)
it refuses. `--force` skips the question. Both audit `FORCE_UNLOCK`.

**D26. Operators can always release locks.** `locking.allow_force_unlock=false` restricts the state API only.
`DELETE /v1/admin/locks/...` (which needs `admin`) and the CLI still work, otherwise a stuck lock could not be cleared.

**D27. Restore bypasses the serial guard.** A restore goes backwards on purpose, and its bytes cannot change, so
`state.reject_serial_regression` does not apply to it. The new version has the old serial and sets `RESTORED_FROM`.

**D28. Restoring content that is already current is a no-op**, matching idempotent PUT, and returns the current version.

**D29. GC rules.** A version goes only when it fails both `keep_versions` and `keep_days`. The current version always
stays. `keep_versions: 0` means unlimited. A locked state is never purged. GC removes the pointer, then rows, then blobs,
so a crash can leave orphans but never a row pointing at a missing blob.

**D30. Orphan blobs must be an hour old.** Blobs upload before the metadata transaction, so a recent unreferenced blob may
be a write in flight. A blob with an unreadable timestamp is treated as recent, so it is kept. A test confirms real `LIST`
timestamps parse.

**D31. Project name collisions are refused.** `P_<PROJECT>` maps `my-app` and `my_app` to one schema, which would silently
merge tenants. `project create` refuses the second, because the schema is an isolation boundary.

**D32. `Principal.Scopes` extends §3's `Principal`** so the scope authorizer stays stateless.

**D33. Project roles' grants.** §5.3's lists are applied by `--permissions`: `read` gives `SELECT` on the tables and `READ`
on the stage, `lock` gives `SELECT, INSERT, DELETE` on `LOCKS`, `write` gives the `STATES`, `STATE_VERSIONS` and stage
privileges. Every role gets `INSERT` on `AUDIT_LOG`, and `USAGE` plus read access to the control tables that listing and
`migrate` checks need.

### Open: role isolation is not verified on a real account

`TestSchemaPerProjectIsolation` (§12.3) is written but **has not run**. It needs two roles the test user can assume, each
with `USAGE` on the warehouse. During this build the environment's permission policy refused an attempt to create a test
role and grant it, and `SNOWSTATE_DEV` also holds warehouse `USAGE` without `GRANT OPTION`. So `TestSchemaPerProjectRouting`
(no grants) is what ran. Whether `FOR UPDATE` on `LOCKS` needs more than `SELECT, INSERT, DELETE` is therefore also
unconfirmed. To run it, an admin creates the roles per `dev_setup.md` §3.2 and sets `SNOWSTATE_IT_ROLE_A` and
`SNOWSTATE_IT_ROLE_B`.

## M5: standard-mode locking

**Concurrency result (§6.5, §12.3).** `TestStandardModeConcurrency` ran 25 goroutines for 10 rounds on a reused row and
10 rounds on a fresh key, on the same account as above. **Every one of the 20 rounds had exactly one winner.** Two winners
never occurred, so standard mode is not marked unsafe and remains available behind `locking.standard_mode_acknowledged`.

This is one measurement, not a proof, and the mode stays EXPERIMENTAL for these reasons:

- **The primary key is not enforced.** Safety comes only from table-level DML locks serialising the transaction. Whether
  a blocked `UPDATE` re-evaluates its predicate is not documented behaviour, and this test only observes that it held.
- **The lock is on the whole `LOCKS` table.** Rounds took about 18 s against about 3.8 s for hybrid, and four racers per
  round hit `LOCK_TIMEOUT` (error 625), returning `503`. That is safe, but unrelated states contend with each other.
- **The write fence is weaker.** `FOR UPDATE` is hybrid-only, so D19's protection does not apply.

Hybrid remains the only mode recommended for production.

**D34. Standard mode clears lock rows rather than deleting them**, so later acquires skip the seed `MERGE`, the riskiest
step on a table with no enforced key. `getLock` and `ListLocks` filter on `LOCK_ID IS NOT NULL`.

**D35. `migrate` always probes hybrid support.** In hybrid mode, if the probe fails it stops with guidance rather than
failing later on `CREATE HYBRID TABLE`. In standard mode it reports when hybrid would be available. A dry run skips the
probe because it creates and drops a table.

## M6 decisions

**D36. gosnowflake v2.3.0 instead of v1.x (deviates from §2).** `govulncheck` reported GO-2026-6665 (CVE-2026-86597, the
driver writes sensitive information to logs) as reachable. It covers the whole v1 module path, and the fix shipped in
v2.2.0. §2 asks for the latest v1.x, but §10.7 requires `govulncheck` to pass and the project forbids logging secrets, so
security wins. The full integration suite was re-run on v2. API changes: `WithFilePutStream` replaces `WithFileStream`,
and blob reads use `SnowflakeConnection.DownloadStream`, which streams rather than buffering.

Driver v2.3 also turned OCSP checks off by default. `sfconn` sets `OCSPFailOpen: OCSPFailOpenTrue`, keeping v1's posture
(check revocation, fail open if the responder is unreachable).

Transitive upgrades for reachable or imported advisories: `aws-sdk-go-v2/service/s3` and `eventstream` (GO-2026-5764),
`golang.org/x/net` (GO-2026-66xx, 68xx), `go.opentelemetry.io/otel` (GO-2026-5506).

`govulncheck` still lists GO-2026-5932 (`golang.org/x/crypto/openpgp` is unmaintained) in a required module. There is no
fix, and neither this code nor its imports call it, so the scan passes.

**D37. Release builds are a Makefile target, not goreleaser.** `make release` cross-compiles the six platforms and writes
`checksums.txt`, which avoids a tool §2 does not list.

**D38. Docker was not available on the build machine**, so the image has not been built or run locally. CI builds it.

**D39. Runbook order for recovering `errored.tfstate`.** §8.4 says to `state push` and then release the lock. That cannot
work: `state push` must take the lock, and the crashed run still holds it. The runbook releases first, then pushes.

**D40. `migrate` and `project` connect without a session schema.** Following the README literally found that Snowflake
rejects a login naming a schema that does not exist (390201), so a first `migrate` could never run. The store qualifies
every name, so provisioning commands simply leave the session schema unset. `TestCLIMigrateCreatesTheSchema` covers it.

## M6.5: GitHub Actions

| VERIFY | Outcome |
|---|---|
| WIF fields (§5.2) | `Authenticator = AuthTypeWorkloadIdentityFederation`, `WorkloadIdentityProvider = "OIDC"`, and the JWT in `Token` (`auth_wif.go:86`, read through `sfconfig.GetToken`). **Not exercised against Snowflake**: see below. |
| OIDC token response (§5.2) | JSON field `value`, from `@actions/core` `oidc-utils.ts`. |
| `sub` claim formats (§8.4.1) | Recorded in `docs/github-actions.md`, from GitHub's OIDC reference. An environment takes precedence, and a colon becomes `%3A`. |
| Plan writes state? (§8.4.1) | **No**, for `plan`, `plan -refresh-only` and `plan -out`, on all three clients. Scenario 9 asserts no `PUT`. |
| Cancellation timings (§8.4.4) | SIGINT, then SIGTERM after 7.5 s, then process-tree kill after 2.5 s (`Runner.Sdk/ProcessInvoker.cs:32-33`, `:448-455`). The server force-terminates after 5 minutes. |
| Static IP runners (§8.4.2) | Need GitHub Enterprise Cloud, though larger runners also exist on Team. Not on macOS or with Azure private networking. Ranges are dropped after 90 days unused. |

**D41. The setup action uses one bash installer.** §8.4.3 says bash and PowerShell steps. Git Bash is on every hosted
runner, including Windows, so one `shell: bash` script covers all of them, with less to test.

**D42. The binary installs through `install.sh`, with a `BASE_URL` override** so CI can install a build of the current
commit from a local directory.

**D43. `cosign` signs `checksums.txt` in the release workflow, but the action does not verify it.** §8.4.3 makes this
conditional, and verifying would mean installing cosign on the runner. The checksum check is on by default.

**D44. The summary never shows arguments.** It names the program and subcommand only, because `-var` can carry secrets.

**D45. Network-policy detection is unverified.** The test account has no network policy, so 390432 could not be
reproduced. `doctor` also matches the documented message text as a fallback. This is the one place matching uses text.

**D46. A short cancellation window.** Terraform has 10 s from SIGINT to the process-tree kill, which is often not enough
to finish a graceful stop. This is why the lock is left held in that case and the docs cover recovery.

**D47. The reference workflow in §8.4.4 is not valid YAML as written.** `concurrency: { group: tf-${{ github.workflow }}-prod, ... }`
puts `${{ }}` braces inside a flow mapping, which a YAML parser rejects. `examples/github-actions/terraform.yml` uses block
style. Everything else matches the spec.

**D48. UUIDv7 is strictly increasing within a process.** Version listings and GC order by `VERSION_ID`, but plain UUIDv7
sorts randomly within a millisecond, and repeated test runs caught a listing with the wrong newest version. `ids.V7` now
uses `rand_a` as a sequence (RFC 9562 §6.2). Across replicas, order follows the clock, which is safe in practice because
writes to one state are serialised by its lock and are seconds apart.

**D49. e2e clients use separate plugin caches.** Clients run in parallel, and a shared `TF_PLUGIN_CACHE_DIR` let one read a
provider package another was still writing, which failed every time on a cold cache. CI's macOS job hit it first.

**D50. Release tags are checked, and only stable tags move the major tag.** `.github/scripts/release-version.sh` refuses
anything that is not semver. `v0.1.0` moves `v0`, `v1.2.3` moves `v1`, and `v1.0.0-rc.1` moves nothing and is published as a
prerelease. The spec only described `v1`.

**D51. The release image covers linux/amd64 and linux/arm64.** The Dockerfile cross-compiles in its build stage, so arm64
needs no emulation.

**D52. `integration` skips rather than passes without credentials.** It used to show green with every step skipped.

**D53. Docs-only changes skip the long CI jobs, and `ci-ok` and `action-ok` are the required checks.** A workflow-level path
filter leaves required checks pending forever, and a skipped matrix job reports as `e2e`, not `e2e (ubuntu-latest)`. So every
workflow starts, `changed-code.sh` decides, jobs skip themselves, and a gate job that always runs reports one result. The
gate fails on anything but `success` or `skipped`, and if the `changes` job itself did not succeed. The script is local
rather than `dorny/paths-filter`, so it can be tested and needs no pinning, and it runs everything when unsure.

**D54. `action/setup` resolves major tags.** A release only exists under its full version, so `@v1` would have asked for
`/releases/download/v1/` and got a 404. A major tag now resolves to the newest stable release using `git ls-remote`, which
needs no token. The cache key uses the resolved version, so `@v1` picks up new releases. Anything that is not a version,
such as `@main`, fails with a clear message. The action's CI used to pass an explicit version, which is why this was missed.

**D55. A stable release needs a changelog section.** `check-changelog.sh` runs before anything is built or published, so a
forgotten changelog fails the release cleanly. Prereleases skip it, because their contents may still change. A tag is
immutable under the ruleset, so a failed tag means a new version number, not a retry of the same one.

### Open: not verified in this build

- **`github-wif`, the real Actions workflows and CI itself have not run.** They need a push to GitHub, an OIDC service
  user and the `integration` environment. The workflow files were only parsed as YAML, not run. Everything they exercise was tested
  locally with the runner's variables set (scenario 10), and the OIDC fetch against a local HTTP server, but a real
  Snowflake WIF login has not happened.
- **The multi-arch image and the release workflow** have not run. The single-platform image has been built and checked
  in CI, which closes D38.
- **Role isolation** has not run (see M4).

## M8: One-command install

**D56. Terraform owns the Foundation, and `migrate` owns what is inside the schema.** Moving tables into Terraform would
duplicate the schema outside the binary, break version tracking in `SCHEMA_MIGRATIONS`, and leave sidecar users who only
have the binary with no way to upgrade. The cost is two tools with a shared boundary, so grants to `<DB>_SVC` and
`<DB>_READER` are future grants, applied before `migrate`. That also removes the ordering problem that stopped
`bootstrap.sql` running top to bottom. `<DB>_READER` can read any future view, so every view must stay free of raw
state.

**D57. The Foundation's state lives in the backend it helps run (self-hosted).** If a bad apply breaks the warehouse or
roles, the backend can't be used to read the state that would fix them. A separate store, such as S3, would avoid that,
but it brings back the bucket snowstate exists to avoid. The state stays readable in Snowflake tables with admin access,
so the risk is accepted, with `prevent_destroy` on the database and schema and tested recovery procedures in the
runbook.

**D58. `snowstate` orchestrates the real Terraform and never reimplements it.** `install` runs the real binary, and
every step stays documented and runnable by hand, so recovery never depends on the CLI. The Terraform code is embedded
so a binary-only user has it, and is written to a directory the operator keeps, so they can read and commit it.

**D59. `install` uses one user, `SNOWSTATE_SEED`, and a role per step.** The role split limits mistakes, not a stolen
key, because the key holder can use any of its roles. A second user would only help once CI holds a separate state key,
and it adds manual setup, which is what the Seed is meant to minimise. So the user is for Foundation changes only and
never goes on a server.

**D60. Names derive from `database_name` (deviates from §9 and §10 as first written).** Roles, warehouses and users are
account-level, so a unique database alone would leave two Installs sharing `SNOWSTATE_ADMIN`, making an admin of one an
admin of both. A default Install therefore has `TF_STATE_ADMIN`, not `SNOWSTATE_ADMIN`, and the configuration defaults
change to match. This is a breaking change, accepted before v1.0.

**D61. M7 is Deferred (deviates from §14 as first written).** `state_store` still only works in alpha builds, so effort
goes to what exists today. G7, §14.2 and `TestImportBoundaries` stay, because the core staying frontend-neutral is what
keeps the option open, at almost no cost.

**D62. `schema_per_project` and GitHub identity setup leave the shipped SQL (deviates from §5.3 and §8.4.1 as first
written).** `bootstrap.sql` goes away, and neither has a Foundation version yet: the first has a known login failure and
unverified role isolation, the second has never had a real login, and provider 2.22 has no workload identity attribute
on service users. Terraforming them now would encode untested behaviour, so their setup moves to the operator guide,
marked unverified.

**D63. `install` runs `doctor` as `<DB>_SVC`, not `<DB>_ADMIN`.** The admin role owns the tables, so a round trip as
admin would pass even with broken runtime grants, and the fault would first show up during the state move. `<DB>_SVC`
reaches the Seed user through `<DB>_ADMIN`, because a user can activate any role below one it holds.

**D64. Hybrid tables need their own privilege (VERIFY outcome).** On a real account, `migrate` failed with `003001`:
`CREATE TABLE` is not enough, and the role must hold `CREATE HYBRID TABLE` on the schema. Future grants on `TABLES` do
reach hybrid tables: `doctor` wrote and deleted a lock row as `<DB>_SVC` with no other grant on `LOCKS`. A user can
activate a role below one it holds, which confirms D63: of this Install's roles, `SNOWSTATE_SEED` holds only
`<DB>_ADMIN`, and it ran as `<DB>_SVC`. Provider 2.22.1 needed no preview features for any resource.

**D65. The backend block ships disabled, as `backend.tf.disabled` (deviates from §8.5 as first written).** A backend
block in the first apply would point at something that does not exist until `migrate` has run. So it stays out of the
configuration until the state moves, when it is renamed. `-migrate-state` leaves an empty `terraform.tfstate` and a full
`terraform.tfstate.backup`, so both are removed afterwards. Because of this rename, and because the directory also gains
`.terraform.lock.hcl` and `terraform.tfvars`, `install` compares shipped files only, treating `backend.tf` as its
disabled form.
