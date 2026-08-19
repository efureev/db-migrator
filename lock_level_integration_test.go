//go:build integration

package migrator_test

import (
	"errors"
	"strings"
	"testing"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

// TestMaxLockLevelRefusesBeforeAnythingRuns is the property the gate exists
// for. A refusal that happened halfway would be worse than none: the operator
// would have to work out what had already been applied before deciding what to
// do, which is exactly the position the gate is meant to prevent.
func TestMaxLockLevelRefusesBeforeAnythingRuns(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)
	testdb.Exec(t, dsn, `INSERT INTO users (name) SELECT 'u' || g FROM generate_series(1, 300) g`)
	// Without this the planner has no estimate at all — reltuples is -1 on a
	// table nobody has analysed — and the refusal would honestly report that it
	// does not know how large the table is.
	testdb.Exec(t, dsn, `ANALYZE users`)

	files := src(map[string]string{
		"1_add_index.up.sql":   "CREATE INDEX ix_users_name ON users (name);",
		"1_add_index.down.sql": "DROP INDEX ix_users_name;",
		"2_add_column.up.sql":  "ALTER TABLE users ADD COLUMN email text;",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files,
		migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = m.Up(ctx(t))
	if !errors.Is(err, migrator.ErrLockTooHeavy) {
		t.Fatalf("Up = %v, want ErrLockTooHeavy", err)
	}

	var detail *migrator.LockLevelError
	if !errors.As(err, &detail) {
		t.Fatalf("no LockLevelError in %v", err)
	}

	if detail.Version != 1 {
		t.Errorf("refused version %d, want the first one — the gate must stop before it runs", detail.Version)
	}

	if detail.Prediction.Relation != "users" {
		t.Errorf("relation = %q, want users", detail.Prediction.Relation)
	}

	// The row count is what makes the refusal actionable, and it can only come
	// from the server.
	if detail.Prediction.Rows < 0 {
		t.Error("the refusal did not say how large the table is")
	}

	// Nothing ran: the index the first migration would have built is absent.
	if testdb.QueryInt(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = 'ix_users_name'`) != 0 {
		t.Error("the first migration ran despite the refusal")
	}

	if testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'users' AND column_name = 'email'`) != 0 {
		t.Error("the second migration ran despite the refusal")
	}
}

// TestLockAcknowledgedIsPerFile keeps the waiver where the knowledge is. The
// acknowledgement in one migration must not quietly cover the next one.
func TestLockAcknowledgedIsPerFile(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)

	files := src(map[string]string{
		"1_add_index.up.sql": "-- migrator:lock-acknowledged share\n\n" +
			"CREATE INDEX ix_users_name ON users (name);",
		"2_add_column.up.sql": "ALTER TABLE users ADD COLUMN email text;",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files,
		migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = m.Up(ctx(t))

	var detail *migrator.LockLevelError
	if !errors.As(err, &detail) {
		t.Fatalf("Up = %v, want the second migration refused", err)
	}

	if detail.Version != 2 {
		t.Errorf("refused version %d, want 2: the waiver in 1 must not cover 2", detail.Version)
	}
}

// TestLockAcknowledgedMustCoverWhatIsTaken fixes the direction of the
// comparison. Acknowledging a lighter lock than the statement takes is not an
// acknowledgement of that statement, and treating it as one would let a stale
// waiver outlive the SQL it was written for.
func TestLockAcknowledgedMustCoverWhatIsTaken(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)

	files := src(map[string]string{
		"1_widen.up.sql": "-- migrator:lock-acknowledged share\n\n" +
			"ALTER TABLE users ALTER COLUMN name TYPE varchar(50);",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files,
		migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if _, err := m.Up(ctx(t)); !errors.Is(err, migrator.ErrLockTooHeavy) {
		t.Fatalf("Up = %v, want ErrLockTooHeavy: SHARE does not cover ACCESS EXCLUSIVE", err)
	}
}

// TestLockAcknowledgedLetsTheRunProceed is the other half: once the file says
// what it takes, the run goes ahead.
func TestLockAcknowledgedLetsTheRunProceed(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)

	files := src(map[string]string{
		"1_add_column.up.sql": "-- migrator:lock-acknowledged access-exclusive\n\n" +
			"ALTER TABLE users ADD COLUMN email text;",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files,
		migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	rep, err := m.Up(ctx(t))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(rep.Applied) != 1 {
		t.Fatalf("applied %d migrations, want 1", len(rep.Applied))
	}
}

// TestNoPolicyMeansNoGate keeps the default harmless. A tool that started
// refusing migrations it used to run would be a tool people stopped upgrading.
func TestNoPolicyMeansNoGate(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)

	files := src(map[string]string{
		"1_widen.up.sql": "ALTER TABLE users ALTER COLUMN name TYPE text;",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up without a policy: %v", err)
	}
}

// TestPlanPredictsAgainstTheLiveCatalogue checks the half of the prediction
// that cannot be unit tested: the row count and the resolution of an index name
// to its table both come from the server.
func TestPlanPredictsAgainstTheLiveCatalogue(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE users (id bigserial PRIMARY KEY, name text)`)
	testdb.Exec(t, dsn, `INSERT INTO users (name) SELECT 'u' || g FROM generate_series(1, 500) g`)
	testdb.Exec(t, dsn, `CREATE INDEX ix_users_name ON users (name)`)
	testdb.Exec(t, dsn, `ANALYZE users`)

	files := src(map[string]string{
		"1_touch.up.sql": "ALTER TABLE users ADD COLUMN email text;\nDROP INDEX ix_users_name;",
	})

	m, err := migrator.New(migrator.FromDSN(dsn), files)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	plan, err := m.Plan(ctx(t), migrator.DirectionUp, migrator.All())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(plan.Steps) != 1 {
		t.Fatalf("planned %d steps, want 1", len(plan.Steps))
	}

	preds := plan.Steps[0].Predictions
	if len(preds) != 2 {
		t.Fatalf("got %d predictions, want one per statement: %+v", len(preds), preds)
	}

	for _, p := range preds {
		if p.Relation != "users" {
			t.Errorf("statement %d names %q, want users — DROP INDEX must resolve to its table",
				p.Statement, p.Relation)
		}

		if p.Rows < 400 {
			t.Errorf("statement %d reported ~%d rows, want about 500", p.Statement, p.Rows)
		}
	}

	// And the text form has to carry it, since that is what a person reads.
	var b strings.Builder
	if err := plan.Text(&b); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(b.String(), "ACCESS EXCLUSIVE on users") {
		t.Errorf("the rendered plan does not name the lock:\n%s", b.String())
	}
}
