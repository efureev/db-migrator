//go:build integration

package migrator_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

func TestDownRequiresTheFlag(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m := newMigrator(t, dsn, twoTables)
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Without WithAllowDown the method refuses before opening a transaction —
	// or, here, before touching the database at all.
	_, err := m.Down(ctx(t), migrator.Steps(1))
	if !errors.Is(err, migrator.ErrDownNotAllowed) {
		t.Fatalf("Down error = %v, want %v", err, migrator.ErrDownNotAllowed)
	}

	if !testdb.TableExists(t, dsn, "public", "b") {
		t.Error("the refused rollback ran anyway")
	}
}

func TestDown(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m := newMigrator(t, dsn, twoTables, migrator.WithAllowDown())
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rep, err := m.Down(ctx(t), migrator.Steps(1))
	if err != nil {
		t.Fatalf("Down: %v", err)
	}

	if rep.Len() != 1 {
		t.Fatalf("rolled back %d migrations, want 1", rep.Len())
	}

	if testdb.TableExists(t, dsn, "public", "b") {
		t.Error("the rolled-back migration's table is still there")
	}

	if !testdb.TableExists(t, dsn, "public", "a") {
		t.Error("Steps(1) rolled back more than one migration")
	}

	// The row is updated, not deleted: what ran is worth keeping.
	rows := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM public.schema_migrations WHERE version = 2 AND rolled_back_at IS NOT NULL`)
	if rows != 1 {
		t.Error("the rollback deleted the row instead of marking it")
	}

	status, err := m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if status.Current() != 1 {
		t.Errorf("current = %d, want 1", status.Current())
	}

	// A rolled-back migration is pending again, and applying it works.
	if len(status.Pending()) != 1 {
		t.Fatalf("pending = %d, want 1", len(status.Pending()))
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}

	if !testdb.TableExists(t, dsn, "public", "b") {
		t.Error("re-applying the rolled-back migration did nothing")
	}
}

func TestDownWithoutADownFileIsRefusedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	files := map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);", "1_a.down.sql": "DROP TABLE a;",
		"2_b.up.sql": "CREATE TABLE b (id int);", // no down file
		"3_c.up.sql": "CREATE TABLE c (id int);", "3_c.down.sql": "DROP TABLE c;",
	}

	m := newMigrator(t, dsn, files, migrator.WithAllowDown())
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	_, err := m.Down(ctx(t), migrator.All())
	if !errors.Is(err, migrator.ErrMissingDownFile) {
		t.Fatalf("Down error = %v, want %v", err, migrator.ErrMissingDownFile)
	}

	// Nothing ran: the gap is found while planning, so a rollback either goes
	// the whole way or does not start. Rolling back version 3 and stopping at
	// version 2 would be the worst outcome available.
	for _, name := range []string{"a", "b", "c"} {
		if !testdb.TableExists(t, dsn, "public", name) {
			t.Errorf("table %q was dropped by a rollback that should not have started", name)
		}
	}
}

func TestRedo(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m := newMigrator(t, dsn, twoTables, migrator.WithAllowDown())
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	testdb.Exec(t, dsn, `INSERT INTO b VALUES (1)`)

	rep, err := m.Redo(ctx(t), 1)
	if err != nil {
		t.Fatalf("Redo: %v", err)
	}

	// Down then up over the same version.
	if rep.Len() != 2 {
		t.Fatalf("Redo touched %d migrations, want 2", rep.Len())
	}

	if !rep.OneTransaction {
		t.Error("a redo over transactional migrations should have held one transaction")
	}

	if !testdb.TableExists(t, dsn, "public", "b") {
		t.Fatal("the table was not recreated")
	}

	if got := testdb.QueryInt(t, dsn, `SELECT count(*) FROM b`); got != 0 {
		t.Errorf("the table holds %d rows after a redo, want 0", got)
	}

	status, err := m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if status.Current() != 2 || len(status.Pending()) != 0 {
		t.Errorf("after redo: current=%d pending=%d", status.Current(), len(status.Pending()))
	}
}

func TestProductionRefusesEverythingDestructive(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	// Every permissive option is set. Production overrides all of them: this is
	// the refusal no flag reaches.
	m := newMigrator(t, dsn, twoTables,
		migrator.WithAllowDown(), migrator.WithAllowWipe(),
		migrator.WithEnvironment(migrator.EnvProduction))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up must still work in production: %v", err)
	}

	for name, call := range map[string]func() error{
		"Down": func() error { _, err := m.Down(ctx(t), migrator.Steps(1)); return err },
		"Redo": func() error { _, err := m.Redo(ctx(t), 1); return err },
		"Wipe": func() error { _, err := m.Wipe(ctx(t), migrator.Confirm("anything")); return err },
	} {
		if err := call(); !errors.Is(err, migrator.ErrProductionGuard) {
			t.Errorf("%s error = %v, want %v", name, err, migrator.ErrProductionGuard)
		}
	}

	// Nothing was touched.
	for _, name := range []string{"a", "b"} {
		if !testdb.TableExists(t, dsn, "public", name) {
			t.Errorf("table %q went missing despite the production guard", name)
		}
	}
}

func TestWipeRequiresTheFlag(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m := newMigrator(t, dsn, twoTables)
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	_, err := m.Wipe(ctx(t), migrator.Confirm("whatever"))
	if !errors.Is(err, migrator.ErrWipeRefused) {
		t.Fatalf("Wipe error = %v, want %v", err, migrator.ErrWipeRefused)
	}

	if !testdb.TableExists(t, dsn, "public", "a") {
		t.Error("the refused wipe ran anyway")
	}
}

func TestWipeNeedsTheRightDatabaseName(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m := newMigrator(t, dsn, twoTables, migrator.WithAllowWipe())
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// A confirmation naming the wrong database is the case this guard exists
	// for: a command copy-pasted with somebody else's PGDATABASE in the
	// environment names the database the operator meant, not the one connected.
	_, err := m.Wipe(ctx(t), migrator.Confirm("some_other_database"))
	if !errors.Is(err, migrator.ErrNotConfirmed) {
		t.Fatalf("Wipe error = %v, want %v", err, migrator.ErrNotConfirmed)
	}

	if !testdb.TableExists(t, dsn, "public", "a") {
		t.Error("a wipe with a mismatched confirmation ran")
	}

	// No confirmation at all is also a refusal, not a prompt and not a default.
	if _, err := m.Wipe(ctx(t), migrator.Confirmation{}); !errors.Is(err, migrator.ErrNotConfirmed) {
		t.Fatalf("unconfirmed Wipe error = %v, want %v", err, migrator.ErrNotConfirmed)
	}
}

func TestWipe(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	database := databaseOf(t, dsn)

	testdb.Exec(t, dsn, `CREATE EXTENSION IF NOT EXISTS pg_trgm`)

	m := newMigrator(t, dsn, map[string]string{
		"1_things.up.sql": `
			CREATE TYPE mood AS ENUM ('ok', 'bad');
			CREATE TABLE things (id bigserial PRIMARY KEY, how mood);
			CREATE VIEW good_things AS SELECT * FROM things WHERE how = 'ok';
			CREATE FUNCTION count_things() RETURNS bigint LANGUAGE sql AS $$
			  SELECT count(*) FROM things;
			$$;`,
	}, migrator.WithAllowWipe())

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rep, err := m.Wipe(ctx(t), migrator.Confirm(database))
	if err != nil {
		t.Fatalf("Wipe: %v", err)
	}

	if len(rep.Dropped) == 0 {
		t.Fatal("Wipe dropped nothing")
	}

	for _, name := range []string{"things", "good_things", "schema_migrations"} {
		if testdb.TableExists(t, dsn, "public", name) {
			t.Errorf("%q survived the wipe", name)
		}
	}

	if n := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		  WHERE n.nspname = 'public' AND t.typname = 'mood'`); n != 0 {
		t.Error("the enum type survived the wipe")
	}

	if n := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname = 'public' AND p.proname = 'count_things'`); n != 0 {
		t.Error("the function survived the wipe")
	}

	// The schema itself stays, with its privileges untouched: recreating it by
	// hand would change them, because PUBLIC lost CREATE on public in PG 15.
	if n := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM pg_namespace WHERE nspname = 'public'`); n != 1 {
		t.Error("the schema itself was dropped")
	}

	// The extension stays. DROP SCHEMA CASCADE would have taken it, and the
	// next migration calling one of its functions would fail.
	if n := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_trgm'`); n != 1 {
		t.Error("the extension was dropped by the wipe")
	}

	// After a wipe the next run starts from nothing.
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up after Wipe: %v", err)
	}

	if !testdb.TableExists(t, dsn, "public", "things") {
		t.Error("the migration did not re-run after the wipe")
	}
}

func TestWipeRefusesAProductionLookingName(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	// The database this test owns is called mig_<random>; renaming is not
	// available mid-connection, so the pattern is pointed at the name instead.
	// The property under test is that a name matching the pattern is refused.
	m := newMigrator(t, dsn, twoTables,
		migrator.WithAllowWipe(), migrator.WithWipeProtectPattern(`^mig_`))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	_, err := m.Wipe(ctx(t), migrator.Confirm(databaseOf(t, dsn)))
	if !errors.Is(err, migrator.ErrWipeRefused) {
		t.Fatalf("Wipe error = %v, want %v", err, migrator.ErrWipeRefused)
	}

	if !strings.Contains(err.Error(), "^mig_") {
		t.Errorf("error %v does not name the pattern that refused it", err)
	}
}

func TestWipeRefusesASystemSchema(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	m, err := migrator.New(migrator.FromDSN(dsn), src(twoTables),
		migrator.WithAllowWipe(), migrator.WithSchema("pg_catalog"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Wipe(ctx(t), migrator.Confirm(databaseOf(t, dsn)))
	if !errors.Is(err, migrator.ErrWipeRefused) {
		t.Fatalf("Wipe error = %v, want %v", err, migrator.ErrWipeRefused)
	}
}

// databaseOf reports the database a DSN points at, as the server sees it.
func databaseOf(t *testing.T, dsn string) string {
	t.Helper()

	conn := testdb.Connect(t, dsn)

	var name string
	if err := conn.QueryRow(ctx(t), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("current_database: %v", err)
	}

	return name
}

// TestFromPool is the path go-outbox uses. The property that matters is that
// one connection is held for the whole run — a pool that routed consecutive
// statements to different backends would break the advisory lock silently, and
// the pin check would catch it here.
func TestFromPool(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	// A pool with a statement_timeout of its own, which is the situation the
	// run-scoped SET exists for: without overriding it, a long migration would
	// be killed by a setting it never chose.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "250"
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx(t), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	t.Cleanup(pool.Close)

	m, err := migrator.New(migrator.FromPool(pool), src(map[string]string{
		// Longer than the pool's 250ms statement_timeout: this only passes if
		// the run overrode it.
		"1_slow.up.sql": "CREATE TABLE slow (id int);\nSELECT pg_sleep(0.6);",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up over a pool: %v", err)
	}

	if !testdb.TableExists(t, dsn, "public", "slow") {
		t.Error("the migration did not run")
	}

	// The connection went back to the pool with the pool's own settings, not
	// the run's: otherwise an hour-long timeout outlives the migration that
	// needed it.
	conn, err := pool.Acquire(ctx(t))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()

	var timeout string
	if err := conn.QueryRow(ctx(t), `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}

	if timeout != "250ms" {
		t.Errorf("statement_timeout is %q after the run, want the pool's 250ms", timeout)
	}
}

// TestNilConnectors: a nil pool or connection is a bug in the calling code and
// must be reported as one rather than panicking three statements later.
func TestNilConnectors(t *testing.T) {
	t.Parallel()

	files := src(map[string]string{"1_a.up.sql": "SELECT 1;"})

	for name, c := range map[string]migrator.Connector{
		"nil conn": migrator.FromConn(nil),
		"nil pool": migrator.FromPool(nil),
	} {
		m, err := migrator.New(c, files)
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}

		if _, err := m.Up(ctx(t)); err == nil {
			t.Errorf("%s: Up succeeded", name)
		}
	}
}

// TestValidateReportsEverythingAtOnce: fixing a drifted deployment one restart
// at a time is how a five-minute job becomes an hour.
func TestValidateReportsEverythingAtOnce(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	first := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);", "1_a.down.sql": "DROP TABLE a;",
		"2_b.up.sql": "CREATE TABLE b (id int);", "2_b.down.sql": "DROP TABLE b;",
	})

	if _, err := first.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Now the source disagrees in three ways at once: version 1 was edited,
	// version 2 no longer exists, and version 3 is new.
	drifted := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id bigint);", "1_a.down.sql": "DROP TABLE a;",
		"3_c.up.sql": "CREATE TABLE c (id int);",
	})

	report, err := drifted.Validate(ctx(t))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if report.OK() {
		t.Fatal("Validate found no problems in a thoroughly drifted database")
	}

	kinds := map[migrator.ProblemKind]bool{}
	for _, p := range report.Problems() {
		kinds[p.Kind] = true
	}

	for _, want := range []migrator.ProblemKind{
		migrator.ProblemChecksum, migrator.ProblemMissing,
		migrator.ProblemPending, migrator.ProblemNoDownFile,
	} {
		if !kinds[want] {
			t.Errorf("Validate did not report %v: %v", want, report.Problems())
		}
	}

	// The joined error matches the sentinels, so a caller can branch without
	// parsing text.
	err = report.Err()
	for _, want := range []error{migrator.ErrChecksumMismatch, migrator.ErrMissingMigration} {
		if !errors.Is(err, want) {
			t.Errorf("Err does not report %v: %v", want, err)
		}
	}

	// A pending migration is a warning, not an error: having migrations waiting
	// is the normal state of a repository between deploys.
	clean := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);", "1_a.down.sql": "DROP TABLE a;",
		"2_b.up.sql": "CREATE TABLE b (id int);", "2_b.down.sql": "DROP TABLE b;",
		"3_c.up.sql": "CREATE TABLE c (id int);", "3_c.down.sql": "DROP TABLE c;",
	})

	ok, err := clean.Validate(ctx(t))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if !ok.OK() {
		t.Errorf("a pending migration was reported as an error: %v", ok.Problems())
	}
}

// TestRepairRehash is the escape hatch that exists so that no flag on up has to
// be. It edits bookkeeping, never the schema, and leaves a trace.
func TestRepairRehash(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	before := newMigrator(t, dsn, map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"})
	if _, err := before.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The same schema, spelled differently: exactly the cosmetic edit rehash is
	// for.
	after := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (\n  id int\n);",
	})

	if _, err := after.Up(ctx(t)); !errors.Is(err, migrator.ErrChecksumMismatch) {
		t.Fatalf("Up error = %v, want %v", err, migrator.ErrChecksumMismatch)
	}

	rep, err := after.Repair(ctx(t), migrator.RepairChecksum(1))
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}

	if len(rep.Repairs) != 1 || rep.Repairs[0].Action != "rehash" {
		t.Fatalf("Repair did %v", rep.Repairs)
	}

	if rep.Repairs[0].Before == rep.Repairs[0].After {
		t.Error("the repair reported no change")
	}

	// The trace: what it was, and when it was rewritten. Repairing metadata
	// must not erase the history of what actually ran.
	traced := testdb.QueryInt(t, dsn, `
		SELECT count(*) FROM public.schema_migrations
		 WHERE version = 1 AND checksum_previous IS NOT NULL AND checksum_repaired_at IS NOT NULL`)
	if traced != 1 {
		t.Error("the repair left no trace in the row")
	}

	if _, err := after.Up(ctx(t)); err != nil {
		t.Fatalf("Up after repair: %v", err)
	}
}

func TestRepairCompleteAndDiscard(t *testing.T) {
	t.Parallel()

	t.Run("complete", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		m := newMigrator(t, dsn, map[string]string{
			"1_a.up.sql": "-- migrator:no-transaction\nCREATE TABLE a (id int);\nSELECT nope();",
		})

		if _, err := m.Up(ctx(t)); err == nil {
			t.Fatal("Up succeeded on an invalid migration")
		}

		// A person has looked and confirmed the schema is right.
		if _, err := m.Repair(ctx(t), migrator.RepairComplete(1)); err != nil {
			t.Fatalf("Repair: %v", err)
		}

		if _, err := m.Up(ctx(t)); err != nil {
			t.Fatalf("Up after complete: %v", err)
		}
	})

	t.Run("discard makes it run again", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		m := newMigrator(t, dsn, map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"})
		if _, err := m.Up(ctx(t)); err != nil {
			t.Fatalf("Up: %v", err)
		}

		testdb.Exec(t, dsn, `DROP TABLE a`)

		if _, err := m.Repair(ctx(t), migrator.RepairDiscard(1)); err != nil {
			t.Fatalf("Repair: %v", err)
		}

		if _, err := m.Up(ctx(t)); err != nil {
			t.Fatalf("Up after discard: %v", err)
		}

		if !testdb.TableExists(t, dsn, "public", "a") {
			t.Error("the discarded migration did not run again")
		}
	})

	t.Run("prune drops rows with no file", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		full := newMigrator(t, dsn, map[string]string{
			"1_a.up.sql": "CREATE TABLE a (id int);",
			"2_b.up.sql": "CREATE TABLE b (id int);",
		})

		if _, err := full.Up(ctx(t)); err != nil {
			t.Fatalf("Up: %v", err)
		}

		trimmed := newMigrator(t, dsn, map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"})

		if _, err := trimmed.Up(ctx(t)); !errors.Is(err, migrator.ErrMissingMigration) {
			t.Fatalf("Up error = %v, want %v", err, migrator.ErrMissingMigration)
		}

		rep, err := trimmed.Repair(ctx(t), migrator.RepairPrune())
		if err != nil {
			t.Fatalf("Repair: %v", err)
		}

		if len(rep.Repairs) != 1 || rep.Repairs[0].Version != 2 {
			t.Fatalf("prune did %v", rep.Repairs)
		}

		if _, err := trimmed.Up(ctx(t)); err != nil {
			t.Fatalf("Up after prune: %v", err)
		}
	})

	t.Run("no operations is nothing to do", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		m := newMigrator(t, dsn, twoTables)
		if _, err := m.Repair(ctx(t)); !errors.Is(err, migrator.ErrNothingToDo) {
			t.Errorf("Repair with no operations = %v, want %v", err, migrator.ErrNothingToDo)
		}
	})
}
