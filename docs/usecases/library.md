# Using it as a library

*[По-русски](library.ru.md)*

**Use this when** a service should apply its own migrations at startup, with the
files travelling inside the binary.

The whole model is three values. A **source** is a set of migration files read
through an `io/fs.FS`, so `os.DirFS`, `embed.FS` and `fstest.MapFS` all fit — and
the parsing half is testable with no database at all. A **plan** says what a run
would do. A **report** says what a run did.

## The shape of it

A runnable version of this lives in [`examples/embed`](../../examples/embed).

```go
//go:embed migrations/*.sql
var embedded embed.FS

func migrate(ctx context.Context, log *slog.Logger) error {
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	// The embedded FS is rooted above the files; fs.Sub points at the directory
	// that holds them, which is what Load expects.
	src, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return err
	}

	// No WithAllowDown and no WithAllowWipe: a service has no business rolling
	// back or erasing its own schema, and leaving them out makes that
	// impossible rather than merely discouraged.
	m, err := migrator.New(migrator.FromPool(pool), src,
		migrator.WithLogger(log),
		migrator.WithMigratorTag("shop-api/1.4.2"),
	)
	if err != nil {
		return err
	}

	report, err := m.Up(ctx)
	if err != nil {
		return err
	}

	log.Info("migrations applied", "report", report)

	return nil
}
```

```console
$ DATABASE_URL=... go run ./examples/embed
level=INFO msg="migrator: migration applied" version=1 name=create_users direction=up took=4ms
level=INFO msg="migrator: migration applied" version=2 name=index_users_email direction=up took=3ms
level=INFO msg="migrations applied" report.direction=up report.applied=2 report.duration=46.575458ms report.current=2
```

Run it again and it does nothing, quietly:

```console
$ DATABASE_URL=... go run ./examples/embed
level=INFO msg="migrations applied" report.direction=up report.applied=0 report.duration=29.087417ms report.current=0
```

`Report` implements `slog.LogValuer`, which is why the whole run lands as one
grouped field.

## Why a Connector and not a pool

`New` takes a `Connector`, which hands it **one pinned PostgreSQL backend for the
whole run**. This is not an optimisation.

The advisory lock that serialises concurrent runs is session-level, and `SET`
applies to one backend. Taken on one connection and released on another, the lock
is not released at all, and the timeouts a migration needs never reach the
statement that needed them. A `*pgxpool.Pool` satisfies `Exec` and `Begin` and
would compile — and would fail once in a hundred deploys, under load, in a way
nobody can reproduce.

Hence three constructors that are explicit about it:

| Constructor | What it does | Progress reporting |
|---|---|---|
| `FromDSN(dsn)` | opens a connection per run | on by default |
| `FromPool(pool)` | takes one connection out and holds it | on by default |
| `FromConn(conn)` | uses the connection you own | unavailable — see below |

`FromConn` hands back the connection its caller owns, and that one is busy
running the migration, so there is no second connection to poll
`pg_stat_progress_*` from. That is detected rather than assumed, and said once at
debug level. See [Watching a long migration](watching-a-long-migration.md).

`Conn` is exported so that you can wrap a connection — for tracing, for metrics —
and still satisfy `Connector`. That is a legitimate thing to want, which is why
the type stayed public when the rest of the surface was audited down.

## The dependency tree

**The library depends on `jackc/pgx/v5` and nothing else.** Not on a logger, not
on a config library, not on a CLI framework. Every transitive dependency of a
migration tool becomes a transitive dependency of somebody's production binary,
which is why a linter enforces this rather than a review.

Logging is `log/slog` from the standard library, and the default logger discards
everything: a library that logs where it was not asked to is a library that
corrupts somebody's JSON on stdout.

## Options worth knowing

```go
m, err := migrator.New(migrator.FromPool(pool), src,
	migrator.WithSchema("app"),              // journal here, and seeds @schema@
	migrator.WithTable("schema_migrations"),
	migrator.WithLogger(log),
	migrator.WithMigratorTag("shop-api/1.4.2"), // recorded in the journal
	migrator.WithAppliedBy("deploy-bot"),       // recorded in the journal
	migrator.WithStatementTimeout(30*time.Minute),
	migrator.WithLockTimeout(5*time.Second),
	migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive),
	migrator.WithProgressInterval(15*time.Second),
	migrator.WithPlaceholders(map[string]string{"@tablespace@": "fast_ssd"}),
)
```

`Option` is a sealed interface — its only method is unexported — so the
configuration struct stays entirely internal and therefore free to change. A
`type Option func(*Config)` with an exported `Config` would have frozen it under
SemVer on day one.

Deliberately absent unless you ask: `WithAllowDown`, `WithAllowWipe`,
`WithForceWipe`. Leaving them out of a service makes rolling back impossible
rather than merely discouraged.

## Deciding without doing

```go
plan, err := m.Plan(ctx, migrator.DirectionUp, migrator.All())
```

`Plan` opens its own session, takes no lock and writes nothing. Each `Step`
carries the statements as they would be sent and a `LockPrediction` per statement
— which lock, whether it rewrites, whether it scans, how many rows are in the
table. `Plan.Text(w)` and `Plan.JSON(w)` render it. See
[Making a schema change safely](safe-schema-change.md).

`Status` and `Version` are read-only in the same way, and neither requires the
create privilege — so both stay usable against a replica and with a read-only
role.

## Errors are sentinels

Match with `errors.Is`, never on the message:

```go
switch {
case errors.Is(err, migrator.ErrLockTimeout):
	// another deploy is migrating; retrying is correct
case errors.Is(err, migrator.ErrChecksumMismatch):
	// a released file was edited; a person has to look
case errors.Is(err, migrator.ErrForeignJournal):
	// somebody else's journal is in the way — see Adopt
}
```

For detail, `errors.As` into the typed ones: `*MigrationError` carries the
version, the file, the statement number and the line; `*LockLevelError` carries
the prediction that was refused.

Everything the server says passes through redaction before it can be logged, so a
DSN with a password in it does not end up in somebody's log aggregator.

## Testing your own migrations

The source half needs no database:

```go
src := fstest.MapFS{
	"1_create_users.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (id int);")},
}

m, err := migrator.New(conn, src)   // Load parses and validates here
```

For the half that does need one, give each test its **own database** rather than
its own schema, and remember that advisory locks are cluster-wide: two tests
using the default `public.schema_migrations` derive the same lock id and will
serialise against each other whether you meant them to or not.

## Next

- [One set of migrations, many schemas](many-schemas.md)
- [Watching a long migration](watching-a-long-migration.md)
