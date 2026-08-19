# Changelog

Keep a Changelog, SemVer.

## [2.1.0] — unreleased

Migrations now say what they will lock before they run, and say what they are
doing while they run.

### Added

- **A long migration reports its progress.** PostgreSQL publishes the progress
  of its own long operations and almost nobody reads it. Every
  `--progress-interval` — 30 seconds by default — a second connection reads
  those views for the backend running the migration and writes one line:

  ```
  migrator: migration in progress  version=20260901130000 source=create_index
            phase=building index  relation=users  percent=30
            progress=12 400 000 of 41 200 000 blocks
  ```

  An index build, a VACUUM, an ANALYZE, a CLUSTER and a COPY are covered by a
  view each. Everything else — a table rewrite, a backfill, a wait on a lock —
  is covered by none of them, and those are the majority of slow migrations, so
  the line then reports `pg_stat_activity` instead: the state, the wait event,
  and how long the statement has been running. An hour-long
  `CREATE INDEX CONCURRENTLY` that says nothing is an hour in which nobody knows
  whether it is alive, and that was the whole of the previous behaviour.

  It costs a second connection, which is why it is not unconditional.
  `FromDSN` and `FromPool` can supply one and do so by default; `FromConn` hands
  back the connection its caller already owns and cannot. That is *detected*
  rather than assumed — both connections are asked for `pg_backend_pid()` before
  the first statement runs — so a hand-written `Connector` wrapping a single
  connection is caught as well. Nothing about it can fail a run: a pool with one
  connection in it, a server that will not answer, a role that may not see the
  backend, all mean no reporting and one line at debug level.

  Nothing is polled unless a logger was given. The default logger discards
  everything, so a library caller who never asked for one does not pay for the
  connection; `--quiet` and `--log-level error` have the same effect. The lines
  go to stderr and switch to JSON along with everything else under `--json`, so
  a consumer piping stdout into `jq` never sees them.

  `WithProgress`, `WithProgressInterval`, `--progress-interval` and
  `MIGRATOR_PROGRESS_INTERVAL`.

- **`migrator up --dry-run` predicts the locks.** Every statement is classified:
  which lock it takes, whether it rewrites the table, whether it scans it, and
  how many rows are in the table it is about to hold.

  ```
    Plan  2 migration(s) up

      20260901130000_widen_status  transactional, 1 statement(s)
        ALTER TABLE orders ALTER COLUMN status TYPE text
          ACCESS EXCLUSIVE on orders (~8 900 000 rows), REWRITES THE TABLE
          changing a column's type rewrites the table unless the two types are
          binary-coercible …
  ```

  Row counts come from `pg_class.reltuples`, and version-dependent rules read
  `server_version_num` — before PostgreSQL 11 a `DEFAULT` on a new column
  rewrote every row, and offline that is the answer given, because the
  optimistic guess is the one that causes an outage.

- **`WithMaxLockLevel` / `--max-lock-level` refuses a run that would take a
  heavier lock than allowed.** The refusal happens under the advisory lock,
  before the first statement, and names the migration, the statement, the table
  and its size. Exit code 6: fix the migration, not the database.

  A migration that genuinely needs the heavier lock says so in its own text:

  ```sql
  -- migrator:lock-acknowledged access-exclusive
  ```

  There is no flag that waives it. The decision belongs where the knowledge is —
  at review time, in the file — not with whoever is holding the deploy at three
  in the morning. Same principle as `retry-safe`.

- **`up --dry-run --json`** now emits the plan, predictions included, with the
  usual `"format": 1`.

The rule table is checked against the server rather than against itself: an
integration test runs each statement on a real PostgreSQL and reads `pg_locks`
to see what was actually taken. Where the two disagreed, PostgreSQL was right.
It is still a heuristic over statement text — it cannot see triggers, rules,
inheritance, or the queue in front of the lock — and the documentation says so
rather than promising more.

## [2.0.1] — 2026-08-19

2.0.0 shipped without its own command. Install it and there was nothing to run.

### Fixed

- **`cmd/migrator` was missing from the 2.0.0 tag**, so
  `go install github.com/efureev/db-migrator/v2/cmd/migrator@v2.0.0` fails with
  "found, but does not contain package", and both release jobs failed while
  building it. The cause was one line in `.gitignore`: `migrator`, meant for the
  binary a plain `go build` drops in the root. A pattern without a leading slash
  matches every path component, so it also matched `cmd/migrator/`. It is now
  `/migrator`.

  Nothing local could have caught it: the compiler, the linter and the tests all
  read the working tree, where the file was present. Only git disagreed, and
  nobody was asking git. `TestNoGoFileIsIgnored` now asks, on every run.

  **2.0.0 cannot be repaired.** A version in the module proxy is immutable, so
  it stays broken. The library half of 2.0.0 was intact — only the command was
  missing — so `go get` worked and `go install` did not; 2.0.1 is the first
  release of the 2.x line that installs.
- **`migrator version --json` ignored the flag** and printed the human sentence.
  It now writes the same `"format": 1` object shape as every other command, with
  the version, commit, build date, Go version and platform.

## [2.0.0] — 2026-08-19 (no command; use 2.0.1 or later)

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
- **`--json`** on every command, every object carrying `"format": 1` so that a
  consumer can tell one release's shape from another's.
- **Three levels of test**: unit, integration against a real PostgreSQL on
  14/17/18, and an `e2e` package that runs the built binary as a subprocess.
- **CI that runs on every push**, with per-package coverage thresholds.
- **`darwin/arm64` binaries**, which 1.x never shipped.

### Added since the first draft of 2.0

- **`adopt`** — records the migrations an existing database already has, without
  running them, so that a database built by hand or by golang-migrate can be
  handed over. Rows stay marked as adopted forever: "applied" and "we were told
  it was applied" are different claims.
- **The journal upgrades itself.** `CREATE TABLE IF NOT EXISTS` does nothing to a
  table that already exists, so a release that added a column would have failed
  on every installation with "column does not exist". Missing columns are now
  added, additively, on every run — and a table under our name that is *not*
  ours (golang-migrate's shares it) is refused rather than altered.
- **`locks`** — who holds the advisory lock, and for how long.
- **A privilege preflight**, so that a missing GRANT is a refusal that changed
  nothing rather than a failure three migrations in.
- **`wipe` refuses when another schema depends on this one**, listing what
  `CASCADE` would have taken with it. `WithForceWipe` accepts it deliberately.
- **`"format": 1`** on every `--json` object, so a consumer can tell one
  release's shape from another's.

### Removed

- `golang-migrate/v4`, `spf13/viper`, `spf13/cast`, `integrii/flaggy`,
  `kelseyhightower/envconfig`, `iancoleman/strcase`, `lib/pq`, `yaml.v3`.
  The library depends on `pgx/v5` and nothing else; the CLI adds
  `efureev/reggol` and `efureev/envi/v2`, both dependency-free.

## [1.4.10] — 2024-10-31

The last release of the 1.x line. See the tag.
