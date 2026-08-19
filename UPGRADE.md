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
per migration, with a checksum, so the two journals are not compatible. Handing a
database over is one command:

```bash
migrator adopt --from-golang-migrate --confirm <database>
```

It reads the version out of the old journal, records every migration up to and
including it as applied — **running none of them** — and moves the old table
aside as `schema_migrations_pre_v2` rather than dropping it, so a rollback to the
old tool stays possible.

Run it with `--dry-run` first: that prints exactly which versions would be
recorded and writes nothing.

If the old journal is marked `dirty`, adoption refuses. That flag means a
migration failed partway and nobody recorded what state it left the schema in;
adopting would freeze an unknown state as the truth. Resolve it with the old
tool first.

For a database whose schema was built by hand or by something else entirely,
name the version yourself:

```bash
migrator adopt --baseline 20240517101122 --confirm <database>
```

### What adoption is, and what it is not

Adoption takes your word that the schema matches the files. That word is the
whole risk, so the rows are marked: `migrator status` shows them as `(adopted)`
forever, and `--json` carries `"adopted": true`.

**Check the schema before adopting.** A database that does not match the files is
not made to match by adopting it — it is made to look as though it does, which is
worse. The cheapest check is to run `migrator up` against a *copy* restored from
a dump, see that it produces the schema you expect, and only then adopt the real
one.

Adoption runs no migration SQL and touches no schema object other than renaming
that one table.

## What to expect the first time

- `migrator validate` will list every migration with no down file as a warning.
  That is informational; 1.x never mentioned it.
- If any released migration was edited after it ran, `up` refuses with exit code
  4 and names the file. Restore the file, or put the change in a new migration,
  or — if the edit was cosmetic and every database is known to hold the intended
  schema — `migrator repair --rehash <version>`.
- `migrator config` shows every setting and where its value came from, which is
  the fastest way to see what the new variable names picked up.
