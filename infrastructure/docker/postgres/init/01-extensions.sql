-- Runs once, automatically, on first container start of an empty data
-- volume (docker-entrypoint-initdb.d convention -- see the postgres image's
-- own docs). It exists so a completely fresh `docker compose up` has
-- pg_trgm available the moment migrations run against it, without a manual
-- step.
--
-- database/migrations/00001_initial.sql *also* runs
-- `CREATE EXTENSION IF NOT EXISTS pg_trgm;` itself, so this file is
-- deliberately redundant with the migration -- belt and suspenders, not a
-- substitute for it. Both are idempotent (`IF NOT EXISTS`), so running both
-- is harmless. This file is what lets `pg_trgm`-dependent indexes exist even
-- if migrations are ever applied by a role without CREATE EXTENSION
-- privilege on a hardened, non-Compose deployment -- the extension is
-- guaranteed to already be present by the time migrate runs locally.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
