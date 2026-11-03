# snowstate

A Terraform/OpenTofu `http` state backend that stores state in Snowflake.

- **`SPEC.md` is the source of truth.** Read it fully before starting, then build milestone by milestone (§14). Don't skip acceptance criteria.
- Background and the options considered are in `docs/initial-review.md`. Terraform's experimental `state_store` is covered in `docs/pluggable-state-storage.md`.
- Keep the core in `pkg/` frontend-neutral (SPEC §3, G7) so the M7 provider stays possible. M7 is Deferred: revisit it if `state_store` reaches a stable Terraform release.
- Local tooling and the Snowflake dev environment are described in `dev_setup.md`. Real-Snowflake integration tests read **`SNOWSTATE_IT_ACCOUNT`, `SNOWSTATE_IT_USER`, `SNOWSTATE_IT_ROLE`, `SNOWSTATE_IT_WAREHOUSE`, `SNOWSTATE_IT_DATABASE`, `SNOWSTATE_IT_AUTH`, `SNOWSTATE_IT_PRIVATE_KEY_PATH`** (and optionally `SNOWSTATE_IT_PRIVATE_KEY_PASSPHRASE`, and `SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH` for the install test). Skip those tests when the variables are unset. Never read or print the private key file contents.
- Record every VERIFY outcome and every deviation from the spec in `docs/decisions.md`.
- Don't stop to ask questions. Choose the safest reasonable behaviour, document it, and continue.
- Never log state bodies, tokens, passwords or `Authorization` headers.
- Before each commit, run `make lint test` (and `make e2e` from M1 onward).

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues on `thunter/snowstate`, using the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default labels are used as they are: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human` and `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root, with existing decisions in `docs/decisions.md`. See `docs/agents/domain.md`.
