# Upgrading from 1.x to 2.0

Named `UPGRADE.md` rather than `MIGRATION.md` because in a migration tool the
word "migration" is already taken, and a file called that reads like
documentation of the file format.

There is no automatic path. Version 2 shares no code with 1.x, and its
bookkeeping table is not the one `golang-migrate` wrote. Below is what to change
and, for a database already under 1.x, how to adopt it.

## The module path

```
github.com/efureev/db-migrator      →  github.com/efureev/db-migrator/v2
```

```bash
go install github.com/efureev/db-migrator/v2/cmd/migrator@latest
```

The image moves from `feugene/migrate` to `ghcr.io/efureev/migrator`.

## Configuration

The `DB_*` and `MIGRATION_*` prefixes are gone, and their disappearance is the
main reason to upgrade: `envconfig.MustProcess("DB", …)` meant that Kubernetes,
which injects `DB_PORT=tcp://10.x.x.x:5432` for any Service named `db`, made the
binary **panic on startup**.

| 1.x | 2.0 |
|---|---|
| `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASS`, `DB_NAME` | `MIGRATOR_DSN`, or `DATABASE_URL`, or libpq's `PG*` |
| `MIGRATION_DIR` | `MIGRATOR_DIR` |
| `config.yaml`, `-f <path>` | `.env`, or `--env-file`, or plain environment variables |

```bash
# 1.x
DB_HOST=db DB_PORT=5432 DB_USER=app DB_NAME=shop MIGRATION_DIR=./migrations migrate up

# 2.0
MIGRATOR_DSN='postgres://app@db:5432/shop' MIGRATOR_DIR=./migrations migrator up
```

The YAML config file is gone. Mount a `.env` instead, or set the variables; they
are read in that order.

## Commands

| 1.x | 2.0 |
|---|---|
| `create -n <name>` | `create <name>` |
| `up`, `down` | the same, but `down` needs `--allow-down` |
| `fresh` | `redo` for the development case; `wipe && up` for the destructive one |
| `wipe` | `wipe --allow-wipe --confirm <database>` |
| `db:version` | `status --current` |
| `version` | the same, but now it means the version of the binary |
| `status` | the same, plus `--check` and `--json` |

`fresh` was `Drop()` followed by `Up()`, with no confirmation and no idea which
database it was pointed at. The two halves are now separate on purpose.

## Migration files

The naming is unchanged: `<version>_<name>.up.sql` and `.down.sql`. Files
written by 1.x load as they are — the Unix timestamps it produced sort before
2.0's `20060102150405` format both numerically and lexically, so the two can
coexist in one directory and no renaming is needed.

Two rules are stricter than they were, and both fail loudly at load rather than
quietly at run time:

- The name must be lower snake case. `CreateUsers.up.sql` is rejected.
- The extension must be lower case. `.UP.SQL` worked on macOS and vanished
  inside `embed.FS`.

## Adopting a database that 1.x migrated

`golang-migrate` recorded one row: `version` and `dirty`. Version 2 records a row
per migration, with a checksum, so the two are not compatible and no conversion
is possible without deciding what the checksums should be.

For a database whose schema is known to match the files:

```bash
# 1. Check what 1.x thinks is applied.
psql -c 'SELECT * FROM schema_migrations'

# 2. Move the old table out of the way rather than dropping it.
psql -c 'ALTER TABLE schema_migrations RENAME TO schema_migrations_v1'

# 3. Let version 2 create its own journal and record every migration up to the
#    version 1.x reported, without running any of them.
migrator repair --baseline <version>   # planned for 2.1; until then, see below
```

Until `repair --baseline` ships, the manual equivalent is to run
`migrator up` against a **copy** of the database, take the resulting
`schema_migrations` rows, and insert them into the real one. The checksums must
be the ones this version of the files produces; `migrator status` on the copy
shows them.

A database that has not been migrated yet needs none of this: point version 2 at
it and run `migrator up`.

## What to expect the first time

- `migrator validate` will list every migration with no down file as a warning.
  That is informational; 1.x never mentioned it.
- If any released migration was edited after it ran, `up` refuses with exit code
  4 and names the file. Restore the file, or put the change in a new migration,
  or — if the edit was cosmetic and every database is known to hold the intended
  schema — `migrator repair --rehash <version>`.
- `migrator config` shows every setting and where its value came from, which is
  the fastest way to see what the new variable names picked up.
