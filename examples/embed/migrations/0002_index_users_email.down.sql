-- migrator:no-transaction
--
-- DROP INDEX CONCURRENTLY needs the directive too, and needs it in its own file.

DROP INDEX CONCURRENTLY IF EXISTS users_created_at_idx;
