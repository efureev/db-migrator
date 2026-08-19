package buildinfo

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestLdflagsPathIsReal is the test version 1 needed and did not have.
//
// Its build script stamped "migrator/src/commands.version" while the module was
// "github.com/efureev/db-migrator". The linker ignores an unknown -X target
// without a word, so every release for two years shipped a binary that printed
// "unknown (unknown)" — and nothing in the build, the tests or the CI noticed.
//
// The path is a string in a shell script and a Dockerfile, which no compiler
// checks. This does.
func TestLdflagsPathIsReal(t *testing.T) {
	t.Parallel()

	want := reflect.TypeFor[marker]().PkgPath()

	for _, file := range []string{"../../build.sh", "../../Dockerfile"} {
		body, err := os.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				// Dockerfile arrives with the release rails; the build script
				// is what matters and must exist.
				if strings.HasSuffix(file, "build.sh") {
					t.Fatalf("%s is missing", file)
				}

				continue
			}

			t.Fatalf("read %s: %v", file, err)
		}

		// Only the lines that carry an -X flag are examined. Checking the whole
		// file would flag the comment above the flags, which quotes the wrong
		// path from v1 on purpose — and a test that reads prose is a test that
		// fails on an edit to a comment.
		var stamps []string

		for line := range strings.Lines(string(body)) {
			trimmed := strings.TrimSpace(line)

			// Comments are prose, and the prose here quotes the wrong path from
			// v1 on purpose. A test that reads comments fails on an edit to one.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			if strings.Contains(trimmed, "-X ") {
				stamps = append(stamps, trimmed)
			}
		}

		if len(stamps) == 0 {
			t.Errorf("%s stamps no version at all", file)

			continue
		}

		for _, line := range stamps {
			if !strings.Contains(line, want) && !strings.Contains(line, "${LDFLAGS_PKG}") &&
				!strings.Contains(line, "$LDFLAGS_PKG") {
				t.Errorf("%s: %q does not name the real package path %q.\n"+
					"An -X flag pointing at a package that does not exist is ignored "+
					"silently, and the binary then reports its version as unknown.",
					file, line, want)
			}

			if strings.Contains(line, "migrator/src/commands") {
				t.Errorf("%s: %q still stamps the version-1 package path", file, line)
			}
		}

		// The variable the flags interpolate must itself hold the real path.
		if strings.Contains(string(body), "LDFLAGS_PKG") && !strings.Contains(string(body), want) {
			t.Errorf("%s uses LDFLAGS_PKG but never sets it to %q", file, want)
		}
	}
}

// marker exists only so that reflect can report this package's import path.
type marker struct{}

// TestFallbacks: a binary built without ldflags still knows what it is, rather
// than claiming to be "unknown".
func TestFallbacks(t *testing.T) {
	t.Parallel()

	if got := Version(); got == "" {
		t.Error("Version is empty")
	}

	if got := Long(); !strings.HasPrefix(got, "migrator ") {
		t.Errorf("Long = %q", got)
	}

	// Under `go test` there is no main module version, so the fallback applies.
	if got := Version(); got != "dev" && !strings.HasPrefix(got, "v") {
		t.Errorf("Version = %q, want a version or the dev fallback", got)
	}
}

func TestStampedValuesWin(t *testing.T) {
	// Not parallel: it writes the package-level variables the linker sets.
	old := [3]string{version, commit, date}

	t.Cleanup(func() { version, commit, date = old[0], old[1], old[2] })

	version, commit, date = "v2.0.0", "0123456789abcdef", "2026-08-19T00:00:00Z"

	if got := Version(); got != "v2.0.0" {
		t.Errorf("Version = %q", got)
	}

	if got := Commit(); got != "0123456789abcdef" {
		t.Errorf("Commit = %q", got)
	}

	long := Long()
	for _, want := range []string{"v2.0.0", "0123456789ab", "2026-08-19"} {
		if !strings.Contains(long, want) {
			t.Errorf("Long = %q, missing %q", long, want)
		}
	}

	// The commit is shortened for reading, not printed whole.
	if strings.Contains(long, "0123456789abcdef") {
		t.Errorf("Long = %q, want the commit shortened", long)
	}
}
