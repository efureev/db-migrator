//go:build integration

package migrator_test

import (
	"errors"
	"strings"
	"testing"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

// threeMigrations is a source whose first two versions are already in the
// database when adoption starts.
var threeMigrations = map[string]string{
	"1_create_a.up.sql":   "CREATE TABLE a (id int PRIMARY KEY);",
	"1_create_a.down.sql": "DROP TABLE a;",
	"2_create_b.up.sql":   "CREATE TABLE b (id int PRIMARY KEY);",
	"2_create_b.down.sql": "DROP TABLE b;",
	"3_create_c.up.sql":   "CREATE TABLE c (id int PRIMARY KEY);",
	"3_create_c.down.sql": "DROP TABLE c;",
}

// legacyDatabase builds what a golang-migrate installation looks like: the
// schema already applied by hand, and a (version, dirty) journal.
func legacyDatabase(t *testing.T, dirty bool) string {
	t.Helper()

	dsn := testdb.Fresh(t)

	testdb.Exec(t, dsn, `CREATE TABLE a (id int PRIMARY KEY)`)
	testdb.Exec(t, dsn, `CREATE TABLE b (id int PRIMARY KEY)`)
	testdb.Exec(t, dsn, `
		CREATE TABLE public.schema_migrations (
			version BIGINT NOT NULL PRIMARY KEY,
			dirty   BOOLEAN NOT NULL
		)`)
	testdb.Exec(t, dsn, `INSERT INTO public.schema_migrations VALUES ($1, $2)`, 2, dirty)

	return dsn
}

// TestAdoptFromGolangMigrate is the path out of 1.x, and the reason this
// command exists: without it a database with a schema cannot be handed over at
// all, because Up would try to apply migration 1 against tables that exist.
func TestAdoptFromGolangMigrate(t *testing.T) {
	t.Parallel()

	dsn := legacyDatabase(t, false)
	m := newMigrator(t, dsn, threeMigrations)

	// Before adoption the tool refuses to touch a journal that is not its own.
	if _, err := m.Up(ctx(t)); !errors.Is(err, migrator.ErrForeignJournal) {
		t.Fatalf("Up over a foreign journal = %v, want %v", err, migrator.ErrForeignJournal)
	}

	report, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
		migrator.AdoptOptions{FromGolangMigrate: true})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if report.Len() != 2 {
		t.Fatalf("adopted %d migrations, want 2 (the baseline read from the old journal)", report.Len())
	}

	for _, rec := range report.Applied {
		if !rec.Adopted() {
			t.Errorf("version %d is not marked adopted", rec.Version)
		}
	}

	// The old journal was moved aside, not dropped: a rollback to the old tool
	// has to stay possible, and that row is the only copy.
	if !testdb.TableExists(t, dsn, "public", "schema_migrations_pre_v2") {
		t.Error("the old journal was not preserved")
	}

	if n := testdb.QueryInt(t, dsn, `SELECT version FROM public.schema_migrations_pre_v2`); n != 2 {
		t.Errorf("the preserved journal holds version %d, want 2", n)
	}

	// Only the third migration is left, and it runs.
	status, err := m.Status(ctx(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if len(status.Pending()) != 1 || status.Current() != 2 {
		t.Errorf("after adoption: pending=%d current=%d", len(status.Pending()), status.Current())
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up after adoption: %v", err)
	}

	if !testdb.TableExists(t, dsn, "public", "c") {
		t.Error("the remaining migration did not run")
	}

	// And the adopted rows stay marked forever: nobody here watched them run.
	adopted := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM public.schema_migrations WHERE adopted_at IS NOT NULL`)
	if adopted != 2 {
		t.Errorf("%d rows are marked adopted, want 2", adopted)
	}
}

// TestAdoptRefusesADirtyJournal: dirty means a migration failed partway and
// nobody recorded what state it left the schema in. Adopting that would freeze
// an unknown state as the truth.
func TestAdoptRefusesADirtyJournal(t *testing.T) {
	t.Parallel()

	dsn := legacyDatabase(t, true)
	m := newMigrator(t, dsn, threeMigrations)

	_, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
		migrator.AdoptOptions{FromGolangMigrate: true})
	if !errors.Is(err, migrator.ErrDirtyJournal) {
		t.Fatalf("Adopt = %v, want %v", err, migrator.ErrDirtyJournal)
	}

	// Nothing was touched: the old journal keeps its name and its row.
	if testdb.TableExists(t, dsn, "public", "schema_migrations_pre_v2") {
		t.Error("the refused adoption moved the old journal anyway")
	}

	if !strings.Contains(err.Error(), "old tool") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestAdoptBaseline(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	testdb.Exec(t, dsn, `CREATE TABLE a (id int PRIMARY KEY)`)

	m := newMigrator(t, dsn, threeMigrations)

	report, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
		migrator.AdoptOptions{Baseline: 1})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if report.Len() != 1 || report.Applied[0].Version != 1 {
		t.Fatalf("adopted %v, want just version 1", report.Versions())
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up after adoption: %v", err)
	}

	for _, name := range []string{"b", "c"} {
		if !testdb.TableExists(t, dsn, "public", name) {
			t.Errorf("migration for %q did not run", name)
		}
	}
}

func TestAdoptGuards(t *testing.T) {
	t.Parallel()

	t.Run("a baseline that is not in the source", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		m := newMigrator(t, dsn, threeMigrations)

		_, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
			migrator.AdoptOptions{Baseline: 99})
		if !errors.Is(err, migrator.ErrMissingMigration) {
			t.Errorf("Adopt = %v, want %v", err, migrator.ErrMissingMigration)
		}
	})

	t.Run("no baseline at all", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		m := newMigrator(t, dsn, threeMigrations)

		_, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)), migrator.AdoptOptions{})
		if !errors.Is(err, migrator.ErrNoBaseline) {
			t.Errorf("Adopt = %v, want %v", err, migrator.ErrNoBaseline)
		}
	})

	t.Run("a journal this tool already manages", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		m := newMigrator(t, dsn, threeMigrations)

		if _, err := m.Up(ctx(t)); err != nil {
			t.Fatalf("Up: %v", err)
		}

		_, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
			migrator.AdoptOptions{Baseline: 1})
		if !errors.Is(err, migrator.ErrAlreadyAdopted) {
			t.Errorf("Adopt over a managed journal = %v, want %v", err, migrator.ErrAlreadyAdopted)
		}

		// --force is the second attempt after an interrupted first one.
		if _, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
			migrator.AdoptOptions{Baseline: 1, Force: true}); err != nil {
			t.Errorf("Adopt --force: %v", err)
		}
	})

	t.Run("a confirmation naming the wrong database", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		m := newMigrator(t, dsn, threeMigrations)

		_, err := m.Adopt(ctx(t), migrator.Confirm("some_other_database"),
			migrator.AdoptOptions{Baseline: 1})
		if !errors.Is(err, migrator.ErrNotConfirmed) {
			t.Errorf("Adopt = %v, want %v", err, migrator.ErrNotConfirmed)
		}
	})
}

// TestAdoptDryRunWritesNothing: the whole point of running it first is to see
// which versions would be claimed as already applied, on a database where that
// claim cannot be checked afterwards.
func TestAdoptDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	dsn := legacyDatabase(t, false)
	m := newMigrator(t, dsn, threeMigrations)

	report, err := m.Adopt(ctx(t), migrator.Confirm(databaseOf(t, dsn)),
		migrator.AdoptOptions{FromGolangMigrate: true, DryRun: true})
	if err != nil {
		t.Fatalf("Adopt --dry-run: %v", err)
	}

	if !report.DryRun || report.Len() != 2 {
		t.Errorf("dry run reported %d rows, DryRun=%v", report.Len(), report.DryRun)
	}

	// The old journal keeps its name, and no journal of ours appeared.
	if testdb.TableExists(t, dsn, "public", "schema_migrations_pre_v2") {
		t.Error("the dry run moved the old journal")
	}

	n := testdb.QueryInt(t, dsn, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'schema_migrations'
		   AND column_name = 'checksum'`)
	if n != 0 {
		t.Error("the dry run created our journal")
	}
}

// TestWipeRefusesWhenAnotherSchemaDepends: DROP ... CASCADE is silent about
// what it takes, and a view two schemas away vanishing because somebody reset
// their development schema is not something to discover later.
func TestWipeRefusesWhenAnotherSchemaDepends(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	database := databaseOf(t, dsn)

	m := newMigrator(t, dsn, twoTables, migrator.WithAllowWipe())
	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	testdb.Exec(t, dsn, `CREATE SCHEMA reporting`)
	testdb.Exec(t, dsn, `CREATE VIEW reporting.all_a AS SELECT * FROM public.a`)

	report, err := m.Wipe(ctx(t), migrator.Confirm(database))
	if !errors.Is(err, migrator.ErrWipeRefused) {
		t.Fatalf("Wipe = %v, want %v", err, migrator.ErrWipeRefused)
	}

	if report == nil || len(report.Dependents) != 1 {
		t.Fatalf("the refusal does not list the dependants: %+v", report)
	}

	if report.Dependents[0].Schema != "reporting" || report.Dependents[0].Name != "all_a" {
		t.Errorf("dependant = %+v", report.Dependents[0])
	}

	if !strings.Contains(err.Error(), "reporting.all_a") {
		t.Errorf("the error does not name what would be lost: %v", err)
	}

	// Nothing was dropped by the refusal.
	if !testdb.TableExists(t, dsn, "public", "a") {
		t.Error("the refused wipe dropped something")
	}

	// With the force option the operator accepts it, and it goes.
	forced := newMigrator(t, dsn, twoTables, migrator.WithAllowWipe(), migrator.WithForceWipe())
	if _, err := forced.Wipe(ctx(t), migrator.Confirm(database)); err != nil {
		t.Fatalf("forced Wipe: %v", err)
	}

	if n := testdb.QueryInt(t, dsn,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'reporting' AND c.relname = 'all_a'`); n != 0 {
		t.Error("the forced wipe left the dependent view behind")
	}
}
