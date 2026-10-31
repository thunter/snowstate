# Tutorial: create a Snowflake database with Terraform, with state in Snowflake

In this tutorial you use the official Snowflake Terraform provider to create one database, and you keep the Terraform
state in Snowflake with `snowstate`. There is no server to run: `snowstate exec` runs a small local backend for the length
of each Terraform command.

It takes about 20 minutes, and you create only tutorial objects, all removed in step 11.

| Object | Created by | Purpose |
|---|---|---|
| `TF_TUTORIAL_USER`, `TF_TUTORIAL_ROLE` | You, step 3 | The identity Terraform and snowstate use |
| `TF_TUTORIAL_WH` | You, step 3 | The warehouse for both |
| `TF_TUTORIAL_STATE.BACKEND` | You and `snowstate migrate` | Where the Terraform state lives |
| `TUTORIAL_DB` | Terraform | The database this tutorial is about |

## Prerequisites

- A Snowflake account on AWS or Azure. snowstate's locking needs hybrid tables, which are in preview on GCP, and which
  some trial accounts do not have.
- A login that can use `ACCOUNTADMIN`, for step 3 only.
- Go 1.27 or later, Terraform 1.5 or later, and `openssl`.
- This repository, cloned.

## 1. Build snowstate

From the root of this repository:

```bash
make build
export PATH="$PWD/bin:$PATH"
snowstate version
```

## 2. Create a key pair

Terraform and snowstate both log in as a service user with this key:

```bash
mkdir -p ~/.snowflake/keys
openssl genrsa 2048 | openssl pkcs8 -topk8 -inform PEM -out ~/.snowflake/keys/tf_tutorial.p8 -nocrypt
openssl rsa -in ~/.snowflake/keys/tf_tutorial.p8 -pubout -out ~/.snowflake/keys/tf_tutorial.pub
chmod 600 ~/.snowflake/keys/tf_tutorial.p8
```

Print the public key as one line, which you need in the next step:

```bash
grep -v "PUBLIC KEY" ~/.snowflake/keys/tf_tutorial.pub | tr -d '\n'; echo
```

## 3. Set up Snowflake

Paste this into a Snowsight worksheet, replace `<public key>` with the line you just printed, and run all of it.

```sql
USE ROLE ACCOUNTADMIN;

CREATE WAREHOUSE IF NOT EXISTS TF_TUTORIAL_WH
  WAREHOUSE_SIZE = XSMALL AUTO_SUSPEND = 60 AUTO_RESUME = TRUE INITIALLY_SUSPENDED = TRUE;

CREATE DATABASE IF NOT EXISTS TF_TUTORIAL_STATE;

CREATE ROLE IF NOT EXISTS TF_TUTORIAL_ROLE;
GRANT USAGE ON WAREHOUSE TF_TUTORIAL_WH TO ROLE TF_TUTORIAL_ROLE;
GRANT USAGE, CREATE SCHEMA ON DATABASE TF_TUTORIAL_STATE TO ROLE TF_TUTORIAL_ROLE;  -- for the state tables
GRANT CREATE DATABASE ON ACCOUNT TO ROLE TF_TUTORIAL_ROLE;                          -- for the provider

CREATE USER IF NOT EXISTS TF_TUTORIAL_USER
  TYPE = SERVICE
  DEFAULT_ROLE = TF_TUTORIAL_ROLE
  DEFAULT_WAREHOUSE = TF_TUTORIAL_WH
  RSA_PUBLIC_KEY = '<public key>';
GRANT ROLE TF_TUTORIAL_ROLE TO USER TF_TUTORIAL_USER;

-- The two parts of your account identifier, for the next step.
SELECT CURRENT_ORGANIZATION_NAME() AS ORGANIZATION_NAME, CURRENT_ACCOUNT_NAME() AS ACCOUNT_NAME;
```

One role is used for everything here to keep the tutorial short. A real setup separates them: see
`deploy/snowflake/bootstrap.sql`.

## 4. Set your environment

Use the two values from the last query. Keep this terminal open, because every later step needs these variables.

```bash
export SF_ORG=<ORGANIZATION_NAME>
export SF_ACCOUNT=<ACCOUNT_NAME>

# Where the state lives, and how snowstate logs in
export SNOWSTATE_SNOWFLAKE__ACCOUNT="$SF_ORG-$SF_ACCOUNT"
export SNOWSTATE_SNOWFLAKE__USER=TF_TUTORIAL_USER
export SNOWSTATE_SNOWFLAKE__ROLE=TF_TUTORIAL_ROLE
export SNOWSTATE_SNOWFLAKE__WAREHOUSE=TF_TUTORIAL_WH
export SNOWSTATE_SNOWFLAKE__DATABASE=TF_TUTORIAL_STATE
export SNOWSTATE_SNOWFLAKE__SCHEMA=BACKEND
export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH="$HOME/.snowflake/keys/tf_tutorial.p8"

# Which state this module uses
export SNOWSTATE_KEY=tutorial/database/default
```

`SNOWSTATE_KEY` names the state, in the form `<project>/<name>/<workspace>`. These variables configure snowstate only.
The Terraform provider gets its settings from a vars file in step 6.

## 5. Create the state tables

```bash
snowstate migrate
snowstate doctor
```

`migrate` creates the `BACKEND` schema and its tables. `doctor` should print `PASS` on every line, finishing with
`PASS  privileges`.

## 6. Write the Terraform

Make a new directory outside this repository:

```bash
mkdir -p ~/snowstate-tutorial && cd ~/snowstate-tutorial
```

You need two files. Create `main.tf` with this content:

```hcl
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    snowflake = {
      source  = "snowflakedb/snowflake"
      version = "~> 2.22"
    }
  }

  # Empty on purpose. `snowstate exec` supplies the address at run time.
  backend "http" {}
}

variable "organization_name" {
  type        = string
  description = "Snowflake organization name"
}

variable "account_name" {
  type        = string
  description = "Snowflake account name"
}

variable "user" {
  type        = string
  description = "Snowflake user the provider logs in as"
}

variable "role" {
  type        = string
  description = "Snowflake role the provider uses"
}

variable "warehouse" {
  type        = string
  description = "Snowflake warehouse the provider uses"
}

variable "private_key_path" {
  type        = string
  description = "Path to the PEM private key for the user"
}

variable "database_name" {
  type        = string
  description = "Name of the database to create"
}

variable "comment" {
  type        = string
  description = "Comment on the database"
}

provider "snowflake" {
  organization_name = var.organization_name
  account_name      = var.account_name
  user              = var.user
  role              = var.role
  warehouse         = var.warehouse
  authenticator     = "SNOWFLAKE_JWT"
  private_key       = file(pathexpand(var.private_key_path))
}

resource "snowflake_database" "tutorial" {
  name    = var.database_name
  comment = var.comment
}

output "database_name" {
  value = snowflake_database.tutorial.name
}
```

Then create `tutorial.tfvars`, using the same two values you exported as `SF_ORG` and `SF_ACCOUNT`:

```hcl
organization_name = "<ORGANIZATION_NAME>"
account_name      = "<ACCOUNT_NAME>"
user              = "TF_TUTORIAL_USER"
role              = "TF_TUTORIAL_ROLE"
warehouse         = "TF_TUTORIAL_WH"
private_key_path  = "~/.snowflake/keys/tf_tutorial.p8"
database_name     = "TUTORIAL_DB"
comment           = "Created by Terraform. State is stored in Snowflake."
```

This file holds a path to the key, not the key itself. Terraform only loads `terraform.tfvars` and `*.auto.tfvars` by
itself, so `tutorial.tfvars` is passed by name with `-var-file`.

## 7. Create the database

Put `snowstate exec --` in front of every Terraform command that touches state. `plan`, `apply` and `destroy` also need
`-var-file`.

```bash
snowstate exec -- terraform init
snowstate exec -- terraform plan -var-file=tutorial.tfvars
snowstate exec -- terraform apply -var-file=tutorial.tfvars
```

The plan says `1 to add`. Type `yes` when `apply` asks. It finishes with:

```text
Apply complete! Resources: 1 added, 0 changed, 0 destroyed.

Outputs:

database_name = "TUTORIAL_DB"
```

## 8. Check the result

Terraform can see its state:

```bash
snowstate exec -- terraform state list
```

```text
snowflake_database.tutorial
```

So can snowstate, which also shows that no local state file was written:

```bash
snowstate state list
snowstate state versions tutorial/database/default
ls
```

`ls` shows `main.tf` but no `terraform.tfstate`.

To see the database itself, ask Terraform, which reads it through the provider:

```bash
snowstate exec -- terraform show
```

`TF_TUTORIAL_ROLE` owns `TUTORIAL_DB`, so your own role may not list it in Snowsight unless it holds that role.

## 9. Make a change

In `tutorial.tfvars`, change the last line to:

```hcl
comment           = "Changed by the tutorial"
```

Then apply it:

```bash
snowstate exec -- terraform apply -var-file=tutorial.tfvars
```

This says `1 to change`. Every write is an immutable version, so the state now has more than one:

```bash
snowstate state versions tutorial/database/default
```

## 10. Destroy the database

```bash
snowstate exec -- terraform destroy -var-file=tutorial.tfvars
```

Type `yes`. Terraform drops `TUTORIAL_DB`, and it finishes with `Destroy complete! Resources: 1 destroyed.`

## 11. Clean up

Run this in a Snowsight worksheet. It removes everything else, including the state and its history.

```sql
USE ROLE ACCOUNTADMIN;
DROP DATABASE IF EXISTS TF_TUTORIAL_STATE;
DROP USER IF EXISTS TF_TUTORIAL_USER;
DROP ROLE IF EXISTS TF_TUTORIAL_ROLE;
DROP WAREHOUSE IF EXISTS TF_TUTORIAL_WH;
```

Then remove the local files:

```bash
rm -rf ~/snowstate-tutorial
rm -f ~/.snowflake/keys/tf_tutorial.p8 ~/.snowflake/keys/tf_tutorial.pub
```

## If something goes wrong

| Symptom | Cause | Fix |
|---|---|---|
| `no state key: pass --key or set SNOWSTATE_KEY` | The variables from step 4 are not set in this terminal | Run step 4 again |
| `migrate` says hybrid tables are not available | The account or region does not have them | Use an AWS or Azure account |
| Terraform asks for `var.organization_name`, or reports `No value for required variable` | `-var-file` is missing | Add `-var-file=tutorial.tfvars` to the command |
| `Invalid function argument` on `file(...)` | `private_key_path` in `tutorial.tfvars` is wrong | Check the path from step 2 |
| `Backend initialization required` | `.terraform` was deleted, or `init` was not run | `snowstate exec -- terraform init` |
| `insufficient privileges` on `CREATE DATABASE` | The grant from step 3 is missing | Rerun step 3 |
| `Error acquiring the state lock` | Another run holds the lock, or one crashed | `snowstate lock list`, then `snowstate lock release tutorial/database/default` once you know nothing is running |

If you press Ctrl-C during an apply, Terraform stops cleanly and releases the lock. If a run is killed outright, the lock
stays held on purpose, and snowstate prints the command to release it.

## Next steps

- `docs/operator-guide.md` covers a shared server, tokens, retention and production roles.
- `docs/github-actions.md` runs the same flow in CI without a stored secret.
- `docs/runbook.md` covers stuck locks and restoring a bad state.
