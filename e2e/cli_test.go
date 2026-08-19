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

var (
	binary string
	// coverDir is where the instrumented binary writes its coverage, when the
	// test run itself was built with coverage.
	//
	// Without this the CLI looks barely tested: everything reached through the
	// binary runs in another process, and Go collects nothing from a subprocess
	// unless it is built with -cover and told where to put the data. The
	// threshold for internal/cli used to carry an apology about it.
	coverDir string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "migrator-e2e-*")
	if err != nil {
		panic(err)
	}

	binary = filepath.Join(dir, "migrator")

	args := []string{"build", "-o", binary}

	// An explicit variable set by coverage.sh, not GOCOVERDIR: `go test` does
	// not put GOCOVERDIR into the test process's environment, so reading it
	// here would silently never fire — which is exactly the shape of bug this
	// whole exercise is about.
	if out := os.Getenv("MIGRATOR_E2E_COVERDIR"); out != "" {
		coverDir = out

		// -covermode=atomic must match what the test run uses, or covdata
		// refuses to merge the two with "counter mode clash".
		args = append(args,
			"-cover", "-covermode=atomic",
			"-coverpkg=github.com/efureev/db-migrator/v2/...")
	}

	args = append(args, "../cmd/migrator")

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)

	build := exec.CommandContext(buildCtx, "go", args...)
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

	if coverDir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+coverDir)
	}

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

	t.Run("2 on a progress interval that is not one", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, twoTables)

		// Two rejections down two different paths: a duration that does not
		// parse is stopped by the parser, one that parses to a negative by
		// Validate. One has been broken without the other before.
		for _, bad := range []string{"often", "-5s"} {
			got := runMigrator(t, dir, dsn, "up", "--progress-interval", bad)
			if got.code != exitUsage {
				t.Errorf("--progress-interval %s exited %d, want %d; stderr: %s",
					bad, got.code, exitUsage, got.stderr)
			}

			if !strings.Contains(got.stderr, "progress-interval") {
				t.Errorf("the refusal does not name the setting: %s", got.stderr)
			}
		}

		// And nothing ran: a usage error has to be decided before the database
		// is touched, or exit 2 stops meaning "retrying will not help".
		if code := runMigrator(t, dir, dsn, "status", "--check").code; code != exitPending {
			t.Errorf("status --check exited %d, want %d", code, exitPending)
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

	t.Run("6 when a migration takes a heavier lock than allowed", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)
		dir := project(t, map[string]string{
			"1_create.up.sql": "CREATE TABLE t (id int PRIMARY KEY, name text);",
			"2_widen.up.sql":  "ALTER TABLE t ALTER COLUMN name TYPE varchar(80);",
		})

		got := runMigrator(t, dir, dsn, "up", "--max-lock-level", "share-update-exclusive")
		if got.code != exitRefused {
			t.Fatalf("exited %d, want %d; stderr: %s", got.code, exitRefused, got.stderr)
		}

		// The refusal has to say what to do about it, or it is just a wall.
		if !strings.Contains(got.stderr, "lock-acknowledged") {
			t.Errorf("the refusal does not say how to accept it: %s", got.stderr)
		}

		// And nothing may have run: the first migration is still pending.
		if code := runMigrator(t, dir, dsn, "status", "--check").code; code != exitPending {
			t.Errorf("status --check exited %d, want %d — the refusal let something through",
				code, exitPending)
		}
	})

	t.Run("2 on a lock level that is not one", func(t *testing.T) {
		t.Parallel()

		dsn := testdb.Fresh(t)

		got := runMigrator(t, project(t, twoTables), dsn, "up", "--max-lock-level", "very-exclusive")
		if got.code != exitUsage {
			t.Errorf("exited %d, want %d; stderr: %s", got.code, exitUsage, got.stderr)
		}

		if !strings.Contains(got.stderr, "access-exclusive") {
			t.Errorf("the error does not list the valid levels: %s", got.stderr)
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
	dir := project(t, map[string]string{"1_slow.up.sql": "SELECT pg_sleep(10);"})

	done := make(chan result, 1)

	go func() { done <- runMigrator(t, dir, dsn, "up", "--advisory-lock-timeout", "60s") }()

	// Waiting for the lock to actually appear rather than sleeping a guessed
	// interval. A fixed sleep raced the holder's process startup, and under
	// -race with the other tests running in parallel the holder sometimes had
	// not acquired anything yet — so the "waiter" won the lock and the test
	// failed for a reason that had nothing to do with what it checks.
	waitForAdvisoryLock(t, dsn)

	got := runMigrator(t, dir, dsn, "up", "--advisory-lock-timeout", "200ms")
	if got.code != exitLocked {
		t.Errorf("exited %d, want %d; stderr: %s", got.code, exitLocked, got.stderr)
	}

	if held := <-done; held.code != exitOK {
		t.Errorf("the holder exited %d: %s", held.code, held.stderr)
	}
}

// waitForAdvisoryLock blocks until some backend in this test's database holds
// an advisory lock, or the test times out.
func waitForAdvisoryLock(t *testing.T, dsn string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		held := testdb.QueryInt(t, dsn, `
			SELECT count(*) FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE l.locktype = 'advisory' AND a.datname = current_database()`)
		if held > 0 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("no advisory lock appeared: the holding process never started")
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

// TestConfigShowsTheProgressInterval: a setting nobody can see the value of is
// a setting nobody can debug. `migrator config` needs no database, which is
// what makes this cheap.
func TestConfigShowsTheProgressInterval(t *testing.T) {
	t.Parallel()

	dir := project(t, twoTables)

	got := runMigrator(t, dir, "", "config")
	if got.code != exitOK {
		t.Fatalf("config exited %d: %s", got.code, got.stderr)
	}

	if !strings.Contains(got.stdout, "progress-interval") || !strings.Contains(got.stdout, "30s") {
		t.Errorf("the default is not shown:\n%s", got.stdout)
	}

	got = runMigrator(t, dir, "", "config", "--progress-interval", "5s")
	if !strings.Contains(got.stdout, "flag --progress-interval") {
		t.Errorf("the provenance of the typed flag is not shown:\n%s", got.stdout)
	}
}

// TestProgressGoesToStderrAndNeverBreaksJSON is the line that earns its place
// most.
//
// Commentary on stdout would break every consumer that pipes the answer into
// jq, and commentary that is not itself JSON would break every consumer that
// parses stderr — which is exactly why ui.New switches the commentary encoder
// under --json. A progress line arriving every 300ms is the first thing that
// would find either hole.
func TestProgressGoesToStderrAndNeverBreaksJSON(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, map[string]string{"1_slow.up.sql": "SELECT pg_sleep(2);"})

	got := runMigrator(t, dir, dsn, "up", "--json", "--progress-interval", "300ms")
	if got.code != exitOK {
		t.Fatalf("up exited %d: %s", got.code, got.stderr)
	}

	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, got.stdout)
	}

	if strings.TrimSpace(got.stdout)[0] != '{' {
		t.Errorf("stdout does not begin with JSON: %q", got.stdout)
	}

	var progress int

	for _, line := range strings.Split(strings.TrimSpace(got.stderr), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a commentary line is not JSON: %v\n%s", err, line)
		}

		if _, ok := entry["elapsed"]; ok {
			progress++
		}
	}

	// Two seconds against a 300ms interval. Not how many lines — that would be
	// a test of a ticker on a shared runner — only that the channel carried
	// something.
	if progress == 0 {
		t.Errorf("a two-second migration reported nothing on stderr:\n%s", got.stderr)
	}
}

// TestQuietSilencesProgressAndNotFailures: --quiet is a request for less noise,
// never for a program that fails without saying so.
func TestQuietSilencesProgressAndNotFailures(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	dir := project(t, map[string]string{"1_slow.up.sql": "SELECT pg_sleep(2);"})

	got := runMigrator(t, dir, dsn, "up", "--quiet", "--progress-interval", "300ms")
	if got.code != exitOK {
		t.Fatalf("up exited %d: %s", got.code, got.stderr)
	}

	if strings.Contains(got.stderr, "in progress") || strings.Contains(got.stderr, "still running") {
		t.Errorf("--quiet still reported progress:\n%s", got.stderr)
	}

	bad := project(t, map[string]string{"1_bad.up.sql": "SELECT nonexistent_function();"})

	failed := runMigrator(t, bad, testdb.Fresh(t), "up", "--quiet", "--progress-interval", "300ms")
	if failed.code != exitFailure {
		t.Errorf("a failing migration under --quiet exited %d, want %d", failed.code, exitFailure)
	}

	if strings.TrimSpace(failed.stderr) == "" {
		t.Error("a failing migration under --quiet said nothing at all")
	}
}
