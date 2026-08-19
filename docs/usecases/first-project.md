# Starting a new project

*[По-русски](first-project.ru.md)*

**Use this when** the database is empty and this tool is the first one to touch
it. If the schema already exists, you want
[Adopting an existing database](adopt-existing-database.md) instead — `up`
against tables that are already there fails on migration 1.

## 1. Write the first migration

`create` needs no database. It writes both halves and prints their paths.

```console
$ migrator create add_users_table
migrations/20260819194642_add_users_table.up.sql
migrations/20260819194642_add_users_table.down.sql
```

The version is a timestamp by default (`--format unix` and `--format sequential`
exist). The up file arrives with a comment explaining the directives and
deliberately not using them:

```sql
-- Write the migration below.
--
-- If it needs a directive, add one above the SQL, one per line, in the form
-- shown here with the leading marker restored:
--
--   ..migrator:no-transaction        required by CREATE INDEX CONCURRENTLY
--                                    and ALTER TYPE ... ADD VALUE
--   ..migrator:retry-safe            this migration is idempotent and may be
--                                    re-run after an interrupted attempt
--   ..migrator:statement-timeout 30m
--   ..migrator:lock-timeout 5s
--
-- Replace the ".." with "-- " when you use one. They are written that way here
-- because a real directive in this template would apply to every migration
-- this tool creates — and "-- migrator:no-transaction" on an ordinary
-- migration costs it the atomicity of its own bookkeeping.
```

The `..` is not a typo. A real `-- migrator:` line in the template would be a
real directive in every file the tool ever creates.

Fill both halves in:

```sql
-- 20260819194642_add_users_table.up.sql
CREATE TABLE users (
    id    bigserial PRIMARY KEY,
    email text NOT NULL UNIQUE
);
```

```sql
-- 20260819194642_add_users_table.down.sql
DROP TABLE users;
```

## 2. Look before you run

`status` writes nothing — no journal, no lock — so it is safe against a replica
and a read-only role.

```console
$ migrator status
  The journal "public.schema_migrations" does not exist yet: nothing has been applied.
  "migrator up" will create it.

  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  20260819194642   add_users_table              pending      -                     -

  0 applied, 1 pending.  No current version.
```

## 3. Apply

```console
$ migrator up
INF migrator: migration applied direction=up name=add_users_table took=2ms version=20260819194642
  applied    20260819194642_add_users_table  2ms

  Done. 1 applied in 21ms. Current version 20260819194642.
```

Two streams, and the split matters if you script this: the answer goes to
**stdout**, the running commentary to **stderr**. `migrator up | tee deploy.log`
captures the answer and leaves the commentary on your terminal; `--json` makes
stdout a single JSON object and the commentary JSON lines.

The migration and the row recording it share one transaction. "Applied but not
recorded" cannot happen — which is why a migration that *cannot* run in a
transaction has to say so with `-- migrator:no-transaction`, and is then
recorded in two steps so that a crash between them leaves evidence rather than
silence.

## 4. Confirm

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  20260819194642   add_users_table              applied      2026-08-19 22:46:57   2ms

  1 applied, 0 pending.  Current version 20260819194642.
```

Running `up` again is a no-op and exits 0. "Nothing to do" is a success, not a
special case — a deploy that runs `migrator up` on every release needs it to be.

```console
$ migrator up
Schema is up to date. Nothing to apply.
```

For a script that wants the version and nothing else:

```console
$ migrator status --current
20260819194642_add_users_table
```

## Connecting

Highest priority first: a flag you actually typed, then `MIGRATOR_*`, then
`DATABASE_URL` and libpq's `PG*`, then `--env-file`, then `./.env`, then the
built-in default.

```bash
export MIGRATOR_DSN='postgres://app:secret@localhost:5432/shop?sslmode=disable'
migrator up
```

An empty DSN is not an error: pgx then reads `PGHOST`, `PGUSER`, `PGDATABASE`
and `~/.pgpass`, so an unconfigured `migrator` connects exactly where `psql`
with no arguments connects. `migrator config` shows every resolved value and
where it came from.

## Next

- [Making a schema change safely](safe-schema-change.md) — what each statement
  will lock, before it runs
- [In CI and in a deploy](ci-and-deploy.md) — exit codes and `--json`
- [Rolling back, and repairing the journal](rollback-and-repair.md)
