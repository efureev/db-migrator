//go:build integration

package pgprogress_test

import (
	"context"
	"testing"
	"time"

	"github.com/efureev/db-migrator/v2/internal/pgprogress"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

// TestTheBuiltQueryRunsOnThisServer is the cheapest test in this package and
// the one most likely to catch something.
//
// The progress views are not stable across the versions this project supports.
// pg_stat_progress_vacuum was reshaped in PostgreSQL 17 — num_dead_tuples and
// max_dead_tuples became dead_tuple_bytes and num_dead_item_ids — and
// pg_stat_progress_copy gained tuples_skipped in 16. A query written against
// one version and never run against another fails at the moment somebody
// needed it most: in the middle of a long migration, where the failure is a
// missing log line and nobody notices at all.
//
// So the query is built the way it is built in production and executed on
// whatever server the matrix supplies. It asserts nothing about the answer,
// because there is nothing to assert about a backend that does not exist. It
// asserts that PostgreSQL accepted every column name in it.
func TestTheBuiltQueryRunsOnThisServer(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	conn := testdb.Connect(t, dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := pgprogress.New(ctx, conn)
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}

	// A pid no backend can have: pids are positive.
	snap, ok, err := r.Read(ctx, conn, -1)
	if err != nil {
		t.Fatalf("read a backend that does not exist: %v", err)
	}

	if ok {
		t.Errorf("pid -1 was reported as a running backend: %+v", snap)
	}
}

// TestReadingItselfFillsInTheActivityHalf runs the same query against a backend
// that certainly exists — the one asking — so that every column of the scan is
// filled from a real row rather than from the coalesce defaults.
//
// It also pins the fallback: a session running an ordinary query appears in no
// progress view, and that must be an answer rather than the absence of one.
func TestReadingItselfFillsInTheActivityHalf(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	conn := testdb.Connect(t, dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := pgprogress.New(ctx, conn)
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}

	var pid int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read own pid: %v", err)
	}

	snap, ok, err := r.Read(ctx, conn, pid)
	if err != nil {
		t.Fatalf("read own backend: %v", err)
	}

	if !ok {
		t.Fatal("the backend running the query reported itself as gone")
	}

	// "active" because the backend is running this very query.
	if snap.State != "active" {
		t.Errorf("state = %q, want %q; a blank state means the role may not see it", snap.State, "active")
	}

	if snap.Restricted() {
		t.Error("a session reported itself as invisible to itself")
	}

	if snap.Found {
		t.Errorf("a plain SELECT was reported as progress: %+v", snap)
	}

	if snap.StatementAge < 0 {
		t.Errorf("statement age = %s, which is in the future", snap.StatementAge)
	}
}
