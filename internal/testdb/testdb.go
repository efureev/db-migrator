// Package testdb hands integration tests a database of their own.
//
// The seam is a DSN in MIGRATOR_TEST_DSN. Tests do not know or care who started
// the server: GitHub Actions uses a service container, a developer uses
// `make db-up`. That is the whole reason this project does not depend on
// testcontainers — everything it would provide here is one docker command, and
// the module is still v0.x with forty dependencies behind it.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// DSN reports the connection string for the server integration tests run
// against.
//
// In CI a missing DSN is a failure, not a skip. A silently skipped integration
// level is the worst outcome available: it looks exactly like a green run, and
// this is the level that covers everything a fake cannot — advisory locks,
// CREATE INDEX CONCURRENTLY, SQLSTATE 25001, DDL rollback.
func DSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("MIGRATOR_TEST_DSN")
	if dsn != "" {
		return dsn
	}

	if os.Getenv("CI") != "" {
		t.Fatal("MIGRATOR_TEST_DSN is not set in CI: the integration level must not be skipped silently")
	}

	t.Skip("MIGRATOR_TEST_DSN is not set: run `make db-up && make test-integration`")

	return ""
}

// Fresh creates a database of its own for one test and reports its DSN.
//
// A database rather than a schema, because wipe and the bootstrap race are only
// testable honestly when the test owns everything in sight — and because it
// makes t.Parallel safe. It costs about fifty milliseconds.
func Fresh(t *testing.T) string {
	t.Helper()

	admin := DSN(t)
	name := "mig_" + randomSuffix(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, admin)

	if _, err := conn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("create database %s: %v", name, err)
	}

	_ = conn.Close(ctx)

	t.Cleanup(func() {
		// A detached context: the test's own may already be cancelled, and a
		// database left behind outlives the run.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()

		c := connect(t, dropCtx, admin)
		defer func() { _ = c.Close(dropCtx) }()

		// WITH (FORCE) terminates whatever is still connected. A test that
		// leaked a connection should fail on its own assertions, not by
		// wedging the cleanup of every test after it.
		if _, err := c.Exec(dropCtx,
			`DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})

	return replaceDatabase(t, admin, name)
}

// Connect opens a connection to dsn and closes it when the test ends.
func Connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)

	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	})

	return conn
}

// ServerVersion reports the major version of the server behind dsn, so that a
// test needing a feature can say which release introduced it.
func ServerVersion(t *testing.T, dsn string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)
	defer func() { _ = conn.Close(ctx) }()

	var num int
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&num); err != nil {
		t.Fatalf("read server version: %v", err)
	}

	return num / 10000
}

func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	return conn
}

// replaceDatabase rewrites the database part of a DSN, in whichever of the two
// syntaxes pgx accepts it was written.
func replaceDatabase(t *testing.T, dsn, name string) string {
	t.Helper()

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}

		u.Path = "/" + name

		return u.String()
	}

	// keyword/value form: replace dbname= if present, append it otherwise.
	var (
		out     []string
		replace bool
	)

	for _, field := range strings.Fields(dsn) {
		if strings.HasPrefix(field, "dbname=") {
			out = append(out, "dbname="+name)
			replace = true

			continue
		}

		out = append(out, field)
	}

	if !replace {
		out = append(out, "dbname="+name)
	}

	return strings.Join(out, " ")
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}

	return hex.EncodeToString(b[:])
}

// Exec runs a statement against dsn, for the arrangement a test needs before
// the code under test runs.
func Exec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// QueryInt runs a query returning one integer.
func QueryInt(t *testing.T, dsn, sql string, args ...any) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)
	defer func() { _ = conn.Close(ctx) }()

	var n int
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}

	return n
}

// TableExists reports whether a table exists in the given schema.
func TableExists(t *testing.T, dsn, schema, table string) bool {
	t.Helper()

	return QueryInt(t, dsn, `
		SELECT count(*) FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r','p')`,
		schema, table) > 0
}

// Terminate kills the backend holding the given advisory lock, which is how a
// test simulates a process dying mid-run.
func Terminate(t *testing.T, dsn string, classID, objID int32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)
	defer func() { _ = conn.Close(ctx) }()

	// Scoped to this test's own database. Advisory locks are cluster-wide: the
	// same (classid, objid) is visible from every database on the server, so an
	// unscoped terminate kills the backend of whichever parallel test happens
	// to share a schema and table name — which is every test, since they all
	// default to public.schema_migrations.
	rows, err := conn.Query(ctx, `
		SELECT pg_terminate_backend(l.pid)
		  FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = $2
		   AND l.pid <> pg_backend_pid()
		   AND a.datname = current_database()`, classID, objID)
	if err != nil {
		t.Fatalf("terminate backend: %v", err)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		t.Fatalf("terminate backend: %v", err)
	}
}

// Dump reports the bookkeeping table as text, for an artifact attached to a
// failing test.
func Dump(t *testing.T, dsn, schema, table string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := connect(t, ctx, dsn)
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, fmt.Sprintf(
		`SELECT version, name, checksum, applied_at, finished_at, rolled_back_at, transactional
		   FROM %s ORDER BY version`, pgx.Identifier{schema, table}.Sanitize()))
	if err != nil {
		return "could not read the journal: " + err.Error()
	}
	defer rows.Close()

	var b strings.Builder

	b.WriteString("version | name | checksum | applied_at | finished_at | rolled_back_at | tx\n")

	for rows.Next() {
		var (
			version                int64
			name, checksum         string
			appliedAt              time.Time
			finishedAt, rolledBack *time.Time
			transactional          bool
		)

		if err := rows.Scan(&version, &name, &checksum, &appliedAt,
			&finishedAt, &rolledBack, &transactional); err != nil {
			return "could not scan the journal: " + err.Error()
		}

		fmt.Fprintf(&b, "%d | %s | %s | %s | %v | %v | %t\n",
			version, name, checksum[:12], appliedAt.Format(time.RFC3339),
			finishedAt, rolledBack, transactional)
	}

	return b.String()
}
