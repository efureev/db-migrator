# db-migrator

SQL migrations for PostgreSQL: a library and a CLI.

[Русская версия](Readme.ru.md)

```bash
go install github.com/efureev/db-migrator/v2/cmd/migrator@latest
```

```bash
migrator create 'add users table'   # writes 20260819120000_add_users_table.{up,down}.sql
migrator status                     # what is applied, what is pending
migrator up                         # apply everything pending
```

## What it does differently

**Each migration and the row recording it share one transaction.** "Applied but
not recorded" is not a state this can produce.

**Every migration carries a checksum of its file.** Editing a migration that has
already run is refused, loudly, before anything else runs — every database that
applied the old text will never see the new one. The checksum is taken over a
normalised form, so a Windows checkout with `core.autocrlf=true` does not look
like an edit.

**`CREATE INDEX CONCURRENTLY` works.** A migration marked
`-- migrator:no-transaction` runs outside a transaction, one statement at a time.
Sending the file whole would not do: PostgreSQL wraps a multi-statement simple
query in an implicit transaction, and `CONCURRENTLY` fails inside one. Such a
migration is recorded in two steps, so a crash between them leaves evidence
instead of silence.

**A dry run says what each statement will lock.** Not "this migration is risky"
— the lock mode, the table, its size, and whether it rewrites or scans:

```
  Plan  1 migration(s) up

    20260901130000_widen_status  transactional, 1 statement(s)
      ALTER TABLE orders ALTER COLUMN status TYPE text
        ACCESS EXCLUSIVE on orders (~8 900 000 rows), REWRITES THE TABLE
```

`--max-lock-level share-update-exclusive` turns that into a refusal, before the
first statement runs. A migration that needs the heavier lock says so in its own
text, with `-- migrator:lock-acknowledged access-exclusive`. There is no flag
that waives it: the decision belongs at review time, in the file, not with
whoever is holding the deploy at three in the morning.

It is a heuristic over statement text, not a planner — it cannot see triggers,
rules, inheritance, or the queue in front of the lock. Its rule table is checked
against a real server, which is a different thing from being checked against
itself.

**Concurrent runs serialise on a PostgreSQL advisory lock**, taken before the
bookkeeping table is created. The lock is session-level, so a process that dies
releases it — there is no lock row to clean up by hand.

**Destructive operations need three separate things.** `wipe` needs
`--allow-wipe`, a `--confirm <database>` that matches what the connection
actually reports, and an environment that is not production. `--yes` is never
enough: it means "stop asking", and `--confirm shop_dev` means "I know which
database this is". A typo in the name is what saves you.

**Exit codes distinguish "retry" from "wake somebody up".**

## Using it as a library

The main scenario for a service that ships its migrations inside its binary:

```go
import (
    "embed"
    "io/fs"

    migrator "github.com/efureev/db-migrator/v2"
)

//go:embed migrations/*.sql
var embedded embed.FS

func migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
    src, err := fs.Sub(embedded, "migrations")
    if err != nil {
        return err
    }

    m, err := migrator.New(migrator.FromPool(pool), src,
        migrator.WithLogger(log),
        migrator.WithSchema("app"),
    )
    if err != nil {
        return err
    }

    report, err := m.Up(ctx)
    if err != nil {
        return err
    }

    log.Info("migrations", "report", report)

    return nil
}
```

`FromPool` holds one connection for the whole run rather than routing statements
across the pool. That is not an optimisation: a session-level advisory lock taken
on one backend and released on another is not released at all, and `SET` does not
reach the statement that needed it. `FromConn` and `FromDSN` are the other two
ways in.

A `Migrator` built this way physically cannot wipe the schema or roll anything
back — those need `WithAllowWipe` and `WithAllowDown`, which an application has
no reason to pass.

## Migration files

```
migrations/
  20260819120000_add_users_table.up.sql
  20260819120000_add_users_table.down.sql
```

`<version>_<name>.up.sql` and its `.down.sql`. The version is a number, the name
is lower snake case, and both halves of the extension are lower case — a
capitalised `.UP.SQL` works on a case-insensitive filesystem and vanishes inside
`embed.FS`, which is the most expensive shape of failure there is.

A migration without a down file is allowed. It is reported by `validate` and
refused only when a rollback of it is actually requested.

### Directives

Lines at the head of the file, above any SQL:

```sql
-- migrator:no-transaction        -- required by CREATE INDEX CONCURRENTLY
-- migrator:retry-safe            -- idempotent; safe to re-run after a crash
-- migrator:statement-timeout 30m
-- migrator:lock-timeout 5s
-- migrator:tags ddl,slow
-- migrator:lock-acknowledged access-exclusive
```

An unrecognised directive is an error, not a warning: a mistyped
`no-transacton` leaves `CREATE INDEX CONCURRENTLY` inside a transaction, where it
fails with SQLSTATE 25001 if you are lucky and holds a lock across the whole
migration if you are not.

## Commands

| Command | What it does |
|---|---|
| `up` | apply pending migrations; `--to`, `--steps`, `--dry-run`, `--max-lock-level` |
| `down` | roll back; needs `--allow-down`, refused in production |
| `redo` | roll back and re-apply, under one lock |
| `status` | what is applied and what is pending; `--check` for CI, `--current` for scripts |
| `validate` | every problem at once; `--strict`, `--offline` |
| `create` | write a new pair of files; needs no database |
| `adopt` | record what an existing database already has, without running it |
| `repair` | fix the journal without touching the schema |
| `wipe` | drop everything in the schema; three separate guards |
| `locks` | who holds the migration lock, and for how long |
| `config` | the resolved configuration and where each value came from |
| `version` | the version of the binary |

`fresh` from version 1 is gone. It was `Drop()` plus `Up()` with no confirmation
and no idea what database it was pointed at — two different things under one
word. The convenience is now `redo`; the destructive half is
`migrator wipe && migrator up`, which is two commands on purpose.

## Configuration

Highest priority first: a flag actually typed, then `MIGRATOR_*`, then
`DATABASE_URL` and libpq's `PG*`, then the `.env` files named by `--env-file`,
then `./.env` if it exists, then the built-in default.

| Variable | Flag | Default |
|---|---|---|
| `MIGRATOR_DSN` | `--dsn` | — |
| `MIGRATOR_DIR` | `-d`, `--dir` | `./migrations` |
| `MIGRATOR_SCHEMA` | `--schema` | `public` |
| `MIGRATOR_TABLE` | `--table` | `schema_migrations` |
| `MIGRATOR_ENV` | `--env` | inferred |
| `MIGRATOR_ADVISORY_LOCK_TIMEOUT` | `--advisory-lock-timeout` | `30s` |
| `MIGRATOR_LOCK_TIMEOUT` | `--lock-timeout` | `3s` |
| `MIGRATOR_STATEMENT_TIMEOUT` | `--statement-timeout` | `0` |
| `MIGRATOR_LOG_LEVEL` | `--log-level` | `info` |
| `MIGRATOR_JSON` | `--json` | `false` |

`--allow-down`, `--allow-wipe` and `--confirm` have no environment variables on
purpose. A variable is inherited by every process in a container and every step
of a CI job; a flag is typed for one run.

An empty DSN is not an error: pgx then reads `PGHOST`, `PGUSER`, `PGDATABASE`
and `~/.pgpass`, so an unconfigured `migrator` connects exactly where `psql`
with no arguments connects.

`--lock-timeout` defaults to three seconds rather than none. Without one, an
`ALTER TABLE` queued behind a long-running `SELECT` queues every later query
behind itself — that is not a slow migration, it is an outage. Three seconds
turns the outage into a failed deploy.

## Exit codes

| Code | Meaning | What to do |
|---|---|---|
| 0 | done, or nothing to do | — |
| 1 | the run failed | read the error |
| 2 | the command was not understood | fix the invocation |
| 3 | migrations are pending (`status --check`) | deploy |
| 4 | files and journal disagree | a person has to look |
| 5 | another migrator holds the lock | retry |
| 6 | refused by a guard | fix the pipeline, not the database |

Telling 5 from 1 is the point: in CI the right reaction to "another deploy holds
the lock" is to wait a minute, and the right reaction to "the SQL is wrong" is
not.

## Docker

```bash
docker run --rm \
  -e MIGRATOR_DSN='postgres://app:secret@db:5432/shop?sslmode=disable' \
  -e MIGRATOR_ENV=production \
  -v "$PWD/migrations:/migrations:ro" \
  ghcr.io/efureev/migrator:2 up
```

Distroless, running as user 65532. `:ro` on the migrations is enough for
everything except `create`, which is run locally anyway. A `-alpine` tag ships
the same binary with a shell, for an init container that wants
`sh -c "migrator up && …"`.

In Kubernetes, set `MIGRATOR_ENV=production` in the manifest: `down` and `wipe`
are then unavailable whatever anybody types.

## Development

```bash
make help              # every target
make check             # gofmt, vet, linter, unit tests under -race
make db-up             # local PostgreSQL on port 55439
make test-integration  # the integration level against it
make coverage          # per-package coverage thresholds
```

Three levels of test: unit ones that touch neither disk nor database, an
integration level that requires a real PostgreSQL, and an `e2e` package that
runs the built binary as a subprocess. The integration level refuses to skip
itself in CI: a silently skipped level looks exactly like a green run.

See [CONTRIBUTING.md](CONTRIBUTING.md) and [UPGRADE.md](UPGRADE.md).
[CHANGELOG.md](CHANGELOG.md) says what each release changed and why.

## License

MIT.
