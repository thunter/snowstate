# Foundation

The Foundation is the account-level part of an Install: its warehouse, database, schema, roles and grants. `snowstate migrate`
owns everything inside the schema.

`snowstate install` runs all of these for you. These are the manual steps, so you can run or finish an Install by hand. Run `terraform` or `tofu`, whichever you use.

**1. Seed, once per account.** Generate a key pair for `SNOWSTATE_SEED`, paste the public key body into
`deploy/snowflake/seed.sql`, and run it as `ACCOUNTADMIN`. New Installs skip this.

**2. Apply with local state.** Set the variables in `terraform.tfvars`:

```hcl
organization_name = "myorg"
account_name      = "myaccount"
private_key_path  = "~/.snowflake/keys/snowstate_seed.p8"
database_name     = "TF_STATE"
```

```bash
terraform init
terraform apply
```

Account-level names derive from `database_name`, so a new name gives you a separate Install. Set `service_public_key` if you
will run `snowstate serve`. Never generate a private key in Terraform, because it would end up in state.

**3. Set up the schema.** Load the settings, then run `migrate` as the admin role:

```bash
eval "$(terraform output -raw snowstate_env)"
eval "$(terraform output -raw migrate_command)"
```

**4. Check the runtime role.** The settings already use it, and this proves its grants before anything depends on them:

```bash
snowstate doctor
```

**5. Move the state into snowstate.**

```bash
mv backend.tf.disabled backend.tf
snowstate exec -- terraform init -migrate-state
snowstate exec -- terraform plan   # should show no changes
```

**6. Remove the local copies.**

```bash
rm terraform.tfstate terraform.tfstate.backup
```

From now on, run every Foundation change through `snowstate exec` with the same settings. The provider logs in with the Seed
role, and snowstate uses the runtime role.
