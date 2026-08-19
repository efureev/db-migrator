package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	migrator "github.com/efureev/db-migrator/v2"
)

// TestSplitAcceptsGlobalsOnEitherSide is the trap this parser exists to avoid.
//
// The obvious implementation gives every subcommand FlagSet its own copy of the
// global flags, and then Parse resets to the defaults anything that was given
// before the command name. Here only the flags actually typed are collected, so
// which side of the command name they appeared on makes no difference.
func TestSplitAcceptsGlobalsOnEitherSide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		args     []string
		wantCmd  string
		wantRest []string
		wantFlag map[string]string
	}{
		{
			name: "before the command", args: []string{"--schema", "app", "up"},
			wantCmd: "up", wantRest: []string{}, wantFlag: map[string]string{"schema": "app"},
		},
		{
			name: "after the command", args: []string{"up", "--schema", "app"},
			wantCmd: "up", wantRest: []string{}, wantFlag: map[string]string{"schema": "app"},
		},
		{
			name: "both sides", args: []string{"--dir", "./m", "up", "--schema", "app"},
			wantCmd: "up", wantRest: []string{},
			wantFlag: map[string]string{"dir": "./m", "schema": "app"},
		},
		{
			name: "equals form", args: []string{"up", "--schema=app"},
			wantCmd: "up", wantRest: []string{}, wantFlag: map[string]string{"schema": "app"},
		},
		{
			name: "short alias", args: []string{"-d", "./m", "status"},
			wantCmd: "status", wantRest: []string{}, wantFlag: map[string]string{"dir": "./m"},
		},
		{
			name: "boolean needs no value", args: []string{"up", "--json"},
			wantCmd: "up", wantRest: []string{}, wantFlag: map[string]string{"json": "true"},
		},
		{
			name: "subcommand flags are passed through", args: []string{"up", "--steps", "2", "--json"},
			wantCmd: "up", wantRest: []string{"--steps", "2"},
			wantFlag: map[string]string{"json": "true"},
		},
		{
			name: "positional arguments survive", args: []string{"create", "add", "users"},
			wantCmd: "create", wantRest: []string{"add", "users"}, wantFlag: map[string]string{},
		},
		{
			name: "double dash ends parsing", args: []string{"create", "--", "--not-a-flag"},
			wantCmd: "create", wantRest: []string{"--not-a-flag"}, wantFlag: map[string]string{},
		},
		{
			name: "bare help", args: []string{},
			wantCmd: "", wantRest: []string{}, wantFlag: map[string]string{},
		},
		{
			name: "-h alone is help", args: []string{"-h"},
			wantCmd: "help", wantRest: []string{}, wantFlag: map[string]string{},
		},
		{
			name: "-v alone is version", args: []string{"-v"},
			wantCmd: "version", wantRest: []string{}, wantFlag: map[string]string{},
		},
		{
			name: "help for a command", args: []string{"up", "--help"},
			wantCmd: "up", wantRest: []string{"--help"}, wantFlag: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			name, rest, flags, err := split(tc.args)
			if err != nil {
				t.Fatalf("split(%q): %v", tc.args, err)
			}

			if name != tc.wantCmd {
				t.Errorf("command = %q, want %q", name, tc.wantCmd)
			}

			if fmt.Sprint(rest) != fmt.Sprint(tc.wantRest) {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}

			if len(flags) != len(tc.wantFlag) {
				t.Fatalf("flags = %v, want %v", flags, tc.wantFlag)
			}

			for k, v := range tc.wantFlag {
				if flags[k] != v {
					t.Errorf("flags[%q] = %q, want %q", k, flags[k], v)
				}
			}
		})
	}
}

func TestSplitReportsAMissingValue(t *testing.T) {
	t.Parallel()

	if _, _, _, err := split([]string{"up", "--schema"}); err == nil {
		t.Fatal("split accepted a flag with no value")
	} else if ExitCode(err) != ExitUsage {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitUsage)
	}
}

// TestExitCodes has a row per line of the table in --help. A code that is
// documented and not produced is worse than one that is neither.
func TestExitCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, ExitOK},
		{"unknown failure", errors.New("boom"), ExitFailure},
		{"usage", usageErrorf("nope"), ExitUsage},
		{"pending", &pendingError{n: 2}, ExitPending},

		{"checksum drift", migrator.ErrChecksumMismatch, ExitDrift},
		{"missing migration", migrator.ErrMissingMigration, ExitDrift},
		{"incomplete", migrator.ErrIncomplete, ExitDrift},
		{"out of order", migrator.ErrOutOfOrder, ExitDrift},
		{"unresolved placeholder", migrator.ErrUnresolvedPlaceholder, ExitDrift},

		{"lock held", migrator.ErrLockTimeout, ExitLocked},
		{"not pinned", migrator.ErrSessionNotPinned, ExitLocked},

		{"down not allowed", migrator.ErrDownNotAllowed, ExitRefused},
		{"production", migrator.ErrProductionGuard, ExitRefused},
		{"wipe refused", migrator.ErrWipeRefused, ExitRefused},
		{"not confirmed", migrator.ErrNotConfirmed, ExitRefused},

		{"no source", migrator.ErrNoSource, ExitUsage},
		{"bad filename", migrator.ErrBadFilename, ExitUsage},
		{"duplicate version", migrator.ErrDuplicateVersion, ExitUsage},
		{"unknown directive", migrator.ErrUnknownDirective, ExitUsage},
		{"missing down file", migrator.ErrMissingDownFile, ExitUsage},
		{"bad name", migrator.ErrBadName, ExitUsage},

		{"interrupted", context.Canceled, ExitInterrupted},

		// Wrapping must not change the answer: the library wraps its sentinels
		// with context, and the mapping is by errors.Is for exactly that reason.
		{"wrapped", fmt.Errorf("while running: %w", migrator.ErrLockTimeout), ExitLocked},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestGuardsOutrankDrift: a refusal and a drift can both be true of one error,
// and the refusal is the more useful thing to report — retrying is pointless
// either way, but only one of them means "change the pipeline".
func TestGuardsOutrankDrift(t *testing.T) {
	t.Parallel()

	err := errors.Join(migrator.ErrProductionGuard, migrator.ErrChecksumMismatch)
	if got := ExitCode(err); got != ExitRefused {
		t.Errorf("ExitCode = %d, want %d", got, ExitRefused)
	}
}

func TestInferEnvironment(t *testing.T) {
	t.Parallel()

	cases := map[string]migrator.Environment{
		// Nothing configured is a developer's machine far more often than not.
		"": migrator.EnvDevelopment,

		"postgres://app@localhost:5432/shop":            migrator.EnvDevelopment,
		"postgres://app@127.0.0.1:5432/shop":            migrator.EnvDevelopment,
		"postgres://app@[::1]:5432/shop":                migrator.EnvDevelopment,
		"postgres://app@10.0.0.5:5432/shop":             migrator.EnvDevelopment,
		"postgres://app@192.168.1.10:5432/shop":         migrator.EnvDevelopment,
		"postgres://app@host.docker.internal:5432/shop": migrator.EnvDevelopment,
		"host=localhost dbname=shop":                    migrator.EnvDevelopment,

		// A routable host, or a name that says production, reads as production.
		// The lean is deliberate: guessing wrong this way costs a refusal, and
		// guessing wrong the other way costs a schema.
		"postgres://app@db.internal:5432/shop":      migrator.EnvProduction,
		"postgres://app@203.0.113.7:5432/shop":      migrator.EnvProduction,
		"postgres://app@localhost:5432/shop_prod":   migrator.EnvProduction,
		"postgres://app@localhost:5432/live_orders": migrator.EnvProduction,
		"host=localhost dbname=production":          migrator.EnvProduction,
	}

	for dsn, want := range cases {
		if got := inferEnvironment(dsn); got != want {
			t.Errorf("inferEnvironment(%q) = %v, want %v", dsn, got, want)
		}
	}
}

func TestHostAndDatabase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dsn, host, database string
	}{
		{"postgres://app@db:5432/shop", "db", "shop"},
		{"postgresql://app@db/shop?sslmode=require", "db", "shop"},
		{"host=db port=5432 dbname=shop user=app", "db", "shop"},
		{"", "", ""},
		{"nonsense", "", ""},
	}

	for _, tc := range cases {
		host, database := hostAndDatabase(tc.dsn)
		if host != tc.host || database != tc.database {
			t.Errorf("hostAndDatabase(%q) = %q, %q; want %q, %q",
				tc.dsn, host, database, tc.host, tc.database)
		}
	}
}

func TestWantsHelp(t *testing.T) {
	t.Parallel()

	if !wantsHelp([]string{"--steps", "1", "--help"}) {
		t.Error("--help was not recognised")
	}

	if !wantsHelp([]string{"-h"}) {
		t.Error("-h was not recognised")
	}

	if wantsHelp([]string{"--steps", "1"}) {
		t.Error("help was recognised where none was asked for")
	}
}

// TestHelpIsWrittenToStdout: help is an answer, not a diagnostic, so it belongs
// on standard output where a pager or a pipe can reach it.
func TestHelpIsWrittenToStdout(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"--help"}, {"help"}, {"up", "--help"}, {"help", "up"}} {
		var out, errw bytes.Buffer

		code := Run(args, Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
		if code != ExitOK {
			t.Errorf("%q exited %d, want %d", args, code, ExitOK)
		}

		if out.Len() == 0 {
			t.Errorf("%q wrote nothing to stdout", args)
		}

		if errw.Len() != 0 {
			t.Errorf("%q wrote to stderr: %q", args, errw.String())
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	t.Parallel()

	var out, errw bytes.Buffer

	code := Run([]string{"nosuch"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}

	if !strings.Contains(errw.String(), "nosuch") {
		t.Errorf("stderr does not name the command: %q", errw.String())
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	var out, errw bytes.Buffer

	code := Run([]string{"version"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}

	if !strings.Contains(out.String(), "migrator") {
		t.Errorf("version wrote %q", out.String())
	}
}

// TestCreateNeedsNoDatabase is the property the needsDB field exists to keep
// true: writing a migration is something people do before the connection
// details exist.
func TestCreateNeedsNoDatabase(t *testing.T) {
	dir := t.TempDir()

	t.Chdir(dir)
	t.Setenv("MIGRATOR_DSN", "postgres://nobody@203.0.113.99:1/none")

	var out, errw bytes.Buffer

	code := Run([]string{"create", "add users", "--dir", "./m"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, errw.String())
	}

	if !strings.Contains(out.String(), "add_users.up.sql") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestCreateWithoutANameIsUsage(t *testing.T) {
	t.Chdir(t.TempDir())

	var out, errw bytes.Buffer

	code := Run([]string{"create"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
}

// TestValidateOffline needs no database either, which is what makes it usable
// in a pre-commit hook.
func TestValidateOffline(t *testing.T) {
	dir := t.TempDir()

	t.Chdir(dir)

	var out, errw bytes.Buffer

	// A directory with a misnamed file: the check must fail, and say why.
	if err := writeFiles(dir, map[string]string{"m/CreateUsers.up.sql": "SELECT 1;"}); err != nil {
		t.Fatal(err)
	}

	code := Run([]string{"validate", "--offline", "--dir", "./m"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitUsage, errw.String())
	}

	// A well-formed directory passes, and the missing down file is a warning.
	out.Reset()
	errw.Reset()

	if err := writeFiles(dir, map[string]string{
		"ok/1_a.up.sql": "SELECT 1;",
	}); err != nil {
		t.Fatal(err)
	}

	code = Run([]string{"validate", "--offline", "--dir", "./ok"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, errw.String())
	}

	// --strict promotes it to an error.
	code = Run([]string{"validate", "--offline", "--strict", "--dir", "./ok"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitUsage {
		t.Errorf("--strict exit code = %d, want %d", code, ExitUsage)
	}
}

func TestQuietAndVerboseContradict(t *testing.T) {
	t.Chdir(t.TempDir())

	var out, errw bytes.Buffer

	code := Run([]string{"--quiet", "--verbose", "status"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errw})
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
}
