package migrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) CreateOption { return WithClock(func() time.Time { return t }) }

var noon = time.Date(2026, 8, 19, 12, 30, 45, 0, time.UTC)

func TestCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	pair, err := Create(dir, "Create Users Table", fixedClock(noon))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if pair.Version != 20260819123045 {
		t.Errorf("version = %d, want 20260819123045", pair.Version)
	}

	if pair.Name != "create_users_table" {
		t.Errorf("name = %q", pair.Name)
	}

	for _, path := range []string{pair.UpPath, pair.DownPath} {
		if path == "" {
			t.Fatal("Create reported an empty path")
		}

		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if len(body) == 0 {
			t.Errorf("%s is empty", path)
		}
	}

	// The template shows the directives, and must not carry them.
	//
	// The first version of it listed them as ordinary comment lines, which is
	// exactly the syntax of a real directive: every migration this tool created
	// silently ran outside a transaction, losing the atomicity of its own
	// bookkeeping — the property the package doc leads with.
	up, err := os.ReadFile(pair.UpPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(up), "no-transaction") {
		t.Error("the up template does not mention the directives at all")
	}

	directives, err := parseDirectives(normalise(up))
	if err != nil {
		t.Fatalf("the template does not parse: %v", err)
	}

	if directives.NoTransaction || directives.RetrySafe ||
		directives.StatementTimeout != 0 || directives.LockTimeout != 0 {
		t.Errorf("the template carries live directives: %+v", directives)
	}

	// What Create writes, Load must accept.
	set, err := Load(os.DirFS(dir))
	if err == nil {
		t.Fatalf("Load accepted a template-only migration: %v", set)
	}

	if !errors.Is(err, ErrEmptyMigration) {
		t.Errorf("Load error = %v, want %v", err, ErrEmptyMigration)
	}
}

// TestCreateProducesLoadableFiles is the property that matters: fill the
// templates in and the result is a set this package reads.
func TestCreateProducesLoadableFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	pair, err := Create(dir, "create users", fixedClock(noon),
		WithTemplate("CREATE TABLE users (id int);\n", "DROP TABLE users;\n"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	set, err := Load(os.DirFS(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	mig, ok := set.ByVersion(pair.Version)
	if !ok {
		t.Fatalf("Load did not find version %d", pair.Version)
	}

	if mig.Name != "create_users" || !mig.HasDown() {
		t.Errorf("loaded %+v", mig)
	}
}

func TestCreateVersionFormats(t *testing.T) {
	t.Parallel()

	t.Run("unix", func(t *testing.T) {
		t.Parallel()

		pair, err := Create(t.TempDir(), "a", fixedClock(noon), WithVersionFormat(VersionUnix))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if pair.Version != noon.Unix() {
			t.Errorf("version = %d, want %d", pair.Version, noon.Unix())
		}

		// A Unix version sorts before a timestamp one both numerically and
		// lexically, so the two formats coexist in one directory. That is what
		// lets a project switch formats without renaming its history.
		if pair.Version >= 20260819123045 {
			t.Error("a unix version does not sort before a timestamp version")
		}
	})

	t.Run("sequential continues from the highest", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		for want := int64(1); want <= 3; want++ {
			pair, err := Create(dir, "step", WithVersionFormat(VersionSequential),
				WithTemplate("SELECT 1;\n", "SELECT 1;\n"))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if pair.Version != want {
				t.Fatalf("version = %d, want %d", pair.Version, want)
			}
		}
	})
}

func TestCreateWithoutDown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	pair, err := Create(dir, "forward only", fixedClock(noon), WithoutDown())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if pair.DownPath != "" {
		t.Errorf("DownPath = %q, want empty", pair.DownPath)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("wrote %d files, want 1", len(entries))
	}
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := Create(dir, "a", fixedClock(noon)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The same clock produces the same version, so the second call collides.
	// Overwriting would silently destroy a migration somebody had written.
	_, err := Create(dir, "a", fixedClock(noon))
	if err == nil {
		t.Fatal("Create overwrote an existing migration")
	}

	if !errors.Is(err, os.ErrExist) {
		t.Errorf("error = %v, want it to wrap os.ErrExist", err)
	}
}

// TestCreateWritesBothOrNeither: a half-written pair claims a version with a
// file that does not exist, and the next Create would take the same number.
func TestCreateWritesBothOrNeither(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Arrange for the down file to fail by putting a directory in its place.
	down := filepath.Join(dir, "20260819123045_a.down.sql")
	if err := os.Mkdir(down, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(dir, "a", fixedClock(noon)); err == nil {
		t.Fatal("Create succeeded despite an unwritable down file")
	}

	if _, err := os.Stat(filepath.Join(dir, "20260819123045_a.up.sql")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the up file survived a failed Create")
	}
}

func TestCreateRejectsAnUnusableName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Snake drops non-ASCII, so a name made only of it produces nothing usable.
	// Reporting that is better than writing a file this package cannot read.
	for _, name := range []string{"", "!!!", "добавить таблицу"} {
		if _, err := Create(dir, name); !errors.Is(err, ErrBadName) {
			t.Errorf("Create(%q) error = %v, want %v", name, err, ErrBadName)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("a rejected name still wrote %d files", len(entries))
	}
}

func TestCreateMakesTheDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "migrations")

	if _, err := Create(dir, "a", fixedClock(noon)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
}

func TestValidateSource(t *testing.T) {
	t.Parallel()

	set := mustLoad(t, map[string]string{
		"1_a.up.sql": "SELECT 1;", "1_a.down.sql": "SELECT 1;",
		"2_b.up.sql": "SELECT 1;", // no down file
	})

	lenient := set.Validate(false)
	if !lenient.OK() {
		t.Errorf("a missing down file is not an error by default: %v", lenient.Problems())
	}

	if len(lenient.Problems()) != 1 || lenient.Problems()[0].Kind != ProblemNoDownFile {
		t.Errorf("problems = %v", lenient.Problems())
	}

	strict := set.Validate(true)
	if strict.OK() {
		t.Error("--strict should turn a missing down file into an error")
	}

	if !errors.Is(strict.Err(), ErrMissingDownFile) {
		t.Errorf("Err = %v, want %v", strict.Err(), ErrMissingDownFile)
	}
}

func TestValidationReportRendering(t *testing.T) {
	t.Parallel()

	r := &ValidationReport{}
	r.add(Problem{
		Severity: SeverityWarning, Kind: ProblemPending,
		Version: 3, File: "3_c.up.sql", Message: "not applied",
	})
	r.add(Problem{
		Severity: SeverityError, Kind: ProblemChecksum,
		Version: 1, File: "1_a.up.sql", Message: "applied as abc, on disk def",
	})
	r.sort()

	// Errors first: the thing that stops a deploy must be the first line, not
	// the tenth.
	if r.problems[0].Severity != SeverityError {
		t.Error("the report does not put errors first")
	}

	if r.OK() {
		t.Error("a report holding an error says it is OK")
	}

	var b strings.Builder
	if err := r.Text(&b); err != nil {
		t.Fatalf("Text: %v", err)
	}

	for _, want := range []string{"1_a.up.sql: error:", "3_c.up.sql: warning:", "1 error(s), 1 warning(s)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, b.String())
		}
	}

	var j strings.Builder
	if err := r.JSON(&j); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	for _, want := range []string{`"ok": false`, `"kind": "checksum"`, `"severity": "error"`} {
		if !strings.Contains(j.String(), want) {
			t.Errorf("JSON does not contain %q:\n%s", want, j.String())
		}
	}

	if !strings.Contains(r.String(), "2 problem") {
		t.Errorf("String = %q", r.String())
	}

	// An empty report is a sentence and an empty array, not silence and null.
	empty := &ValidationReport{}
	if !empty.OK() || empty.Err() != nil || empty.String() != "no problems found" {
		t.Errorf("an empty report reports %v / %v / %q", empty.OK(), empty.Err(), empty)
	}

	var eb, ej strings.Builder
	_ = empty.Text(&eb)
	_ = empty.JSON(&ej)

	if !strings.Contains(eb.String(), "No problems") || !strings.Contains(ej.String(), `"problems": []`) {
		t.Errorf("empty rendering: %q / %q", eb.String(), ej.String())
	}
}

func TestProblemAndSeverityStrings(t *testing.T) {
	t.Parallel()

	kinds := map[ProblemKind]string{
		ProblemChecksum: "checksum", ProblemMissing: "missing", ProblemIncomplete: "incomplete",
		ProblemOutOfOrder: "out-of-order", ProblemNoDownFile: "no-down-file",
		ProblemDownChanged: "down-changed", ProblemPending: "pending",
		ProblemKind(99): "unknown",
	}

	for kind, want := range kinds {
		if got := kind.String(); got != want {
			t.Errorf("ProblemKind(%d) = %q, want %q", kind, got, want)
		}

		b, err := kind.MarshalText()
		if err != nil || string(b) != want {
			t.Errorf("MarshalText = %q, %v", b, err)
		}
	}

	if got := SeverityError.String(); got != "error" {
		t.Errorf("SeverityError = %q", got)
	}

	if got := SeverityWarning.String(); got != "warning" {
		t.Errorf("SeverityWarning = %q", got)
	}

	b, err := SeverityWarning.MarshalText()
	if err != nil || string(b) != "warning" {
		t.Errorf("MarshalText = %q, %v", b, err)
	}

	// A problem with no file names its version, so that a row whose file is
	// gone is still identifiable.
	p := Problem{Severity: SeverityError, Kind: ProblemMissing, Version: 7, Message: "no file"}
	if got := p.String(); !strings.Contains(got, "version 7") {
		t.Errorf("String = %q", got)
	}
}

func TestRepairResultString(t *testing.T) {
	t.Parallel()

	plain := RepairResult{Version: 5, Action: "discard"}
	if got := plain.String(); got != "discard 5" {
		t.Errorf("String = %q", got)
	}

	rehash := RepairResult{
		Version: 5, Action: "rehash",
		Before: "0123456789abcdef0", After: "fedcba9876543210f",
	}

	got := rehash.String()
	for _, want := range []string{"rehash 5", "0123456789ab", "fedcba987654", "->"} {
		if !strings.Contains(got, want) {
			t.Errorf("String = %q, missing %q", got, want)
		}
	}
}
