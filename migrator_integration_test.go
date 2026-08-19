//go:build integration

package migrator_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

func ctx(t *testing.T) context.Context {
	t.Helper()

	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	return c
}

func src(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}

	return out
}

// twoTables is the fixture most runs use: two ordinary transactional
// migrations, each with a down file.
var twoTables = map[string]string{
	"1_create_a.up.sql":   "CREATE TABLE a (id int PRIMARY KEY);",
	"1_create_a.down.sql": "DROP TABLE a;",
	"2_create_b.up.sql":   "CREATE TABLE b (id int PRIMARY KEY);",
	"2_create_b.down.sql": "DROP TABLE b;",
}

func newMigrator(t *testing.T, dsn string, files map[string]string, opts ...migrator.Option) *migrator.Migrator {
	t.Helper()

	m, err := migrator.New(migrator.FromDSN(dsn), src(files), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return m
}

func TestUpAppliesEverything(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, twoTables)

	rep, err := m.Up(ctx(t))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if rep.Len() != 2 {
		t.Fatalf("applied %d migrations, want 2", rep.Len())
	}

	for _, name := range []string{"a", "b"} {
		if !testdb.TableExists(t, dsn, "public", name) {
			t.Errorf("table %q was not created", name)
		}
	}

	if !testdb.TableExists(t, dsn, "public", "schema_migrations") {
		t.Error("the journal was not created")
	}

	// Every row must be complete: on the transactional path the row and the
	// DDL share a transaction, so finished_at is never null.
	incomplete := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM public.schema_migrations WHERE finished_at IS NULL`)
	if incomplete != 0 {
		t.Errorf("%d rows have no finished_at", incomplete)
	}

	// A second run is a no-op and not an error.
	again, err := m.Up(ctx(t))
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}

	if !again.Empty() {
		t.Errorf("second Up applied %d migrations", again.Len())
	}
}

func TestStatusWritesNothing(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, twoTables)

	status, err := m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	// The central property: asking what is applied must not create anything.
	// A status command that writes is one nobody runs against production.
	if testdb.TableExists(t, dsn, "public", "schema_migrations") {
		t.Fatal("Status created the journal")
	}

	if status.Initialised {
		t.Error("Status reported an initialised journal that does not exist")
	}

	if len(status.Pending()) != 2 {
		t.Errorf("pending = %d, want 2", len(status.Pending()))
	}

	if status.Current() != 0 {
		t.Errorf("current = %d, want 0", status.Current())
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	status, err = m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status after Up: %v", err)
	}

	if !status.Initialised || status.Current() != 2 || len(status.Pending()) != 0 {
		t.Errorf("after Up: initialised=%v current=%d pending=%d",
			status.Initialised, status.Current(), len(status.Pending()))
	}
}

// TestFailedMigrationRollsBack is scenario B: a migration whose second
// statement fails must leave nothing behind — neither the table its first
// statement created, nor a row saying it ran.
func TestFailedMigrationRollsBack(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, map[string]string{
		"1_ok.up.sql": "CREATE TABLE ok (id int);",
		"2_broken.up.sql": "CREATE TABLE half (id int);\n" +
			"INSERT INTO nonexistent VALUES (1);",
	})

	_, err := m.Up(ctx(t))
	if err == nil {
		t.Fatal("Up accepted a migration whose second statement is invalid")
	}

	var me *migrator.MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("error is %T (%v), want *migrator.MigrationError", err, err)
	}

	if me.Version != 2 || me.File != "2_broken.up.sql" {
		t.Errorf("error names version %d file %q", me.Version, me.File)
	}

	if testdb.TableExists(t, dsn, "public", "half") {
		t.Error("the table created by the first statement survived the failure")
	}

	if !testdb.TableExists(t, dsn, "public", "ok") {
		t.Error("the migration before the failing one was rolled back too")
	}

	recorded := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM public.schema_migrations WHERE version = 2`)
	if recorded != 0 {
		t.Error("the failed migration was recorded")
	}
}

// TestNoTransactionLeavesEvidence is the mirror of the test above, and the pair
// is what gives the directive its meaning. Outside a transaction the same
// failure leaves the completed work in place and the row unconfirmed — a state
// only a person can resolve, which the tool must report rather than hide.
func TestNoTransactionLeavesEvidence(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, map[string]string{
		"1_broken.up.sql": "-- migrator:no-transaction\n" +
			"CREATE TABLE half (id int);\n" +
			"INSERT INTO nonexistent VALUES (1);",
	})

	if _, err := m.Up(ctx(t)); err == nil {
		t.Fatal("Up accepted an invalid no-transaction migration")
	}

	if !testdb.TableExists(t, dsn, "public", "half") {
		t.Error("outside a transaction the completed statement should survive")
	}

	unconfirmed := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM public.schema_migrations WHERE version = 1 AND finished_at IS NULL`)
	if unconfirmed != 1 {
		t.Fatal("no unconfirmed row was left: a crash here would be invisible")
	}

	// Nothing runs while an incomplete migration is on record, and the refusal
	// says which one and who started it.
	_, err := m.Up(ctx(t))
	if !errors.Is(err, migrator.ErrIncomplete) {
		t.Fatalf("second Up error = %v, want %v", err, migrator.ErrIncomplete)
	}

	status, err := m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if len(status.Drifted()) != 1 || status.Drifted()[0].State != migrator.StateIncomplete {
		t.Errorf("status does not report the incomplete migration: %v", status.Drifted())
	}
}

// TestConcurrentIndexNeedsTheDirective is scenario D, and it is what the whole
// no-transaction path exists for. Without the directive PostgreSQL refuses with
// 25001; with it the index is really built and really valid.
func TestConcurrentIndexNeedsTheDirective(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	testdb.Exec(t, dsn, `CREATE TABLE t (id int, c text)`)

	inTransaction := newMigrator(t, dsn, map[string]string{
		"1_index.up.sql": "CREATE INDEX CONCURRENTLY i ON t (c);",
	})

	_, err := inTransaction.Up(ctx(t))
	if err == nil {
		t.Fatal("CREATE INDEX CONCURRENTLY succeeded inside a transaction")
	}

	if !strings.Contains(err.Error(), "25001") {
		t.Errorf("error %v does not mention SQLSTATE 25001", err)
	}

	dsn2 := testdb.Fresh(t)
	testdb.Exec(t, dsn2, `CREATE TABLE t (id int, c text)`)

	// Two statements, so that the run only passes if they are sent separately:
	// the simple protocol wraps a multi-statement string in an implicit
	// transaction, and CONCURRENTLY would fail again inside it.
	withDirective := newMigrator(t, dsn2, map[string]string{
		"1_index.up.sql": "-- migrator:no-transaction\n" +
			"CREATE INDEX CONCURRENTLY i1 ON t (c);\n" +
			"CREATE INDEX CONCURRENTLY i2 ON t (id);",
	})

	if _, err := withDirective.Up(ctx(t)); err != nil {
		t.Fatalf("no-transaction migration failed: %v", err)
	}

	valid := testdb.QueryInt(t, dsn2, `
		SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname IN ('i1', 'i2') AND i.indisvalid`)
	if valid != 2 {
		t.Errorf("%d of 2 indexes are valid", valid)
	}

	complete := testdb.QueryInt(t, dsn2,
		`SELECT count(*) FROM public.schema_migrations WHERE finished_at IS NOT NULL AND NOT transactional`)
	if complete != 1 {
		t.Error("the no-transaction migration was not confirmed")
	}
}

// TestChecksumDrift is scenario C.
func TestChecksumDrift(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	before := newMigrator(t, dsn, twoTables)
	if _, err := before.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	edited := map[string]string{}
	for k, v := range twoTables {
		edited[k] = v
	}

	edited["1_create_a.up.sql"] = "CREATE TABLE a (id bigint PRIMARY KEY);"
	edited["3_create_c.up.sql"] = "CREATE TABLE c (id int);"

	after := newMigrator(t, dsn, edited)

	_, err := after.Up(ctx(t))
	if !errors.Is(err, migrator.ErrChecksumMismatch) {
		t.Fatalf("Up error = %v, want %v", err, migrator.ErrChecksumMismatch)
	}

	var ce *migrator.ChecksumError
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *migrator.ChecksumError", err)
	}

	if ce.Version != 1 || ce.Recorded == ce.Actual {
		t.Errorf("checksum error = %+v", ce)
	}

	// Nothing runs, including the new migration that is itself blameless: the
	// files on disk are not the ones this database was built from, so no later
	// assumption holds either.
	if testdb.TableExists(t, dsn, "public", "c") {
		t.Error("a pending migration ran despite the drift")
	}
}

// TestRenamingAFileIsNotDrift pins a decision: the checksum covers content, so
// renaming a migration while keeping its version and body is not an edit.
func TestRenamingAFileIsNotDrift(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	first := newMigrator(t, dsn, map[string]string{"1_old_name.up.sql": "CREATE TABLE a (id int);"})
	if _, err := first.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	renamed := newMigrator(t, dsn, map[string]string{"1_new_name.up.sql": "CREATE TABLE a (id int);"})
	if _, err := renamed.Up(ctx(t)); err != nil {
		t.Fatalf("Up after rename: %v", err)
	}
}

// TestConcurrentRunsApplyEachMigrationOnce is scenario A, and the reason the
// advisory lock exists. Eight runners, one database, one witness row per
// migration: any missing lock shows up as a duplicate or a lost row.
func TestConcurrentRunsApplyEachMigrationOnce(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	testdb.Exec(t, dsn, `CREATE TABLE witness (version int)`)

	const (
		runners    = 8
		migrations = 4
	)

	files := map[string]string{}
	for i := 1; i <= migrations; i++ {
		files[fmt.Sprintf("%d_step_%d.up.sql", i, i)] = fmt.Sprintf(
			"CREATE TABLE t%d (id int);\nINSERT INTO witness VALUES (%d);", i, i)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for range runners {
		wg.Go(func() {
			m, err := migrator.New(migrator.FromDSN(dsn), src(files),
				migrator.WithAdvisoryLockTimeout(90*time.Second))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				return
			}

			if _, err := m.Up(ctx(t)); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent run failed: %v", err)
	}

	if got := testdb.QueryInt(t, dsn, `SELECT count(*) FROM witness`); got != migrations {
		t.Errorf("witness holds %d rows, want %d: a migration ran more than once", got, migrations)
	}

	if got := testdb.QueryInt(t, dsn, `SELECT count(*) FROM public.schema_migrations`); got != migrations {
		t.Errorf("journal holds %d rows, want %d", got, migrations)
	}
}

// TestConcurrentBootstrapDoesNotRace is the subtest the lock is taken *before*
// the bookkeeping table exists for. Concurrent CREATE TABLE IF NOT EXISTS in
// PostgreSQL can fail with a duplicate key on pg_type_typname_nsp_index, and
// that only ever happens the day two replicas start against an empty database.
func TestConcurrentBootstrapDoesNotRace(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for range 8 {
		wg.Go(func() {
			m, err := migrator.New(migrator.FromDSN(dsn),
				src(map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"}),
				migrator.WithAdvisoryLockTimeout(90*time.Second))
			if err == nil {
				_, err = m.Up(ctx(t))
			}

			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("bootstrap race: %v", err)
	}
}

// TestLockTimeoutIsReportable: a run that cannot get the lock says so with a
// sentinel a CI step can retry on, rather than a generic failure.
func TestLockTimeoutIsReportable(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	files := map[string]string{"1_a.up.sql": "SELECT pg_sleep(3);"}

	holder := newMigrator(t, dsn, files, migrator.WithAdvisoryLockTimeout(30*time.Second))

	done := make(chan error, 1)
	go func() { _, err := holder.Up(ctx(t)); done <- err }()

	// Give the holder time to take the lock and start sleeping.
	time.Sleep(750 * time.Millisecond)

	waiter := newMigrator(t, dsn, files, migrator.WithAdvisoryLockTimeout(200*time.Millisecond))

	_, err := waiter.Up(ctx(t))
	if !errors.Is(err, migrator.ErrLockTimeout) {
		t.Errorf("second run error = %v, want %v", err, migrator.ErrLockTimeout)
	}

	if err := <-done; err != nil {
		t.Errorf("the holder failed: %v", err)
	}
}

// TestLockIsReleasedWhenTheBackendDies: the advisory lock is session-level, so
// PostgreSQL releases it when the connection goes. This is the property that
// makes a crashed migrator harmless, and the reason not to keep a lock row in a
// table the way golang-migrate does.
func TestLockIsReleasedWhenTheBackendDies(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	files := map[string]string{"1_a.up.sql": "SELECT pg_sleep(30);"}

	holder := newMigrator(t, dsn, files, migrator.WithAdvisoryLockTimeout(30*time.Second))
	classID, objID := holder.LockID()

	go func() { _, _ = holder.Up(ctx(t)) }()

	time.Sleep(750 * time.Millisecond)
	testdb.Terminate(t, dsn, classID, objID)

	quick := newMigrator(t, dsn,
		map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"},
		migrator.WithAdvisoryLockTimeout(10*time.Second))

	if _, err := quick.Up(ctx(t)); err != nil {
		t.Fatalf("the lock was not released when the backend died: %v", err)
	}
}

// TestPlanWritesNothing: planning answers the question without taking the lock
// or creating the journal.
func TestPlanWritesNothing(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, twoTables)

	plan, err := m.Plan(ctx(t), migrator.DirectionUp, migrator.All())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Len() != 2 {
		t.Errorf("plan has %d steps, want 2", plan.Len())
	}

	if testdb.TableExists(t, dsn, "public", "schema_migrations") {
		t.Error("Plan created the journal")
	}

	for _, step := range plan.Steps {
		if !step.Transactional {
			t.Errorf("step %s is not transactional", step.Migration)
		}

		if len(step.SQL) != 1 {
			t.Errorf("transactional step %s rendered %d statements, want 1",
				step.Migration, len(step.SQL))
		}
	}
}

func TestPlaceholders(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	testdb.Exec(t, dsn, `CREATE SCHEMA app`)

	m := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE @schema@.@name@ (id int);",
	},
		migrator.WithSchema("app"),
		migrator.WithPlaceholders(map[string]string{"@name@": "widgets"}),
	)

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if !testdb.TableExists(t, dsn, "app", "widgets") {
		t.Error("the substituted table was not created")
	}

	if !testdb.TableExists(t, dsn, "app", "schema_migrations") {
		t.Error("the journal was not created in the configured schema")
	}
}

func TestUnresolvedPlaceholderIsRefused(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, map[string]string{
		"1_a.up.sql": "CREATE TABLE @tabel@ (id int);",
	})

	_, err := m.Up(ctx(t))
	if !errors.Is(err, migrator.ErrUnresolvedPlaceholder) {
		t.Fatalf("Up error = %v, want %v", err, migrator.ErrUnresolvedPlaceholder)
	}

	if !strings.Contains(err.Error(), "@tabel@") {
		t.Errorf("error %v does not quote the unresolved token", err)
	}
}

func TestUpToAndSteps(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, twoTables)

	rep, err := m.UpTo(ctx(t), migrator.Steps(1))
	if err != nil {
		t.Fatalf("UpTo: %v", err)
	}

	if rep.Len() != 1 || rep.Current() != 1 {
		t.Fatalf("one step applied %d migrations, current %d", rep.Len(), rep.Current())
	}

	if testdb.TableExists(t, dsn, "public", "b") {
		t.Error("the second migration ran despite Steps(1)")
	}

	if _, err := m.UpTo(ctx(t), migrator.ToVersion(2)); err != nil {
		t.Fatalf("UpTo version 2: %v", err)
	}

	if !testdb.TableExists(t, dsn, "public", "b") {
		t.Error("the second migration did not run")
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	m := newMigrator(t, dsn, twoTables)

	v, err := m.Version(ctx(t))
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	if v.Current != 0 || v.Pending != 2 || v.Dirty {
		t.Errorf("before Up: %+v", v)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	v, err = m.Version(ctx(t))
	if err != nil {
		t.Fatalf("Version after Up: %v", err)
	}

	if v.Current != 2 || v.Name != "create_b" || v.Pending != 0 {
		t.Errorf("after Up: %+v", v)
	}
}

func TestFromConnAndFromPool(t *testing.T) {
	t.Parallel()

	t.Run("FromConn does not close the caller's connection", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		conn := testdb.Connect(t, dsn)

		m, err := migrator.New(migrator.FromConn(conn), src(twoTables))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if _, err := m.Up(ctx(t)); err != nil {
			t.Fatalf("Up: %v", err)
		}

		// The connection must still work: Release is a no-op for a borrowed one.
		var one int
		if err := conn.QueryRow(ctx(t), `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("the caller's connection was closed: %v", err)
		}
	})
}

// TestRunSettingsDoNotLeak: a migration that sets a long statement_timeout must
// not leave it behind on a pooled connection.
func TestRunSettingsDoNotLeak(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	conn := testdb.Connect(t, dsn)

	var before string
	if err := conn.QueryRow(ctx(t), `SHOW statement_timeout`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	m, err := migrator.New(migrator.FromConn(conn), src(twoTables),
		migrator.WithStatementTimeout(time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var after string
	if err := conn.QueryRow(ctx(t), `SHOW statement_timeout`).Scan(&after); err != nil {
		t.Fatal(err)
	}

	if after != before {
		t.Errorf("statement_timeout leaked: %q before the run, %q after", before, after)
	}
}
