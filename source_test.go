package migrator

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// fsOf builds an in-memory source. Every source-level test runs on one of
// these: the parsing half of this package touches neither a disk nor a
// database, and its tests should not either.
func fsOf(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}

	return out
}

func mustLoad(t *testing.T, files map[string]string) *Set {
	t.Helper()

	set, err := Load(fsOf(files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return set
}

func TestLoad(t *testing.T) {
	t.Parallel()

	set := mustLoad(t, map[string]string{
		"0002_add_email.up.sql":    "ALTER TABLE users ADD COLUMN email text;",
		"0002_add_email.down.sql":  "ALTER TABLE users DROP COLUMN email;",
		"0001_create_users.up.sql": "CREATE TABLE users (id int);",
		"10_later.up.sql":          "SELECT 1;",
		"readme.md":                "ignored: not a .sql file",
	})

	if got := set.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}

	// Ascending by version, not by file name: "10" sorts before "0002"
	// lexically and after it numerically, and numerically is what runs.
	want := []int64{1, 2, 10}
	if got := set.Versions(); len(got) != len(want) {
		t.Fatalf("Versions = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Versions = %v, want %v", got, want)
			}
		}
	}

	m, ok := set.ByVersion(2)
	if !ok {
		t.Fatal("ByVersion(2) not found")
	}

	if !m.HasDown() {
		t.Error("version 2 ships a down file and HasDown says otherwise")
	}

	if m.Name != "add_email" || m.String() != "2_add_email" {
		t.Errorf("migration = %q named %q", m, m.Name)
	}

	if first, _ := set.ByVersion(1); first.HasDown() {
		t.Error("version 1 ships no down file and HasDown says it does")
	}

	if latest, ok := set.Latest(); !ok || latest.Version != 10 {
		t.Errorf("Latest = %v, %v; want version 10", latest.Version, ok)
	}

	if _, ok := set.ByVersion(999); ok {
		t.Error("ByVersion(999) found something")
	}
}

func TestLoadProblems(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		files map[string]string
		want  error
	}{
		{
			name:  "empty directory",
			files: map[string]string{},
			want:  ErrNoSource,
		},
		{
			name:  "only non-sql files",
			files: map[string]string{"readme.md": "x"},
			want:  ErrNoSource,
		},
		{
			name:  "bad file name",
			files: map[string]string{"CreateUsers.up.sql": "SELECT 1;"},
			want:  ErrBadFilename,
		},
		{
			name: "two files claim one version",
			files: map[string]string{
				"0001_a.up.sql": "SELECT 1;",
				"1_b.up.sql":    "SELECT 2;",
			},
			want: ErrDuplicateVersion,
		},
		{
			name: "same version and half twice",
			files: map[string]string{
				"0001_a.up.sql": "SELECT 1;",
				"1_a.up.sql":    "SELECT 2;",
			},
			want: ErrDuplicateVersion,
		},
		{
			name:  "down file with no up file",
			files: map[string]string{"0001_a.down.sql": "DROP TABLE a;"},
			want:  ErrOrphanDownFile,
		},
		{
			name:  "empty up file",
			files: map[string]string{"0001_a.up.sql": "\n\n-- nothing here\n"},
			want:  ErrEmptyMigration,
		},
		{
			name:  "unterminated quote",
			files: map[string]string{"0001_a.up.sql": "SELECT 'a;"},
			want:  ErrUnterminated,
		},
		{
			name:  "unknown directive",
			files: map[string]string{"0001_a.up.sql": "-- migrator:no-transacton\nSELECT 1;"},
			want:  ErrUnknownDirective,
		},
		{
			name:  "malformed directive argument",
			files: map[string]string{"0001_a.up.sql": "-- migrator:lock-timeout soon\nSELECT 1;"},
			want:  ErrBadDirective,
		},
		{
			name: "problem in the down file too",
			files: map[string]string{
				"0001_a.up.sql":   "SELECT 1;",
				"0001_a.down.sql": "-- migrator:nope\nSELECT 2;",
			},
			want: ErrUnknownDirective,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(fsOf(tc.files))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLoadReportsEveryProblem is the property that makes a bad directory
// fixable in one pass instead of one restart per file.
func TestLoadReportsEveryProblem(t *testing.T) {
	t.Parallel()

	_, err := Load(fsOf(map[string]string{
		"CreateUsers.up.sql": "SELECT 1;",
		"0001_a.up.sql":      "\n",
		"0002_b.up.sql":      "-- migrator:nope\nSELECT 1;",
		"0003_c.down.sql":    "DROP TABLE c;",
	}))
	if err == nil {
		t.Fatal("Load accepted four broken files")
	}

	for _, want := range []error{
		ErrBadFilename, ErrEmptyMigration, ErrUnknownDirective, ErrOrphanDownFile,
	} {
		if !errors.Is(err, want) {
			t.Errorf("joined error does not report %v:\n%v", want, err)
		}
	}

	// Every problem names its file, or it is not actionable.
	for _, file := range []string{"CreateUsers.up.sql", "0001_a.up.sql", "0002_b.up.sql", "0003_c.down.sql"} {
		if !strings.Contains(err.Error(), file) {
			t.Errorf("joined error does not name %s:\n%v", file, err)
		}
	}
}

func TestNormalise(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"SELECT 1;\n":                   "SELECT 1;",
		"SELECT 1;\r\n":                 "SELECT 1;",
		"SELECT 1;\r":                   "SELECT 1;",
		"a;\r\nb;\r\n":                  "a;\nb;",
		"a;   \nb;\t\t\n":               "a;\nb;",
		"\ufeffSELECT 1;":               "SELECT 1;",
		"SELECT 1;\n\n\n":               "SELECT 1;",
		"":                              "",
		"\n\n":                          "",
		"a\n\nb":                        "a\n\nb", // blank lines inside are content
		"  leading space is kept":       "  leading space is kept",
		"trailing on last line kept?  ": "trailing on last line kept?",
	}

	for in, want := range cases {
		if got := Normalise([]byte(in)); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestChecksumIgnoresLineEndings is the whole reason Normalise exists: a
// Windows checkout with core.autocrlf=true must not look like somebody editing
// a released migration.
func TestChecksumIgnoresLineEndings(t *testing.T) {
	t.Parallel()

	unix := []byte("CREATE TABLE a (\n  id int\n);\n")
	windows := []byte("CREATE TABLE a (\r\n  id int\r\n);\r\n")
	withBOM := append([]byte("\ufeff"), unix...)
	trailing := []byte("CREATE TABLE a (\n  id int\n);\n\n\n")
	spaces := []byte("CREATE TABLE a (   \n  id int\t\n);\n")

	want := Checksum(unix)
	for name, body := range map[string][]byte{
		"CRLF": windows, "BOM": withBOM, "trailing newlines": trailing, "trailing spaces": spaces,
	} {
		if got := Checksum(body); got != want {
			t.Errorf("%s changed the checksum: %s vs %s", name, got, want)
		}
	}

	// A real edit must still change it, or the mechanism protects nothing.
	if Checksum([]byte("CREATE TABLE a (\n  id bigint\n);\n")) == want {
		t.Error("changing int to bigint did not change the checksum")
	}
}

// TestChecksumIsStable pins the hash of a known body. If this test fails, the
// normalisation algorithm changed — which invalidates every checksum recorded
// in every database and is a breaking change by definition.
func TestChecksumIsStable(t *testing.T) {
	t.Parallel()

	// The expected value is not this package's own output copied back in: it
	// was computed by an independent implementation of the documented
	// normalisation (strip BOM, CRLF to LF, trim trailing blanks per line, trim
	// trailing newlines) followed by sha256. That is what makes it a check of
	// the contract rather than a check that the code equals itself.
	const (
		body = "CREATE TABLE users (id int);\n"
		want = "a15ebcab704727eefd822a74c96ecc837377c7f9028269f520d6d24ff372f0f2"
	)

	got := Checksum([]byte(body))
	if got != want {
		t.Errorf("Checksum(%q) = %q\n"+
			"want %q\n"+
			"If this is intentional, it is a BREAKING change: every checksum "+
			"recorded in every database stops matching.", body, got, want)
	}
}

func TestParseDirectives(t *testing.T) {
	t.Parallel()

	t.Run("none", func(t *testing.T) {
		t.Parallel()

		d, err := ParseDirectives("SELECT 1;")
		if err != nil {
			t.Fatal(err)
		}

		if d.NoTransaction || d.RetrySafe || d.StatementTimeout != 0 || len(d.Tags) != 0 {
			t.Errorf("empty body produced %+v", d)
		}
	})

	t.Run("all of them", func(t *testing.T) {
		t.Parallel()

		d, err := ParseDirectives(
			"-- migrator:no-transaction\n" +
				"-- migrator:retry-safe\n" +
				"-- migrator:statement-timeout 30m\n" +
				"-- migrator:lock-timeout 5s\n" +
				"-- migrator:tags ddl, slow ,\n" +
				"CREATE INDEX CONCURRENTLY i ON t (c);")
		if err != nil {
			t.Fatal(err)
		}

		if !d.NoTransaction || !d.RetrySafe {
			t.Errorf("flags not set: %+v", d)
		}

		if d.StatementTimeout != 30*time.Minute {
			t.Errorf("StatementTimeout = %v, want 30m", d.StatementTimeout)
		}

		if d.LockTimeout != 5*time.Second {
			t.Errorf("LockTimeout = %v, want 5s", d.LockTimeout)
		}

		if len(d.Tags) != 2 || d.Tags[0] != "ddl" || d.Tags[1] != "slow" {
			t.Errorf("Tags = %q, want [ddl slow]", d.Tags)
		}
	})

	t.Run("only the head is read", func(t *testing.T) {
		t.Parallel()

		// A directive below the first SQL token cannot retroactively change how
		// the statements above it ran, so it is not a directive at all.
		d, err := ParseDirectives("SELECT 1;\n-- migrator:no-transaction\nSELECT 2;")
		if err != nil {
			t.Fatal(err)
		}

		if d.NoTransaction {
			t.Error("a directive after the first statement was honoured")
		}
	})

	t.Run("ordinary comments are left alone", func(t *testing.T) {
		t.Parallel()

		d, err := ParseDirectives("-- add the index the report needs\nSELECT 1;")
		if err != nil {
			t.Fatal(err)
		}

		if d.NoTransaction {
			t.Error("a plain comment set a flag")
		}
	})

	t.Run("unknown directive is an error", func(t *testing.T) {
		t.Parallel()

		_, err := ParseDirectives("-- migrator:no-transacton\nSELECT 1;")
		if !errors.Is(err, ErrUnknownDirective) {
			t.Fatalf("error = %v, want %v", err, ErrUnknownDirective)
		}

		// The typo itself must appear, or the message is unusable.
		if !strings.Contains(err.Error(), "no-transacton") {
			t.Errorf("error %q does not quote the directive", err)
		}
	})

	t.Run("every bad directive is reported", func(t *testing.T) {
		t.Parallel()

		_, err := ParseDirectives(
			"-- migrator:nope\n-- migrator:lock-timeout soon\nSELECT 1;")

		for _, want := range []error{ErrUnknownDirective, ErrBadDirective} {
			if !errors.Is(err, want) {
				t.Errorf("error %v does not report %v", err, want)
			}
		}
	})
}

// TestDirectivesAreInTheChecksum pins a decision: adding no-transaction to a
// released file changes how it executes, so it must read as an edit.
func TestDirectivesAreInTheChecksum(t *testing.T) {
	t.Parallel()

	plain := []byte("CREATE INDEX i ON t (c);\n")
	directed := []byte("-- migrator:no-transaction\nCREATE INDEX i ON t (c);\n")

	if Checksum(plain) == Checksum(directed) {
		t.Error("adding a directive did not change the checksum")
	}
}
