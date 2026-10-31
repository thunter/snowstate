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
- gosnowflake stays on **v1.19.1**, the newest v1.x, as §2 requires. A v2 line exists (v2.3.0) and is not used.
- `koanf` loads flag values with `k.Set` rather than through the `posflag` provider. That lets each command expose only
  the flags it needs, and the precedence stays flags > env > file.
- Only `SNOWSTATE_*` variables containing `__` are config keys, so `SNOWSTATE_KEY`, `SNOWSTATE_TOKEN_PEPPER` and
  `SNOWSTATE_IT_*` cannot leak into the config tree.
