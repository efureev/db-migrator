# Contributing

## The gate

```bash
make check             # gofmt, vet, linter, unit tests under -race
make db-up             # PostgreSQL on port 55439
make test-integration  # the integration level
make coverage          # per-package thresholds
```

`make check` is what runs before a commit. CI runs the same things plus the
integration level against PostgreSQL 14, 17 and 18.

## Three levels of test

**Unit.** No disk, no database, no dependencies beyond the standard library.
Sources are `fstest.MapFS`. Two fuzzers, over file names and over the SQL lexer.

**Integration.** Build tag `integration`, a DSN in `MIGRATOR_TEST_DSN`, and a
database of its own per test — a database rather than a schema, so that `wipe`
and the bootstrap race are testable honestly and `t.Parallel` is safe.

The seam is the DSN: the tests do not know who started the server. That is why
this project does not depend on testcontainers — everything it would provide
here is one `docker run`, and it is still a v0.x module with forty dependencies
behind it. If matrix-testing locally ever becomes worth it, a nested module can
set `MIGRATOR_TEST_DSN` without the main `go.mod` changing at all.

`testdb.DSN` calls `t.Fatal` rather than `t.Skip` when `CI` is set and the DSN is
not. A silently skipped integration level looks exactly like a green run, and
this is the level that covers everything a fake cannot: advisory locks,
`CREATE INDEX CONCURRENTLY`, SQLSTATE 25001, DDL rollback.

**End to end.** The `e2e` package builds the binary and runs it as a subprocess.
It covers what is invisible from inside the process: every exit code, which
stream each thing lands on, and what two real processes do to one database at
once. Goroutines cannot catch that last one.

## Dependencies

The library — the root package and `internal/{naming,sqlsplit}` — depends on
`jackc/pgx/v5` and nothing else. `reggol` and `envi` are the binary's, not the
library's.

This is not a convention, it is a `depguard` rule, because a promise of this
shape lasts about six weeks unless a machine checks it. Every transitive
dependency of a migration library becomes a transitive dependency of somebody
else's production binary.

## The linter

Pinned to one version in both the `Makefile` and the workflow. A neighbouring
patch analyses differently, and "green on my machine" stops meaning "green in
CI" the moment those drift.

It runs clean and is expected to stay that way. A finding is either a defect or
a rule worth arguing about explicitly — never noise to live with. Version 1 had
five standing `errcheck` findings, which meant every review compared against
five rather than zero.

## Coverage thresholds

`.coverage-thresholds` holds a floor per package, because one number for the
whole repository hides exactly what matters: a fall in the domain compensated by
a rise elsewhere stays green.

Lowering a threshold is a deliberate edit, visible in the diff, with the reason
in the commit message.

The numbers come from `go tool covdata`, and they include the subprocess: `e2e`
builds the binary with `-cover` and hands it a `GOCOVERDIR`, so the commands
that need a database are counted like everything else. Before that, the
`internal/cli` threshold carried an apology instead of a number.

## The `--json` format

Every top-level JSON object carries `"format": <int>`, from `migrator.JSONFormat`.

Adding a field does not change it. Removing one, renaming one, or changing what
one means does — and that bump is a CHANGELOG entry of its own, because a
consumer that cannot tell one shape from another breaks silently, which is the
class of failure the rest of this tool exists to prevent.

## Commits and changelog

A user-visible change gets a CHANGELOG entry, written as prose that says why.

## Releasing

```bash
git tag v2.0.1 && git push origin v2.0.1
```

The tag triggers `build.yml`, which builds five platforms, checks that the built
binary reports a real version — the check version 1 needed and did not have —
and pushes the images.
