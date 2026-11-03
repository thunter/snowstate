# Developer Setup: snowstate

What you need on your laptop, and in your accounts, **before** handing `SPEC.md` to Claude Code. Work through it top to bottom, then run the checklist in §6. Everything here assumes **macOS with Homebrew**.

| | |
|---|---|
| **Audience** | Todd (repo owner) and anyone joining the build |
| **Last updated** | 31 October 2026 |
| **Time needed** | ~1–2 hours, plus waiting on your Snowflake admin for §3.2 |

---

## 1. Accounts and access

| Account | Needed for | Required by milestone | Who sets it up |
|---|---|---|---|
| **Claude** with Claude Code access (Pro, Max, Team or Enterprise, or an Anthropic Console API account) | Running the build agent | All | You |
| **GitHub** account + a new repository (e.g. `snowstate`) | Source control, CI, releases, the setup action, the OIDC test job | M0 onward. Actions is needed for M6.5. | You |
| **Snowflake account** with a dev database and role (§3) | Integration tests against real Snowflake | M3 onward. M0–M2 run without Snowflake. | **Your Snowflake admin** (needs ACCOUNTADMIN or SECURITYADMIN + SYSADMIN) |
| **Snowflake service user for GitHub OIDC** (§3.4) | The `github-wif` CI job | M6.5 | Snowflake admin |
| *(Optional)* **Snowflake Postgres** privilege `CREATE POSTGRES INSTANCE` | Only if you want to trial Option 1 from the review first | — | Snowflake admin |
| *(Optional)* Container registry (GHCR comes with GitHub) | Publishing the image | M6 | You |
| *(Optional)* Private Terraform registry or a filesystem mirror | Distributing the experimental provider | M7 | You |

> **Ask your Snowflake admin early.** §3.2 is the one step you can't do yourself, and nothing from M3 onward can be verified without it.

---

## 2. Tools to install

### 2.1 Install commands

```bash
# Homebrew (skip if installed): https://brew.sh
xcode-select --install                  # git, make, clang (if not already present)

brew install go                         # Go 1.26.x (spec needs ≥ 1.24; plugin framework needs ≥ 1.25)
brew install git gh jq golangci-lint
brew install tfenv                      # Terraform version manager (do NOT also `brew install terraform`)
brew install opentofu                   # OpenTofu (tested alongside Terraform)
brew install pipx && pipx ensurepath
pipx install snowflake-cli              # the `snow` CLI, for bootstrap SQL and checks
go install golang.org/x/vuln/cmd/govulncheck@latest

# Container runtime: pick ONE (needed for M6 image builds only)
brew install --cask orbstack            # or Docker Desktop (check its licence terms for company use)

# Optional
brew install cosign                     # if you enable release signing (M6.5)
```

Terraform versions the test suite pins (SPEC §12.2, §14.2):

```bash
tfenv install 1.5.7                     # oldest supported
tfenv install latest                    # latest stable 1.x (1.17.1 at time of writing)
tfenv install 1.17.0-alpha20260827      # ONLY for M7 (state_store experiment)
tfenv use latest
```

**Claude Code.** Install it using the method in the [Claude Code docs](https://docs.claude.com/en/docs/claude-code/overview). At the time of writing that's a native installer script or `brew install --cask claude-code`. Then run `claude` once and sign in.

### 2.2 Why each tool is needed

| Tool | Used for |
|---|---|
| Go | Building `snowstate` and the M7 provider |
| git, gh | Repo, pull requests, setting CI secrets and variables |
| golangci-lint, govulncheck | `make lint` and the security scan the agent runs before each commit |
| tfenv / Terraform | E2E tests drive real Terraform binaries (several versions) |
| OpenTofu | E2E tests also run against `tofu` |
| `snow` CLI | Running the Snowflake setup SQL, checking connectivity, poking at tables |
| openssl (built into macOS) | Generating your Snowflake key pair |
| jq | Reading admin API and JSON log output |
| OrbStack / Docker | Building and running the container image (M6) |
| cosign | Optional release signing |

---

## 3. Snowflake setup

### 3.1 Check what your account supports (you can run these yourself)

Run them in Snowsight or with `snow sql -q "..."` once connected:

```sql
SELECT CURRENT_ACCOUNT(), CURRENT_REGION(), CURRENT_VERSION();
-- Hybrid tables are GA on AWS/Azure, preview on GCP. The region tells you which cloud you're on.

SELECT SYSTEM$BEHAVIOR_CHANGE_BUNDLE_STATUS('2025_03');
-- Should be ENABLED for the 128 MB VARCHAR / 64 MB BINARY limits the spec relies on.

SHOW PARAMETERS LIKE 'NETWORK_POLICY' IN ACCOUNT;
-- If a network policy is set, your laptop's IP (and later GitHub runners) must be allowed.
```

Write the answers down. They resolve SPEC §15 assumptions A1 and A10.

### 3.2 Ask your admin to create the dev environment

This is a **dev/test** footprint. The production `bootstrap.sql` is something the agent writes in M3. Integration tests create and drop their own throwaway schemas (`BACKEND_IT_<random>`), which is why the dev role needs `CREATE SCHEMA`.

```sql
USE ROLE SYSADMIN;
CREATE WAREHOUSE IF NOT EXISTS SNOWSTATE_DEV_WH
  WAREHOUSE_SIZE = XSMALL AUTO_SUSPEND = 60 AUTO_RESUME = TRUE INITIALLY_SUSPENDED = TRUE;
CREATE DATABASE IF NOT EXISTS TF_STATE_DEV;

USE ROLE SECURITYADMIN;
CREATE ROLE IF NOT EXISTS SNOWSTATE_DEV;
GRANT USAGE, OPERATE ON WAREHOUSE SNOWSTATE_DEV_WH TO ROLE SNOWSTATE_DEV;
GRANT USAGE, CREATE SCHEMA ON DATABASE TF_STATE_DEV TO ROLE SNOWSTATE_DEV;
GRANT ROLE SNOWSTATE_DEV TO USER <YOUR_SNOWFLAKE_USER>;

-- Needed later for schema_per_project / role tests (SPEC §5.3, §12.3):
GRANT CREATE ROLE ON ACCOUNT TO ROLE SNOWSTATE_DEV;          -- or have the admin pre-create 2 test roles instead

-- Cost guard-rail
USE ROLE ACCOUNTADMIN;
CREATE RESOURCE MONITOR IF NOT EXISTS SNOWSTATE_DEV_RM
  WITH CREDIT_QUOTA = 20 FREQUENCY = MONTHLY START_TIMESTAMP = IMMEDIATELY
  TRIGGERS ON 80 PERCENT DO NOTIFY ON 100 PERCENT DO SUSPEND;
ALTER WAREHOUSE SNOWSTATE_DEV_WH SET RESOURCE_MONITOR = SNOWSTATE_DEV_RM;
```

> If your admin won't grant `CREATE ROLE`, ask them to pre-create two roles (`SNOWSTATE_IT_A`, `SNOWSTATE_IT_B`) granted to `SNOWSTATE_DEV`. Then tell the agent, by adding a line to `docs/decisions.md`, to use those for the isolation tests.

Then check that hybrid tables actually work for this role:

```sql
USE ROLE SNOWSTATE_DEV; USE WAREHOUSE SNOWSTATE_DEV_WH;
CREATE SCHEMA TF_STATE_DEV.SMOKE;
CREATE HYBRID TABLE TF_STATE_DEV.SMOKE.T (ID VARCHAR PRIMARY KEY, V VARCHAR);
INSERT INTO TF_STATE_DEV.SMOKE.T VALUES ('a','1');
INSERT INTO TF_STATE_DEV.SMOKE.T VALUES ('a','2');   -- MUST fail with a duplicate key error
DROP SCHEMA TF_STATE_DEV.SMOKE;
```

If the second insert **doesn't** fail, or `CREATE HYBRID TABLE` errors, stop and tell the agent. The spec's default locking mode depends on this.

### 3.3 Key-pair auth for your own user (for local integration tests)

```bash
mkdir -p ~/.snowflake/keys && cd ~/.snowflake/keys
openssl genrsa 2048 | openssl pkcs8 -topk8 -inform PEM -out snowstate_dev.p8 -nocrypt
openssl rsa -in snowstate_dev.p8 -pubout -out snowstate_dev.pub
chmod 600 snowstate_dev.p8
# Print the public key body without header/footer lines:
grep -v "PUBLIC KEY" snowstate_dev.pub | tr -d '\n'; echo
```

Register the public key (you can usually do this for your own user, otherwise ask the admin):

```sql
ALTER USER <YOUR_SNOWFLAKE_USER> SET RSA_PUBLIC_KEY = '<paste the printed key>';
```

Create a `snow` connection and test it:

```bash
snow connection add --connection-name snowstate_dev \
  --account <orgname-accountname> --user <YOUR_SNOWFLAKE_USER> \
  --authenticator SNOWFLAKE_JWT --private-key-file ~/.snowflake/keys/snowstate_dev.p8 \
  --role SNOWSTATE_DEV --warehouse SNOWSTATE_DEV_WH --database TF_STATE_DEV --no-interactive
snow connection test -c snowstate_dev
```

> Want an encrypted key? Drop `-nocrypt`, and set the passphrase in `SNOWSTATE_IT_PRIVATE_KEY_PASSPHRASE` (below).

### 3.4 GitHub OIDC user (needed for M6.5, can wait)

Once the GitHub repo exists, ask your admin for a service user that trusts GitHub's OIDC issuer for **this repo's** `integration` environment:

```sql
CREATE USER GH_SNOWSTATE_CI
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_DEV
  DEFAULT_WAREHOUSE = SNOWSTATE_DEV_WH
  WORKLOAD_IDENTITY = (
    TYPE = OIDC
    ISSUER = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<github-owner>/snowstate:environment:integration');
GRANT ROLE SNOWSTATE_DEV TO USER GH_SNOWSTATE_CI;
```

If the account has a network policy, GitHub-hosted runners will be blocked. See SPEC §8.4.2 for the options, and decide which one before M6.5.

---

## 4. Local configuration

### 4.1 Integration-test environment variables

The spec gates real-Snowflake tests on `SNOWSTATE_IT_*` variables. Use exactly these names (they're also listed in `CLAUDE.md` for the agent). Put them in `~/.config/snowstate/it.env`, **outside the repo**:

```bash
export SNOWSTATE_IT_ACCOUNT="<orgname-accountname>"
export SNOWSTATE_IT_USER="<YOUR_SNOWFLAKE_USER>"
export SNOWSTATE_IT_ROLE="SNOWSTATE_DEV"
export SNOWSTATE_IT_WAREHOUSE="SNOWSTATE_DEV_WH"
export SNOWSTATE_IT_DATABASE="TF_STATE_DEV"
export SNOWSTATE_IT_AUTH="keypair"
export SNOWSTATE_IT_PRIVATE_KEY_PATH="$HOME/.snowflake/keys/snowstate_dev.p8"
# export SNOWSTATE_IT_PRIVATE_KEY_PASSPHRASE="..."   # only if the key is encrypted
# export SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH="$HOME/.snowflake/keys/snowstate_seed.p8"   # only for the install test
```

The install test needs a Seed on the account, and a `SNOWSTATE_IT_ROLE` above the Seed role, such as `SYSADMIN`, so it can
remove the test Install afterwards. It skips itself without `SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH`.

Load them before starting Claude Code: `source ~/.config/snowstate/it.env`.

### 4.2 GitHub repository

```bash
gh auth login
cd ~/projects/sf_tf_backend
git init -b main
printf '.env*\n*.p8\n*.pem\n*.tfstate*\ntfplan\n.terraform/\ndist/\n' > .gitignore
git add . && git commit -m "Spec, review docs and dev setup"
gh repo create <github-owner>/snowstate --private --source . --push
```

CI configuration (can be done at M6.5). Store **non-secret** values as variables:

```bash
gh variable set SNOWFLAKE_ACCOUNT --body "<orgname-accountname>"
gh api -X PUT repos/<github-owner>/snowstate/environments/integration   # environment the OIDC user trusts
```

If you use the key-pair fallback instead of OIDC, add the key as a secret: `gh secret set SNOWFLAKE_PRIVATE_KEY < ~/.snowflake/keys/<ci-key>.p8`. Use a separate CI key, not your personal one.

### 4.3 Claude Code permissions (optional, saves approval prompts)

Create `.claude/settings.json` in the repo so the agent can run the build tools without asking every time:

```json
{
  "permissions": {
    "allow": [
      "Bash(go:*)", "Bash(make:*)", "Bash(golangci-lint:*)", "Bash(govulncheck:*)",
      "Bash(terraform:*)", "Bash(tofu:*)", "Bash(tfenv:*)", "Bash(git:*)",
      "Bash(docker:*)", "Bash(snow sql:*)", "Bash(jq:*)"
    ],
    "deny": ["Bash(snow sql -q \"DROP DATABASE*)", "Read(~/.snowflake/keys/**)"]
  }
}
```

Check the exact settings format against the current Claude Code docs. The point is to allow the build toolchain and keep the agent away from your private key files. The tests read the key via the env var path, but the agent never needs to open it.

---

## 5. Cost and safety notes

- The dev warehouse auto-suspends after 60 s and is capped by the resource monitor at 20 credits a month. Normal development should use a small fraction of that.
- Integration tests create and drop `BACKEND_IT_*` schemas. If a run crashes, leftovers can be removed with `snow sql -q "SHOW SCHEMAS LIKE 'BACKEND_IT_%' IN DATABASE TF_STATE_DEV"` and a drop. The spec doesn't ask the agent to build a cleanup command, so add one to `docs/decisions.md` if you want it.
- Never commit `*.p8` keys or `it.env`. The `.gitignore` above covers keys, state files and plan files.
- Use only `TF_STATE_DEV` during the build. A production database comes later via the agent's `bootstrap.sql`.

---

## 6. Ready-to-hand-over checklist

Run this. Every line should print OK:

```bash
source ~/.config/snowstate/it.env
for c in go git gh jq golangci-lint govulncheck tfenv terraform tofu snow claude; do
  command -v $c >/dev/null && echo "OK  $c" || echo "MISSING  $c"; done
go version | grep -Eq 'go1\.(2[4-9]|[3-9][0-9])' && echo "OK  go>=1.24" || echo "FAIL go version"
tfenv list | grep -q 1.5.7 && echo "OK  terraform 1.5.7 installed" || echo "MISSING terraform 1.5.7"
snow connection test -c snowstate_dev >/dev/null && echo "OK  snowflake connection" || echo "FAIL snowflake connection"
snow sql -c snowstate_dev -q "SELECT 1" >/dev/null && echo "OK  warehouse runs queries" || echo "FAIL warehouse"
gh auth status >/dev/null 2>&1 && echo "OK  gh authenticated" || echo "FAIL gh auth"
test -f SPEC.md && test -f CLAUDE.md && echo "OK  spec in repo" || echo "FAIL spec missing"
```

Also tick these by hand:

- [ ] The hybrid table smoke test in §3.2 failed on the duplicate insert (as it should)
- [ ] Cloud/region, behaviour bundle and network policy results written down (resolves SPEC §15 A1/A10)
- [ ] Repo pushed to GitHub
- [ ] *(M6.5)* OIDC user created, `integration` environment exists, network access decided

## 7. Handing over to Claude Code

```bash
cd ~/projects/sf_tf_backend
source ~/.config/snowstate/it.env
claude
```

Then give it a prompt like this:

> Read `CLAUDE.md`, `SPEC.md` and the docs in `docs/`. Build snowstate following the milestones in SPEC §14, starting with M0. Snowflake integration credentials are in the `SNOWSTATE_IT_*` environment variables. Commit after each milestone.

Before you start, also pass on any answers you have for SPEC §15 (cloud, network policy, Terraform vs OpenTofu, runner type). Add them to `docs/decisions.md` so the agent doesn't fall back to the defaults.

---

## Sources

- [Snowflake CLI: `snow connection add`](https://docs.snowflake.com/developer-guide/snowflake-cli/command-reference/connection-commands/add-connection)
- [Snowflake hybrid tables](https://docs.snowflake.com/en/user-guide/tables-hybrid)
- [Snowflake workload identity federation](https://docs.snowflake.com/en/user-guide/workload-identity-federation)
- [Snowflake behaviour bundle 2025_03 size limits](https://docs.snowflake.com/release-notes/bcr-bundles/2025_03/bcr-1942)
- [Claude Code overview](https://docs.claude.com/en/docs/claude-code/overview)
