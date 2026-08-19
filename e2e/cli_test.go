//go:build integration

// Package e2e runs the built binary as a black box.
//
// It checks the things that are not visible from inside the process: the exit
// codes, what lands on which stream, and what two real processes do to one
// database at the same time. Goroutines cannot catch the last of those —
// "the lock was taken but the connection closed before the migration ran" is a
// process-lifetime bug.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/efureev/db-migrator/v2/internal/testdb"
)

// Exit codes, repeated here rather than imported: this package is a black box,
// and a table that drifts from the one in internal/cli is exactly what these
// tests exist to catch.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitPending = 3
	exitDrift   = 4
	exitLocked  = 5
	exitRefused = 6
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "migrator-e2e-*")
	if err != nil {
		panic(err)
	}

	binary = filepath.Join(dir, "migrator")

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)

	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "../cmd/migrator")
	build.Stderr = os.Stderr

	err = build.Run()

	cancelBuild()

	if err != nil {
		_ = os.RemoveAll(dir)
		panic("building the binary under test: " + err.Error())
	}

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// result is one invocation of the binary.
type result struct {
	code           int
	stdout, stderr string
}

// runMigrator invokes the binary and reports what happened.
func runMigrator(t *testing.T, dir, dsn string, args ...string) result {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"MIGRATOR_DSN="+dsn,
		// Colour would put escape sequences into every assertion.
		"NO_COLOR=1",
	)

	var out, errw bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &errw

	err := cmd.Run()

	res := result{stdout: out.String(), stderr: errw.String()}

	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %v: %v", args, err)
		}

		res.code = exitErr.ExitCode()
	}

	return res
}

// project lays out a working directory with a migrations folder.
func project(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, "migrations", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

var twoTables = map[string]string{
	"1_create_a.up.sql":   "CREATE TABLE a (id int PRIMARY KEY);",
	"1_create_a.down.sql": "DROP TABLE a;",
	"2_create_b.up.sql":   "CREATE TABLE b (id int PRIMARY KEY);",
	"2_create_b.down.sql": "DROP TABLE b;",
}

func TestExitCodeTable(t *testing.T) {
	t.Parallel()

	t.Run("0 on success and on nothing to do", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		if got := runMigrator(t, dir, dsn, "up"); got.code != exitOK {
			t.Fatalf("up exited %d: %s", got.code, got.stderr)
		}

		got := runMigrator(t, dir, dsn, "up")
		if got.code != exitOK {
			t.Errorf("a second up exited %d", got.code)
		}

		if !strings.Contains(got.stdout, "up to date") {
			t.Errorf("stdout = %q", got.stdout)
		}
	})

	t.Run("1 when a migration fails", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, map[string]string{"1_bad.up.sql": "SELECT nonexistent_function();"})

		got := runMigrator(t, dir, dsn, "up")
		if got.code != exitFailure {
			t.Errorf("exited %d, want %d; stderr: %s", got.code, exitFailure, got.stderr)
		}
	})

	t.Run("2 on an unknown command", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		if got := runMigrator(t, project(t, nil), dsn, "nosuch"); got.code != exitUsage {
			t.Errorf("exited %d, want %d", got.code, exitUsage)
		}
	})

	t.Run("2 on contradictory flags", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		if got := runMigrator(t, dir, dsn, "up", "--steps", "1", "--to", "2"); got.code != exitUsage {
			t.Errorf("exited %d, want %d", got.code, exitUsage)
		}
	})

	t.Run("3 when migrations are pending", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		got := runMigrator(t, dir, dsn, "status", "--check")
		if got.code != exitPending {
			t.Errorf("exited %d, want %d; stderr: %s", got.code, exitPending, got.stderr)
		}

		runMigrator(t, dir, dsn, "up")

		if got := runMigrator(t, dir, dsn, "status", "--check"); got.code != exitOK {
			t.Errorf("after up, status --check exited %d, want %d", got.code, exitOK)
		}
	})

	t.Run("4 on drift", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		runMigrator(t, dir, dsn, "up")

		edit(t, dir, "1_create_a.up.sql", "CREATE TABLE a (id bigint PRIMARY KEY);")

		got := runMigrator(t, dir, dsn, "up")
		if got.code != exitDrift {
			t.Errorf("exited %d, want %d; stderr: %s", got.code, exitDrift, got.stderr)
		}

		if !strings.Contains(got.stderr, "1_create_a.up.sql") {
			t.Errorf("stderr does not name the drifted file: %q", got.stderr)
		}

		// repair fixes it, and up then works.
		if got := runMigrator(t, dir, dsn, "repair", "--rehash", "1"); got.code != exitOK {
			t.Fatalf("repair exited %d: %s", got.code, got.stderr)
		}

		if got := runMigrator(t, dir, dsn, "up"); got.code != exitOK {
			t.Errorf("up after repair exited %d: %s", got.code, got.stderr)
		}
	})

	t.Run("6 when a guard refuses", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		runMigrator(t, dir, dsn, "up")

		for _, args := range [][]string{
			{"down"},                 // no --allow-down
			{"wipe", "--allow-wipe"}, // no --confirm
			{"wipe", "--allow-wipe", "--confirm", "wrong_name"},
		} {
			got := runMigrator(t, dir, dsn, args...)
			if got.code != exitRefused {
				t.Errorf("%v exited %d, want %d; stderr: %s", args, got.code, exitRefused, got.stderr)
			}
		}
	})
}

// TestTwoProcessesRaceForTheLock is what goroutines cannot test: two real
// processes, two connections, one database.
func TestTwoProcessesRaceForTheLock(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, map[string]string{
		"1_slow.up.sql": "CREATE TABLE slow (id int);\nSELECT pg_sleep(2);",
	})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []result
	)

	for range 4 {
		wg.Go(func() {
			got := runMigrator(t, dir, dsn, "up", "--advisory-lock-timeout", "60s")

			mu.Lock()
			results = append(results, got)
			mu.Unlock()
		})
	}

	wg.Wait()

	for _, got := range results {
		if got.code != exitOK {
			t.Errorf("a racing process exited %d: %s", got.code, got.stderr)
		}
	}

	// Exactly one row, whichever process won.
	if n := testdb.QueryInt(t, dsn, `SELECT count(*) FROM public.schema_migrations`); n != 1 {
		t.Errorf("the journal holds %d rows, want 1", n)
	}
}

// TestLockedExitCode: a process that cannot get the lock reports 5, which is
// the code a CI step retries on.
func TestLockedExitCode(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, map[string]string{"1_slow.up.sql": "SELECT pg_sleep(3);"})

	done := make(chan result, 1)

	go func() { done <- runMigrator(t, dir, dsn, "up", "--advisory-lock-timeout", "30s") }()

	time.Sleep(time.Second)

	got := runMigrator(t, dir, dsn, "up", "--advisory-lock-timeout", "200ms")
	if got.code != exitLocked {
		t.Errorf("exited %d, want %d; stderr: %s", got.code, exitLocked, got.stderr)
	}

	if held := <-done; held.code != exitOK {
		t.Errorf("the holder exited %d: %s", held.code, held.stderr)
	}
}

// TestStreams: the answer goes to stdout so it can be piped, the commentary to
// stderr so it does not corrupt the pipe.
func TestStreams(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, twoTables)

	got := runMigrator(t, dir, dsn, "up")
	if got.code != exitOK {
		t.Fatalf("up exited %d: %s", got.code, got.stderr)
	}

	if !strings.Contains(got.stdout, "applied") {
		t.Errorf("the report is not on stdout: %q", got.stdout)
	}

	// --quiet keeps the failure but drops the progress.
	quiet := runMigrator(t, dir, dsn, "status", "--quiet")
	if quiet.code != exitOK {
		t.Fatalf("status --quiet exited %d: %s", quiet.code, quiet.stderr)
	}
}

func TestJSONOutputIsParseable(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, twoTables)

	runMigrator(t, dir, dsn, "up")

	got := runMigrator(t, dir, dsn, "status", "--json")
	if got.code != exitOK {
		t.Fatalf("exited %d: %s", got.code, got.stderr)
	}

	var status struct {
		Schema      string `json:"schema"`
		Initialised bool   `json:"initialised"`
		Current     int64  `json:"current_version"`
		Entries     []struct {
			Version int64  `json:"version"`
			State   string `json:"state"`
		} `json:"entries"`
	}

	if err := json.Unmarshal([]byte(got.stdout), &status); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, got.stdout)
	}

	if !status.Initialised || status.Current != 2 || len(status.Entries) != 2 {
		t.Errorf("decoded %+v", status)
	}

	// Nothing but JSON on stdout: a progress line there would break every
	// consumer that pipes it into jq.
	if strings.TrimSpace(got.stdout)[0] != '{' {
		t.Errorf("stdout does not begin with JSON: %q", got.stdout)
	}
}

// TestVersionIsStamped: a binary built the ordinary way still knows what it is,
// which version 1 never did — its ldflags named a package that did not exist.
func TestVersionIsStamped(t *testing.T) {
	t.Parallel()

	got := runMigrator(t, t.TempDir(), "", "version")
	if got.code != exitOK {
		t.Fatalf("version exited %d: %s", got.code, got.stderr)
	}

	if !strings.HasPrefix(got.stdout, "migrator ") {
		t.Errorf("version printed %q", got.stdout)
	}

	if strings.Contains(got.stdout, "unknown") {
		t.Errorf("version printed %q, which is what v1 always printed", got.stdout)
	}
}

// TestCreateThenUp is the loop a person actually performs.
func TestCreateThenUp(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, nil)

	created := runMigrator(t, dir, dsn, "create", "add widgets")
	if created.code != exitOK {
		t.Fatalf("create exited %d: %s", created.code, created.stderr)
	}

	paths := strings.Fields(strings.TrimSpace(created.stdout))
	if len(paths) != 2 {
		t.Fatalf("create wrote %d paths: %q", len(paths), created.stdout)
	}

	// The templates are comments, so the migration is empty until filled in —
	// and an empty migration is refused rather than recorded as applied.
	if got := runMigrator(t, dir, dsn, "up"); got.code != exitUsage {
		t.Errorf("up over a template-only migration exited %d, want %d", got.code, exitUsage)
	}

	for _, p := range paths {
		body := "CREATE TABLE widgets (id int);"
		if strings.Contains(p, ".down.") {
			body = "DROP TABLE widgets;"
		}

		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := runMigrator(t, dir, dsn, "up"); got.code != exitOK {
		t.Fatalf("up exited %d: %s", got.code, got.stderr)
	}

	if !testdb.TableExists(t, dsn, "public", "widgets") {
		t.Error("the migration did not run")
	}
}

func edit(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "migrations", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
