-- snowstate Seed. An admin runs this once per account, as ACCOUNTADMIN.
-- It creates the identity Terraform starts from, and nothing else. New Installs
-- do not need it again. See deploy/terraform/foundation.
USE ROLE ACCOUNTADMIN;

CREATE ROLE IF NOT EXISTS SNOWSTATE_SEED COMMENT = 'Creates and changes snowstate Foundations. Not for runtime use.';
GRANT CREATE WAREHOUSE, CREATE DATABASE, CREATE ROLE, CREATE USER, MANAGE GRANTS ON ACCOUNT TO ROLE SNOWSTATE_SEED;
GRANT ROLE SNOWSTATE_SEED TO ROLE SYSADMIN;

-- Key-pair auth. Paste the public key body.
CREATE USER IF NOT EXISTS SNOWSTATE_SEED
  TYPE = SERVICE
  DEFAULT_ROLE = SNOWSTATE_SEED
  RSA_PUBLIC_KEY = '<public key body>'
  COMMENT = 'Foundation changes only. Never put this key on a server.';
GRANT ROLE SNOWSTATE_SEED TO USER SNOWSTATE_SEED;
