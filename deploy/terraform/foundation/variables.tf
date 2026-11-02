variable "organization_name" {
  type        = string
  description = "Snowflake organization name"
}

variable "account_name" {
  type        = string
  description = "Snowflake account name"
}

variable "private_key_path" {
  type        = string
  description = "Path to the PEM private key for the Seed user"
}

variable "database_name" {
  type        = string
  description = "Database for this Install. Account-level names derive from it, so it must be unique in the account"

  validation {
    condition     = can(regex("^[A-Z][A-Z0-9_]*$", var.database_name))
    error_message = "database_name must be upper case letters, digits and underscores, starting with a letter."
  }
}

variable "schema_name" {
  type        = string
  description = "Schema that holds state"
  default     = "BACKEND"
}

variable "admin_role_name" {
  type        = string
  description = "Overrides <database_name>_ADMIN"
  default     = null
}

variable "runtime_role_name" {
  type        = string
  description = "Overrides <database_name>_SVC"
  default     = null
}

variable "reader_role_name" {
  type        = string
  description = "Overrides <database_name>_READER"
  default     = null
}

variable "warehouse_name" {
  type        = string
  description = "Overrides <database_name>_WH"
  default     = null
}

variable "service_user_name" {
  type        = string
  description = "Overrides <database_name>_SERVICE"
  default     = null
}

variable "service_public_key" {
  type        = string
  description = "Public key body for the service user. The user is only created when this is set"
  default     = null
}
