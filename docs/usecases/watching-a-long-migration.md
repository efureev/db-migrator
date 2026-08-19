# Watching a long migration

*[По-русски](watching-a-long-migration.ru.md)*

**Use this when** a migration has been running for twenty minutes and the only
honest answer to "is it stuck?" is currently "no idea".

PostgreSQL publishes the progress of its own long operations and almost nobody
reads it. Every `--progress-interval` — 30 seconds by default — a second
connection reads those views for the backend running the migration and writes
one line.

## An index build

```console
$ migrator up --progress-interval 3s
INF migrator: migration in progress command=CREATE INDEX CONCURRENTLY direction=up elapsed=3s name=add_email_idx object=users_email_idx percent=32 phase=building index: scanning table pid=175181 progress=15 of 46 blocks relation=users source=create_index version=20260901120000
INF migrator: migration in progress command=CREATE INDEX CONCURRENTLY direction=up elapsed=6s name=add_email_idx object=users_email_idx percent=67 phase=building index: scanning table pid=175181 progress=31 of 46 blocks relation=users source=create_index version=20260901120000
INF migrator: migration applied direction=up name=add_email_idx took=8.762s version=20260901120000
  applied    20260901120000_add_email_idx  8.762s

  Done. 1 applied in 9.054s. Current version 20260901120000.
```

`pid` is there because it is what you would hand to `pg_terminate_backend`.
`percent` is a number so you can alert on it. `elapsed` is measured here and says
how long *this migration* has been applying.

A view covers each of `CREATE INDEX`, `VACUUM`, `ANALYZE`, `CLUSTER` and `COPY`.

## Everything else: a pulse

A table rewrite, a backfill `UPDATE`, a wait on a lock — none of these appear in
any progress view, and between them they are the majority of slow migrations. So
`pg_stat_activity` is read on every poll too, and when no progress view had a
row the line reports that instead:

```console
$ migrator up --progress-interval 2s
INF migrator: migration still running direction=up elapsed=2s name=backfill_tz pid=175285 state=active statement_age=2s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration still running direction=up elapsed=4s name=backfill_tz pid=175285 state=active statement_age=4s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration still running direction=up elapsed=6s name=backfill_tz pid=175285 state=active statement_age=6s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration applied direction=up name=backfill_tz took=11.94s version=20260901140000
  applied    20260901140000_backfill_tz  11.94s

  Done. 1 applied in 11.966s. Current version 20260901140000.
```

Two different messages, not one with a field to tell them apart: a consumer
filtering the JSON on `msg` wants "the index is 30% built" and "it is alive and
has said nothing for fourteen minutes" to be different events.

`wait_event` is the thing to look at. `Timeout:PgSleep` above is the fixture
sleeping; `Lock:relation` means the migration is not slow at all, it is blocked —
see [Making a schema change safely](safe-schema-change.md) and `lock_timeout`.
`statement_age` comes from the server and says how long the *current statement*
has been running; on a no-transaction migration of several statements it and
`elapsed` diverge, which is why they are two fields.

## Who is holding it up

From another terminal, while the run is in flight:

```console
$ migrator locks
  Lock     1835496052/142344567  (derived from public.schema_migrations)

  pid 175285  migrator  on uc_pulse  active for 6s
    UPDATE users SET tz = slow(email) WHERE tz IS NULL;

  To end one:  SELECT pg_terminate_backend(175285);
```

`locks` takes no lock and writes nothing. Holders are deliberately **not**
filtered by database: advisory locks are cluster-wide, so a holder connected to
a neighbouring database on the same server is exactly the case that otherwise
costs twenty minutes to explain.

## Turning it off, and what it costs

```bash
migrator up --progress-interval 0        # never
migrator up --progress-interval 5s       # more often
export MIGRATOR_PROGRESS_INTERVAL=10s    # once, for a pipeline
```

Values below 100ms are raised to 100ms. The lines go to **stderr** and become
JSON along with everything else under `--json`, so a consumer piping stdout into
`jq` never sees them. `--quiet` and `--log-level error` silence them, because
they are `INFO`.

Reporting costs one extra connection for the duration of the run, and it is
never opened when it would be pointless:

- `--progress-interval 0`;
- no logger was given (as a library) — nothing would read the lines;
- the connector cannot supply a second connection.

## As a library

```go
m, err := migrator.New(migrator.FromPool(pool), src,
	migrator.WithLogger(log),
	migrator.WithProgressInterval(15*time.Second),
)
```

`FromDSN` and `FromPool` supply the second connection themselves and reporting is
on by default. `FromConn` hands back the connection its caller already owns, and
that one is busy running the migration — so there is nothing to poll from.

That is **detected, not assumed**: both connections are asked for
`pg_backend_pid()` before the first statement runs, and if the answer names one
backend, reporting is switched off with one line at debug level. A hand-written
`Connector` wrapping a single connection is caught the same way.

To poll over a connection of your own — a replica, a different role, a separate
pool — pass it:

```go
migrator.WithProgress(migrator.FromDSN(monitorDSN))
```

That role needs to be able to see the backend: the same role as the run, or one
with `pg_read_all_stats`. If it cannot, `pg_stat_activity` returns a row with
every interesting column blank, and reporting switches itself off with a line
saying so rather than printing empty lines forever.

Nothing about progress can fail a run. A pool with one connection in it, a server
that will not answer, a role that may not look: each of them means no reporting
and a line at debug level.

## Next

- [Rolling back, and repairing the journal](rollback-and-repair.md) — when the
  long one dies half way
- [Making a schema change safely](safe-schema-change.md)
