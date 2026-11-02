output "database" {
  value = snowflake_database.this.name
}

output "schema" {
  value = snowflake_schema.this.name
}

output "warehouse" {
  value = snowflake_warehouse.this.name
}

output "admin_role" {
  value = snowflake_account_role.admin.name
}

output "runtime_role" {
  value = snowflake_account_role.runtime.name
}

output "reader_role" {
  value = snowflake_account_role.reader.name
}

output "service_user" {
  value = one(snowflake_service_user.server[*].name)
}

output "snowstate_env" {
  description = "Settings for snowstate commands, using the runtime role. Load with eval"
  value       = <<-EOT
    export SNOWSTATE_SNOWFLAKE__ACCOUNT=${var.organization_name}-${var.account_name}
    export SNOWSTATE_SNOWFLAKE__USER=${local.seed}
    export SNOWSTATE_SNOWFLAKE__ROLE=${snowflake_account_role.runtime.name}
    export SNOWSTATE_SNOWFLAKE__WAREHOUSE=${snowflake_warehouse.this.name}
    export SNOWSTATE_SNOWFLAKE__DATABASE=${snowflake_database.this.name}
    export SNOWSTATE_SNOWFLAKE__SCHEMA=${snowflake_schema.this.name}
    export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH=${var.private_key_path}
    export SNOWSTATE_KEY=snowstate/foundation/default
  EOT
}

output "migrate_command" {
  description = "Runs migrate as the admin role. Load snowstate_env first"
  value       = "SNOWSTATE_SNOWFLAKE__ROLE=${snowflake_account_role.admin.name} snowstate migrate"
}
