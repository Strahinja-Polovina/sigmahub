-- The dashboard keeps its own database on the shared Postgres instance
-- (better-auth users + the local read-model mirror). Runs once on first boot
-- via docker-entrypoint-initdb.d.
CREATE DATABASE sigmahub_web OWNER sigmahub;
