-- snowstate bootstrap. An admin runs this once. Edit names to taste.
-- Variant A: shared tenancy (one schema, access enforced by snowstate tokens).
-- Variant B: schema_per_project (access enforced by Snowflake grants, SPEC §5.3).
-- Variant C: GitHub Actions identities (SPEC §8.4.1).

-- ------------------------------------------------------------------ common
USE ROLE SYSADMIN;
CREATE WAREHOUSE IF NOT EXISTS SNOWSTATE_WH
  WAREHOUSE_SIZE = XSMALL AUTO_SUSPEND = 60 AUTO_RESUME = TRUE INITIALLY_SUSPENDED = TRUE;
CREATE DATABASE IF NOT EXISTS TF_STATE;

USE ROLE SECURITYADMIN;
CREATE ROLE IF NOT EXISTS SNOWSTATE_ADMIN COMMENT = 'Owns snowstate objects and runs migrate, GC and token admin';
CREATE ROLE IF NOT EXISTS SNOWSTATE_SVC COMMENT = 'Runtime role for the snowstate server. No DDL.';
CREATE ROLE IF NOT EXISTS SNOWSTATE_READER COMMENT = 'Inventory view only, no raw state';
GRANT ROLE SNOWSTATE_SVC TO ROLE SNOWSTATE_ADMIN;
GRANT ROLE SNOWSTATE_ADMIN TO ROLE SYSADMIN;

GRANT USAGE ON WAREHOUSE SNOWSTATE_WH TO ROLE SNOWSTATE_SVC;
GRANT USAGE ON WAREHOUSE SNOWSTATE_WH TO ROLE SNOWSTATE_READER;
GRANT USAGE, CREATE SCHEMA ON DATABASE TF_STATE TO ROLE SNOWSTATE_ADMIN;
GRANT USAGE ON DATABASE TF_STATE TO ROLE SNOWSTATE_SVC;
GRANT USAGE ON DATABASE TF_STATE TO ROLE SNOWSTATE_READER;

-- ---------------------------------------------------------- variant A: shared
USE ROLE SNOWSTATE_ADMIN;
CREATE SCHEMA IF NOT EXISTS TF_STATE.BACKEND WITH MANAGED ACCESS;

-- Run `snowstate migrate` as SNOWSTATE_ADMIN now, then apply these grants.
USE ROLE SNOWSTATE_ADMIN;
GRANT USAGE ON SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_SVC;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_SVC;
GRANT SELECT, INSERT, UPDATE, DELETE ON FUTURE TABLES IN SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_SVC;
GRANT READ, WRITE ON ALL STAGES IN SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_SVC;
GRANT READ, WRITE ON FUTURE STAGES IN SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_SVC;
GRANT USAGE ON SCHEMA TF_STATE.BACKEND TO ROLE SNOWSTATE_READER;
GRANT SELECT ON VIEW TF_STATE.BACKEND.STATE_RESOURCES TO ROLE SNOWSTATE_READER;

-- Service user with key-pair auth. Paste the public key body.
USE ROLE SECURITYADMIN;
CREATE USER IF NOT EXISTS SNOWSTATE_SERVICE
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_SVC
  DEFAULT_WAREHOUSE = SNOWSTATE_WH
  RSA_PUBLIC_KEY = '<public key body>';
GRANT ROLE SNOWSTATE_SVC TO USER SNOWSTATE_SERVICE;

-- Sample network policy. Adjust the list, then attach it to the service user.
-- CREATE NETWORK POLICY SNOWSTATE_NP ALLOWED_IP_LIST = ('203.0.113.0/24');
-- ALTER USER SNOWSTATE_SERVICE SET NETWORK_POLICY = SNOWSTATE_NP;

-- ---------------------------------------------- variant B: schema_per_project
-- Direct mode isolates teams with grants (SPEC §5.3). Each team gets a role, and
-- `snowstate project create` makes the schema and applies the grants. GC,
-- restore-overwrite and token admin stay with SNOWSTATE_ADMIN, so a client with
-- direct credentials cannot erase history.
USE ROLE SECURITYADMIN;
CREATE ROLE IF NOT EXISTS SNOWSTATE_P_PLATFORM COMMENT = 'read,write,lock on project platform';
CREATE ROLE IF NOT EXISTS SNOWSTATE_P_PLATFORM_PLAN COMMENT = 'read,lock on project platform (plan jobs)';
GRANT USAGE ON WAREHOUSE SNOWSTATE_WH TO ROLE SNOWSTATE_P_PLATFORM;
GRANT USAGE ON WAREHOUSE SNOWSTATE_WH TO ROLE SNOWSTATE_P_PLATFORM_PLAN;
GRANT ROLE SNOWSTATE_P_PLATFORM TO ROLE SNOWSTATE_ADMIN;
GRANT ROLE SNOWSTATE_P_PLATFORM_PLAN TO ROLE SNOWSTATE_ADMIN;

-- As SNOWSTATE_ADMIN, with tenancy.mode=schema_per_project:
--   snowstate migrate
--   snowstate project create platform --role SNOWSTATE_P_PLATFORM \
--     --plan-role SNOWSTATE_P_PLATFORM_PLAN

-- ------------------------------------------------- variant C: GitHub Actions
-- One service user per repo and environment. SUBJECT must equal the token's
-- `sub` claim. See docs/github-actions.md.
USE ROLE SECURITYADMIN;
CREATE USER IF NOT EXISTS GH_PLATFORM_NETWORK_PROD
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_P_PLATFORM          -- project role, or SNOWSTATE_SVC for shared tenancy
  DEFAULT_WAREHOUSE = SNOWSTATE_WH
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod'
  );
GRANT ROLE SNOWSTATE_P_PLATFORM TO USER GH_PLATFORM_NETWORK_PROD;

-- The plan identity: read,lock only, for plan-on-PR jobs.
CREATE USER IF NOT EXISTS GH_PLATFORM_NETWORK_PLAN
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_P_PLATFORM_PLAN
  DEFAULT_WAREHOUSE = SNOWSTATE_WH
  WORKLOAD_IDENTITY = (
    TYPE    = OIDC
    ISSUER  = 'https://token.actions.githubusercontent.com'
    SUBJECT = 'repo:<org>/<repo>:environment:prod-plan'
  );
GRANT ROLE SNOWSTATE_P_PLATFORM_PLAN TO USER GH_PLATFORM_NETWORK_PLAN;
