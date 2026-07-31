-- Runs once, as the superuser, when the Postgres data directory is first
-- created (docker-entrypoint-initdb.d). Two jobs, both of which have to
-- happen before memory-vault ever connects:
--
--   1. Create the pgvector extension. CREATE EXTENSION needs privileges the
--      app role deliberately does not have; migrationSQL's
--      "CREATE EXTENSION IF NOT EXISTS vector" is then a privilege-free
--      no-op for the app.
--
--   2. Create the unprivileged role the app actually connects as. This is
--      the load-bearing part: superusers and BYPASSRLS roles ignore
--      row-level security entirely, so running the app as POSTGRES_USER
--      (which the postgres image creates as a superuser) would leave the
--      tenant isolation policy on `memories` fully configured and enforcing
--      nothing. store.Open refuses to start in that state rather than
--      serving tenants a false boundary.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE ROLE memory_vault_app LOGIN PASSWORD 'memory_vault_app';

-- The app owns the tables it creates in migrationSQL, which is what lets it
-- run ALTER TABLE ... FORCE ROW LEVEL SECURITY on them. Ownership is not
-- superuser: an owner is still bound by FORCE'd policies.
GRANT ALL ON SCHEMA public TO memory_vault_app;
