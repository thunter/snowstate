# snowstate

snowstate is a Terraform and OpenTofu state backend that keeps state, version history, locks and an audit trail in Snowflake.

## Language

**Seed**:
The one-time manual step that creates the narrow identity Terraform starts from, before anything else exists.
_Avoid_: Bootstrap (used too loosely for the whole install)

**Foundation**:
The Terraform-managed account-level objects an Install needs, such as its database, schema, warehouse, roles and grants.
_Avoid_: Bootstrap, infrastructure

**Install**:
One independent snowstate deployment in a Snowflake account, identified by its database name. An account can hold several.
_Avoid_: Deployment, instance, environment
