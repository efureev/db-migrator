# Changelog

Keep a Changelog, SemVer.

## [2.0.0] — unreleased

A rewrite. Nothing is shared with 1.x except the idea.

### Why

Three defects in 1.4.10 broke silently, and two of them had survived eight
releases:

- The binary **panicked on startup in Kubernetes**. Configuration was read with
  `envconfig.MustProcess("DB", …)`, and Kubernetes injects `DB_PORT=tcp://…` for
  any Service named `db`. A prefix that broad does not belong to one tool.
- `migrator --version` **always printed `unknown`**. The build script stamped
  `-X migrator/src/commands.version` while the module was
  `github.com/efureev/db-migrator`; the linker ignores an unknown symbol without
  a word.
- **The test suite failed on a clean checkout**, on a path that resolved to a
  directory that never existed.

And the CI only ran on `release: published`, so none of it was ever caught
before a release rather than after.

### Changed

- **Module path is `github.com/efureev/db-migrator/v2`.**
- **Own migration runner on `jackc/pgx/v5`.** `golang-migrate` is gone. Its
  bookkeeping was `version` plus `dirty`, which cannot tell "the migration
  failed" from "the process died halfway", and cannot notice a released file
  being edited at all.
- **Library first, CLI second.** The root package is a reusable API over
  `io/fs.FS` and a pgx connection; `cmd/migrator` is a thin layer on top. A
  service that embeds its migrations no longer needs its own runner.
- **Configuration is `MIGRATOR_*`, flags, and an optional `.env`.** YAML,
  `envconfig` and the `DB_*`/`MIGRATION_*` prefixes are gone. An unset DSN falls
  through to libpq's `PG*`, so an unconfigured run connects where `psql` does.
- **`fresh` is gone.** It was `Drop()` plus `Up()` with no confirmation and no
  notion of which database it was pointed at. The convenience half is now
  `redo`; the destructive half is `wipe && up`, two commands on purpose.
- **`version` means the version of the binary.** The version of the schema is
  `status --current`. One word for both was ambiguous every time.

### Added

- **Checksums.** Every migration records a hash of its file, over a normalised
  form so that CRLF is not mistaken for an edit. Editing a released migration is
  refused before anything runs.
- **`-- migrator:no-transaction`**, and the statement splitter that makes it
  correct. `CREATE INDEX CONCURRENTLY` now works: the migration runs outside a
  transaction, one statement at a time, because PostgreSQL wraps a
  multi-statement simple query in an implicit transaction.
- **Advisory-lock serialisation**, taken before the bookkeeping table exists, so
  that concurrent first runs cannot race on `CREATE TABLE IF NOT EXISTS`. It is
  session-level, so a crashed process leaves nothing to clean up.
- **A transaction-pooler detector.** pgbouncer in transaction mode breaks
  session locks silently; this refuses rather than pretending to be safe.
- **Three independent guards on destructive operations**, and an environment
  that is inferred towards production when nothing says otherwise.
- **`validate`**, reporting every problem at once, and **`repair`**, which edits
  the journal and never the schema — so that no `--force` on `up` has to exist.
- **Seven exit codes**, so that CI can tell "retry" from "wake somebody up".
- **`--json`** on every command.
- **Three levels of test**: unit, integration against a real PostgreSQL on
  14/17/18, and an `e2e` package that runs the built binary as a subprocess.
- **CI that runs on every push**, with per-package coverage thresholds.
- **`darwin/arm64` binaries**, which 1.x never shipped.

### Removed

- `golang-migrate/v4`, `spf13/viper`, `spf13/cast`, `integrii/flaggy`,
  `kelseyhightower/envconfig`, `iancoleman/strcase`, `lib/pq`, `yaml.v3`.
  The library depends on `pgx/v5` and nothing else; the CLI adds
  `efureev/reggol` and `efureev/envi/v2`, both dependency-free.

## [1.4.10] — 2024-10-31

The last release of the 1.x line. See the tag.
