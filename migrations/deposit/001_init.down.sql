-- Rollback 001_init: drop all tables created in 001_init in reverse dependency order.
DROP TABLE IF EXISTS cursors;
DROP TABLE IF EXISTS forwards;
DROP TABLE IF EXISTS intents;
