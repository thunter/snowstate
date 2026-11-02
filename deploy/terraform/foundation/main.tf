provider "snowflake" {
  organization_name = var.organization_name
  account_name      = var.account_name
  user              = local.seed
  role              = local.seed
  authenticator     = "SNOWFLAKE_JWT"
  private_key       = file(pathexpand(var.private_key_path))
}

locals {
  # The Seed uses this name for both its role and its user.
  seed = "SNOWSTATE_SEED"

  admin_role   = coalesce(var.admin_role_name, "${var.database_name}_ADMIN")
  runtime_role = coalesce(var.runtime_role_name, "${var.database_name}_SVC")
  reader_role  = coalesce(var.reader_role_name, "${var.database_name}_READER")
  warehouse    = coalesce(var.warehouse_name, "${var.database_name}_WH")
  service_user = coalesce(var.service_user_name, "${var.database_name}_SERVICE")

  create_service_user = var.service_public_key != null && var.service_public_key != ""

  # Keys stay fixed, so renaming a role does not recreate its grants. Every role
  # needs USAGE to reach the schema, and only the admin role runs DDL.
  roles = {
    admin = {
      name              = snowflake_account_role.admin.name
      schema_privileges = ["USAGE", "CREATE TABLE", "CREATE HYBRID TABLE", "CREATE STAGE", "CREATE VIEW"]
    }
    runtime = {
      name              = snowflake_account_role.runtime.name
      schema_privileges = ["USAGE"]
    }
    reader = {
      name              = snowflake_account_role.reader.name
      schema_privileges = ["USAGE"]
    }
  }
}

resource "snowflake_warehouse" "this" {
  name                = local.warehouse
  warehouse_size      = "XSMALL"
  auto_suspend        = 60
  auto_resume         = true
  initially_suspended = true
}

resource "snowflake_database" "this" {
  name = var.database_name

  lifecycle {
    prevent_destroy = true
  }
}

resource "snowflake_schema" "this" {
  database            = snowflake_database.this.name
  name                = var.schema_name
  with_managed_access = true

  lifecycle {
    prevent_destroy = true
  }
}

resource "snowflake_account_role" "admin" {
  name    = local.admin_role
  comment = "Owns the objects inside ${var.database_name}.${var.schema_name} and runs migrate"
}

resource "snowflake_account_role" "runtime" {
  name    = local.runtime_role
  comment = "Runtime role for ${var.database_name}. No DDL"
}

resource "snowflake_account_role" "reader" {
  name    = local.reader_role
  comment = "Inventory view only, no raw state"
}

# The Seed user reaches the runtime role through the admin role.
resource "snowflake_grant_account_role" "runtime_to_admin" {
  role_name        = snowflake_account_role.runtime.name
  parent_role_name = snowflake_account_role.admin.name
}

resource "snowflake_grant_account_role" "admin_to_seed_user" {
  role_name = snowflake_account_role.admin.name
  user_name = local.seed
}

resource "snowflake_grant_privileges_to_account_role" "warehouse" {
  for_each          = local.roles
  account_role_name = each.value.name
  privileges        = ["USAGE"]

  on_account_object {
    object_type = "WAREHOUSE"
    object_name = snowflake_warehouse.this.name
  }
}

resource "snowflake_grant_privileges_to_account_role" "database" {
  for_each          = local.roles
  account_role_name = each.value.name
  privileges        = ["USAGE"]

  on_account_object {
    object_type = "DATABASE"
    object_name = snowflake_database.this.name
  }
}

resource "snowflake_grant_privileges_to_account_role" "schema" {
  for_each          = local.roles
  account_role_name = each.value.name
  privileges        = each.value.schema_privileges

  on_schema {
    schema_name = snowflake_schema.this.fully_qualified_name
  }
}

# Future grants apply before migrate, so nothing here names an object that does
# not exist yet.
resource "snowflake_grant_privileges_to_account_role" "runtime_tables" {
  account_role_name = snowflake_account_role.runtime.name
  privileges        = ["SELECT", "INSERT", "UPDATE", "DELETE"]

  on_schema_object {
    future {
      object_type_plural = "TABLES"
      in_schema          = snowflake_schema.this.fully_qualified_name
    }
  }
}

resource "snowflake_grant_privileges_to_account_role" "runtime_stages" {
  account_role_name = snowflake_account_role.runtime.name
  privileges        = ["READ", "WRITE"]

  on_schema_object {
    future {
      object_type_plural = "STAGES"
      in_schema          = snowflake_schema.this.fully_qualified_name
    }
  }
}

# Every view in this schema must stay free of raw state (D56).
resource "snowflake_grant_privileges_to_account_role" "reader_views" {
  account_role_name = snowflake_account_role.reader.name
  privileges        = ["SELECT"]

  on_schema_object {
    future {
      object_type_plural = "VIEWS"
      in_schema          = snowflake_schema.this.fully_qualified_name
    }
  }
}

resource "snowflake_service_user" "server" {
  count             = local.create_service_user ? 1 : 0
  name              = local.service_user
  default_role      = snowflake_account_role.runtime.name
  default_warehouse = snowflake_warehouse.this.name
  rsa_public_key    = var.service_public_key
}

resource "snowflake_grant_account_role" "runtime_to_server" {
  count     = local.create_service_user ? 1 : 0
  role_name = snowflake_account_role.runtime.name
  user_name = snowflake_service_user.server[0].name
}
