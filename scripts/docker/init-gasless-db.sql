-- Runs once when the compose Postgres volume is first created: the gasless
-- service gets its own database, separate from the deposit bridge's.
-- (Existing volumes: run `docker compose down -v` to recreate, or create it by hand.)
CREATE DATABASE latch_gasless;
