//go:build integration

package pglock_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/efureev/db-migrator/v2/internal/pglock"
	"github.com/efureev/db-migrator/v2/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// TestPredictionsMatchTheServer is the test that makes this package worth
// having.
//
// A rule table checked only against a corpus written from the same reading of
// the documentation proves that the author was consistent, not that the author
// was right. Here every case runs the real statement against a real server,
// reads pg_locks to see what the server actually took, and compares that with
// the prediction. When PostgreSQL disagrees, PostgreSQL is correct and the
// table is wrong.
//
// Each case runs inside a transaction that is rolled back, which is what makes
// it possible to read the locks while they are still held. Statements that
// cannot run inside a transaction — anything CONCURRENTLY — are therefore not
// covered here; see TestConcurrentFormsAreWeakerThanTheirPlainForms.
func TestPredictionsMatchTheServer(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	version := testdb.ServerVersion(t, dsn)

	fixture(t, dsn)

	cases := []struct {
		name     string
		sql      string
		relation string
	}{
		{"add a plain column", "ALTER TABLE users ADD COLUMN nickname text", "users"},
		{"add a column with a constant default", "ALTER TABLE users ADD COLUMN score int DEFAULT 0", "users"},
		{"add a column with a volatile default", "ALTER TABLE users ADD COLUMN k uuid DEFAULT gen_random_uuid()", "users"},
		{"drop a column", "ALTER TABLE users DROP COLUMN legacy", "users"},
		{"change a column type", "ALTER TABLE users ALTER COLUMN name TYPE varchar(200)", "users"},
		{"set not null", "ALTER TABLE users ALTER COLUMN name SET NOT NULL", "users"},
		{"drop not null", "ALTER TABLE users ALTER COLUMN name DROP NOT NULL", "users"},
		{"set a default", "ALTER TABLE users ALTER COLUMN name SET DEFAULT 'x'", "users"},
		{"set statistics", "ALTER TABLE users ALTER COLUMN name SET STATISTICS 500", "users"},
		{"add a check constraint", "ALTER TABLE orders ADD CONSTRAINT positive CHECK (total > 0)", "orders"},
		{"add a check constraint not valid", "ALTER TABLE orders ADD CONSTRAINT positive2 CHECK (total > 0) NOT VALID", "orders"},
		{"validate a constraint", "ALTER TABLE orders VALIDATE CONSTRAINT preexisting", "orders"},
		{"add a foreign key, referencing side", "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id)", "orders"},
		{"add a foreign key, referenced side", "ALTER TABLE orders ADD CONSTRAINT fk2 FOREIGN KEY (user_id) REFERENCES users (id)", "users"},
		{"add a unique constraint", "ALTER TABLE users ADD CONSTRAINT u UNIQUE (email)", "users"},
		{"rename a column", "ALTER TABLE users RENAME COLUMN name TO full_name", "users"},
		{"set unlogged", "ALTER TABLE users SET UNLOGGED", "users"},
		{"create an index", "CREATE INDEX ix_users_email ON users (email)", "users"},
		{"drop an index", "DROP INDEX ix_preexisting", "users"},
		{"truncate", "TRUNCATE orders", "orders"},
		{"insert", "INSERT INTO users (name) VALUES ('a')", "users"},
		{"select", "SELECT count(*) FROM users", "users"},
		{"locking read", "SELECT id FROM users FOR UPDATE", "users"},
		{"lock explicitly", "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE", "users"},
		{"analyze", "ANALYZE users", "users"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preds := pglock.Analyze([]string{tc.sql}, pglock.Options{ServerVersion: version})

			// Enrichment runs before the statement, exactly as it does in a
			// plan: it is what turns the index DROP INDEX names into the table
			// whose lock people care about, and after the drop the index is
			// gone from the catalogue.
			enrich(t, dsn, preds)

			want := strongestFor(preds, tc.relation)

			got := observe(t, dsn, tc.sql, tc.relation)

			if want == pglock.LevelUnknown {
				t.Fatalf("no prediction for %s on %q\nstatement: %s", tc.relation, tc.name, tc.sql)
			}

			if got != want {
				t.Errorf("the rule table is wrong, not the server.\n"+
					"statement: %s\nrelation:  %s\npredicted: %s\nobserved:  %s",
					tc.sql, tc.relation, want, got)
			}
		})
	}
}

// TestConcurrentFormsAreWeakerThanTheirPlainForms covers what the oracle above
// cannot: CONCURRENTLY refuses to run inside a transaction, so its locks cannot
// be read while they are held without racing the statement.
//
// The property that matters is still checkable, and it is the one people rely
// on: the CONCURRENTLY form must predict a lock that does not block writes,
// while the plain form must predict one that does. A table that got this
// backwards would be worse than no table.
func TestConcurrentFormsAreWeakerThanTheirPlainForms(t *testing.T) {
	t.Parallel()

	pairs := []struct{ plain, concurrent string }{
		{"CREATE INDEX i ON users (email)", "CREATE INDEX CONCURRENTLY i ON users (email)"},
		{"DROP INDEX i", "DROP INDEX CONCURRENTLY i"},
		{"REINDEX TABLE users", "REINDEX TABLE CONCURRENTLY users"},
		{
			"REFRESH MATERIALIZED VIEW mv",
			"REFRESH MATERIALIZED VIEW CONCURRENTLY mv",
		},
	}

	for _, p := range pairs {
		plain := strongestAnywhere(pglock.Analyze([]string{p.plain}, pglock.Options{ServerVersion: 170000}))
		conc := strongestAnywhere(pglock.Analyze([]string{p.concurrent}, pglock.Options{ServerVersion: 170000}))

		if conc >= plain {
			t.Errorf("%q predicts %s, which is not weaker than %q at %s",
				p.concurrent, conc, p.plain, plain)
		}
	}
}

// enrich resolves the predictions against the catalogue, in place.
func enrich(t *testing.T, dsn string, preds []pglock.Prediction) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := pglock.Enrich(ctx, testdb.Connect(t, dsn), preds); err != nil {
		t.Fatalf("enrich: %v", err)
	}
}

// observe runs sql in a transaction and reports the strongest relation lock the
// server took on relation, then rolls back.
func observe(t *testing.T, dsn, sql, relation string) pglock.Level {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	conn := testdb.Connect(t, dsn)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Fatalf("run %q: %v", sql, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT l.mode
		  FROM pg_locks l
		  JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_backend_pid()
		   AND l.locktype = 'relation'
		   AND l.granted
		   AND c.relname = $1`, relation)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}

	modes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}

	strongest := pglock.LevelUnknown

	for _, mode := range modes {
		level, ok := levelFromPgMode(mode)
		if !ok {
			t.Fatalf("pg_locks reported a mode this package does not know: %q", mode)
		}

		if level > strongest {
			strongest = level
		}
	}

	if strongest == pglock.LevelUnknown {
		t.Fatalf("the server took no relation lock on %s while running %q", relation, sql)
	}

	return strongest
}

// levelFromPgMode inverts Level.PgMode, so that what the server reports can be
// compared with what was predicted.
func levelFromPgMode(mode string) (pglock.Level, bool) {
	for _, l := range pglock.Levels() {
		if strings.EqualFold(l.PgMode(), mode) {
			return l, true
		}
	}

	return pglock.LevelUnknown, false
}

// strongestFor reports the predicted level for one relation.
func strongestFor(preds []pglock.Prediction, relation string) pglock.Level {
	out := pglock.LevelUnknown

	for _, p := range preds {
		if p.Relation == relation && p.Level > out {
			out = p.Level
		}
	}

	return out
}

// strongestAnywhere reports the strongest predicted level over every relation.
func strongestAnywhere(preds []pglock.Prediction) pglock.Level {
	out := pglock.LevelUnknown

	for _, p := range preds {
		if p.Level > out {
			out = p.Level
		}
	}

	return out
}

// fixture builds the two tables every case above is written against.
func fixture(t *testing.T, dsn string) {
	t.Helper()

	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE users (
			id     bigserial PRIMARY KEY,
			name   text NOT NULL,
			email  text,
			legacy text
		)`,
		`CREATE TABLE orders (
			id      bigserial PRIMARY KEY,
			user_id bigint,
			total   numeric NOT NULL DEFAULT 0,
			CONSTRAINT preexisting CHECK (total >= 0) NOT VALID
		)`,
		`CREATE INDEX ix_preexisting ON users (email)`,
		`CREATE MATERIALIZED VIEW mv AS SELECT count(*) AS n FROM users`,
		`INSERT INTO users (name, email) SELECT 'u' || g, 'e' || g FROM generate_series(1, 200) g`,
	} {
		testdb.Exec(t, dsn, stmt)
	}
}
