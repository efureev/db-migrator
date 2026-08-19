-- migrator:no-transaction
--
-- CREATE INDEX CONCURRENTLY cannot run inside a transaction. Without the
-- directive above, PostgreSQL rejects this with SQLSTATE 25001.

CREATE INDEX CONCURRENTLY IF NOT EXISTS users_created_at_idx ON users (created_at);
