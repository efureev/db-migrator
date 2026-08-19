// Package naming parses and builds migration file names.
//
// The format is <version>_<name>.<half>.sql, where version is a decimal number,
// name is lower snake case and half is "up" or "down". The rules are stricter
// than they need to be, on purpose: a name rejected while loading costs a
// second, and a name accepted but sorted differently on another machine costs
// an incident.
package naming

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// A Half says which direction a file holds.
type Half uint8

const (
	// Up is the forward half of a migration.
	Up Half = iota
	// Down is the rollback half.
	Down
)

// String reports the half as it appears in a file name.
func (h Half) String() string {
	if h == Down {
		return "down"
	}

	return "up"
}

// Errors reported by [Parse]. They are unexported sentinels wrapped in the
// error returned; the root package translates them into its own.
var (
	// ErrNotSQL reports a name that is not a .sql file at all.
	ErrNotSQL = errors.New("not a .sql file")
	// ErrShape reports a name that does not match the required shape.
	ErrShape = errors.New("expected <version>_<name>.(up|down).sql")
	// ErrVersionRange reports a version too large to hold in an int64.
	ErrVersionRange = errors.New("version does not fit in a 64-bit integer")
)

// pattern is deliberately anchored, lower-case and exact.
//
//   - The version is decimal and unbounded in width here; the range check
//     happens in Parse, which can say what was wrong.
//   - The name is lower snake case with no leading, trailing or doubled
//     underscore. CreateUsers.up.sql is rejected rather than silently accepted,
//     because a set that sorts by name anywhere must sort the same everywhere.
//   - The extension is lower case. APFS and NTFS are case-insensitive and
//     embed.FS is not, so a capitalised .UP.SQL works on the machine that wrote
//     it and vanishes inside the container — the most expensive shape of
//     failure there is.
var pattern = regexp.MustCompile(`^(\d{1,19})_([a-z0-9]+(?:_[a-z0-9]+)*)\.(up|down)\.sql$`)

// A File is one parsed migration file name.
type File struct {
	// Version is the numeric prefix. Two files whose prefixes are numerically
	// equal claim one version even when they are spelled differently, so 0001
	// and 1 collide rather than one of them being chosen.
	Version int64
	// Name is the descriptive half, in lower snake case.
	Name string
	// Half says whether this is the up or the down file.
	Half Half
	// Raw is the name as it appeared in the source.
	Raw string
}

// Stem reports the version and name without the half or extension, which is how
// a migration is identified in output: "0003_notify".
func (f File) Stem() string {
	return f.Raw[:len(f.Raw)-len(f.Half.String())-len(".sql")-1]
}

// Parse reads one file name.
//
// It never touches the filesystem: the argument is a base name, not a path.
func Parse(filename string) (File, error) {
	if !strings.HasSuffix(filename, ".sql") {
		return File{}, fmt.Errorf("%q: %w", filename, ErrNotSQL)
	}

	m := pattern.FindStringSubmatch(filename)
	if m == nil {
		return File{}, fmt.Errorf("%q: %w", filename, ErrShape)
	}

	// ParseInt rather than a width check in the pattern: 19 digits fit in an
	// int64 only up to 9223372036854775807, and reporting the real reason beats
	// reporting "wrong shape" for a name whose shape is fine.
	version, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return File{}, fmt.Errorf("%q: %w", filename, ErrVersionRange)
	}

	half := Up
	if m[3] == "down" {
		half = Down
	}

	return File{Version: version, Name: m[2], Half: half, Raw: filename}, nil
}

// Format builds a file name from its parts. It is the exact inverse of Parse
// for any File that Parse produced, except for leading zeros in the version,
// which carry no meaning.
func Format(version int64, name string, half Half) string {
	return fmt.Sprintf("%d_%s.%s.sql", version, name, half)
}

// Snake rewrites a human-typed migration name into the lower snake case the
// format requires: "Create Users Table" and "createUsersTable" both become
// "create_users_table".
//
// This is twenty lines rather than a dependency on purpose. A library that
// sells a small dependency tree cannot import a module to insert underscores.
func Snake(s string) string {
	var (
		runes = []rune(s)
		words []string
		word  []rune
	)

	flush := func() {
		if len(word) > 0 {
			words = append(words, string(word))
			word = nil
		}
	}

	for i, r := range runes {
		switch {
		case isUpper(r):
			prevIsWord := i > 0 && (isLower(runes[i-1]) || isDigit(runes[i-1]))
			// The last capital of a run belongs to the word that follows it:
			// HTTPServer is http_server, not https_erver or h_t_t_p_server.
			endsRun := i > 0 && isUpper(runes[i-1]) &&
				i+1 < len(runes) && isLower(runes[i+1])

			if prevIsWord || endsRun {
				flush()
			}

			word = append(word, unicode.ToLower(r))

		case isLower(r) || isDigit(r):
			word = append(word, r)

		default:
			// Anything else is a separator — spaces, dashes, punctuation, and
			// every non-ASCII rune. Runs of them collapse, so neither a leading
			// underscore nor a doubled one can reach the name.
			//
			// Non-ASCII is dropped rather than kept because the file name
			// format is ASCII: unicode.IsLower says yes to "таблица", and
			// keeping it would produce a name that Parse then refuses — a file
			// created by this tool that this tool cannot read. Create reports
			// an empty result as an error instead, which is answerable.
			flush()
		}
	}

	flush()

	return strings.Join(words, "_")
}

// Valid reports whether name is already in the shape Format accepts, which is
// what Create checks before writing files rather than after.
func Valid(name string) bool {
	return namePattern.MatchString(name)
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)

// The classification below is ASCII-only on purpose; see the default branch of
// [Snake]. unicode.IsLower and friends accept every script, and the file name
// format accepts one.
func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }
