// Package migrator applies versioned SQL migrations to PostgreSQL.
//
// The model is three values. A source is a set of migration files read through
// an [io/fs.FS], so os.DirFS, embed.FS and fstest.MapFS all fit and the whole
// parsing half is testable without a database. A plan says what a run would do,
// and answers that question without writing anything. A report says what a run
// did.
//
// # Why a session and not a pool
//
// A [Migrator] is built over a [Connector], which hands it one pinned
// PostgreSQL backend for the whole run. This is not an optimisation. The
// advisory lock that serialises concurrent runs is session-level, and SET
// applies to one backend: taken on one connection and released on another, the
// lock is not released at all, and the timeouts a migration needs never reach
// the statement that needed them. A *pgxpool.Pool satisfies Exec and Begin and
// would compile — and would fail once in a hundred deploys, under load, in a
// way nobody can reproduce. Hence [FromPool], which takes one connection out
// and holds it, rather than an interface the pool happens to satisfy.
//
// # Safety
//
// Every migration runs in its own transaction together with the row that
// records it, so "applied but not recorded" cannot happen. A migration that
// must run outside a transaction — CREATE INDEX CONCURRENTLY, ALTER TYPE ...
// ADD VALUE — says so with a directive, and is recorded in two steps so that a
// crash between them is visible afterwards rather than silent.
//
// Each migration carries a checksum of its file. Editing a file that has
// already been applied is refused, loudly and before anything runs: every
// database that applied the old text will never see the new one.
package migrator
