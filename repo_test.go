package migrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoGoFileIsIgnored catches the class of mistake that shipped v2.0.0
// without its own entry point.
//
// The .gitignore carried the line "migrator", meant for the binary a plain
// `go build` drops in the root. Without a leading slash that pattern matches
// any path component, so it swallowed cmd/migrator/ — the whole command. The
// working tree built and tested green, every gate passed, and the tag went to
// the module proxy with no main package in it. `go install .../cmd/migrator`
// then failed for everybody, and a version in the proxy is immutable: the only
// repair is the next tag.
//
// Nothing else would have caught it. The compiler sees the working tree, the
// linter sees the working tree, and the tests see the working tree. Only git
// disagreed, and nobody was asking git.
//
// The check is about *ignored*, not *untracked*: a file written five minutes
// ago and not yet added is somebody's work in progress, while a source file git
// has been told never to look at is always a mistake.
func TestNoGoFileIsIgnored(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	root, err := repoRoot(ctx)
	if err != nil {
		// Not a checkout: this is the module as the proxy serves it, where the
		// question does not apply.
		t.Skip("not a git checkout:", err)
	}

	var files []string

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(d.Name(), ".go") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}

			files = append(files, rel)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	if len(files) == 0 {
		t.Skip("no Go files found; not a usable checkout")
	}

	// check-ignore --stdin writes, on stdout, exactly those paths that some rule
	// ignores. Exit status 1 means "none of them", which is the good case.
	cmd := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")

	var out bytes.Buffer

	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Skip("git check-ignore is unavailable:", err)
		}
	}

	for line := range strings.Lines(out.String()) {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}

		t.Errorf("%s is ignored by git.\n"+
			"It compiles here and would be absent from a release. A .gitignore "+
			"pattern without a leading slash matches every path component, not "+
			"just the root — \"migrator\" also matches cmd/migrator/.", name)
	}
}

// TestEntryPointIsTracked states the specific case plainly, so that a failure
// names the thing that actually broke rather than a list of files.
func TestEntryPointIsTracked(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	root, err := repoRoot(ctx)
	if err != nil {
		t.Skip("not a git checkout:", err)
	}

	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "cmd/migrator").Output()
	if err != nil {
		t.Skip("git ls-files is unavailable:", err)
	}

	if strings.TrimSpace(string(out)) == "" {
		t.Error("cmd/migrator is not tracked by git: a release built from this " +
			"commit would contain no command, and `go install .../cmd/migrator` " +
			"would fail for every user")
	}
}

// repoRoot reports the top of the checkout this test runs in.
func repoRoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}
