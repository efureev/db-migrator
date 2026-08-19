package migrator

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/efureev/db-migrator/v2/internal/naming"
	"github.com/efureev/db-migrator/v2/internal/sqlsplit"
)

// A Migration is one versioned pair of files.
type Migration struct {
	// Version is the numeric prefix shared by both halves.
	Version int64
	// Name is the descriptive half of the file name.
	Name string
	// UpFile and DownFile are the names as they appear in the source. DownFile
	// is empty when the migration ships no down file, which is allowed: a
	// project that has decided it does not roll back must still be able to
	// build a Migrator.
	UpFile   string
	DownFile string
	// Checksum is the hex sha256 of the normalised up file. See [Normalise] for
	// what normalised means and why it is part of the contract.
	Checksum string
	// DownChecksum is the same for the down file, or empty when there is none.
	DownChecksum string
	// Directives are the "-- migrator:" lines at the head of the up file.
	Directives Directives
	// DownDirectives are the down file's own. CREATE INDEX CONCURRENTLY needs
	// no-transaction and so does DROP INDEX CONCURRENTLY, and each says so in
	// its own file.
	DownDirectives Directives

	up, down string
}

// UpSQL reports the body of the up file.
func (m Migration) UpSQL() string { return m.up }

// DownSQL reports the body of the down file, or "" when there is none.
func (m Migration) DownSQL() string { return m.down }

// HasDown reports whether the migration ships a down file.
func (m Migration) HasDown() bool { return m.DownFile != "" }

// String reports the migration as it appears in output: "0003_notify".
func (m Migration) String() string {
	return fmt.Sprintf("%d_%s", m.Version, m.Name)
}

// A Set is an ordered, validated collection of migrations.
type Set struct {
	migrations []Migration
}

// Len reports how many migrations the set holds.
func (s *Set) Len() int { return len(s.migrations) }

// All iterates the set in ascending version order.
func (s *Set) All() iter.Seq[Migration] { return slices.Values(s.migrations) }

// Versions reports every version in the set, ascending.
func (s *Set) Versions() []int64 {
	out := make([]int64, len(s.migrations))
	for i, m := range s.migrations {
		out[i] = m.Version
	}

	return out
}

// ByVersion reports the migration with the given version.
func (s *Set) ByVersion(v int64) (Migration, bool) {
	i, ok := slices.BinarySearchFunc(s.migrations, v, func(m Migration, v int64) int {
		switch {
		case m.Version < v:
			return -1
		case m.Version > v:
			return 1
		default:
			return 0
		}
	})
	if !ok {
		return Migration{}, false
	}

	return s.migrations[i], true
}

// Latest reports the highest version in the set.
func (s *Set) Latest() (Migration, bool) {
	if len(s.migrations) == 0 {
		return Migration{}, false
	}

	return s.migrations[len(s.migrations)-1], true
}

// Load reads every migration in fsys and validates the set as a whole.
//
// It reads the directory itself, not below it: an embed.FS built from
// "//go:embed *.sql" is flat, and a nested layout invites two files claiming
// one version from two directories. Point Load at the directory that holds the
// files — os.DirFS("./migrations") or fs.Sub(embedded, "migrations").
//
// Every problem found is reported, joined into one error, rather than the first
// one: fixing a directory one error per run turns a minute into a quarter of an
// hour. Each is a [SourceError] naming its file and wrapping a sentinel, so
// errors.Is still matches through the join.
func Load(fsys fs.FS) (*Set, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrator: read migrations directory: %w", err)
	}

	var (
		problems []error
		halves   = map[int64]map[naming.Half]naming.File{}
		claimed  = map[int64]string{}
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		f, err := naming.Parse(e.Name())
		if err != nil {
			// Detail carries only what ErrBadFilename does not already say.
			// naming.Parse quotes the file name and so does SourceError, and
			// an error that names the same file twice reads as a bug.
			detail := ""
			if errors.Is(err, naming.ErrVersionRange) {
				detail = naming.ErrVersionRange.Error()
			}

			problems = append(problems, &SourceError{
				File: e.Name(), Detail: detail, Err: ErrBadFilename,
			})

			continue
		}

		if prev, dup := claimed[f.Version]; dup && prev != f.Name {
			// Numerically equal versions collide even when spelled differently,
			// so 0001_a and 1_b are a duplicate rather than one of them winning.
			problems = append(problems, &SourceError{
				File:   e.Name(),
				Detail: fmt.Sprintf("version %d is already claimed by %s", f.Version, prev),
				Err:    ErrDuplicateVersion,
			})

			continue
		}

		claimed[f.Version] = f.Name

		if halves[f.Version] == nil {
			halves[f.Version] = map[naming.Half]naming.File{}
		}

		if prev, dup := halves[f.Version][f.Half]; dup {
			problems = append(problems, &SourceError{
				File:   e.Name(),
				Detail: prev.Raw + " claims the same version and half",
				Err:    ErrDuplicateVersion,
			})

			continue
		}

		halves[f.Version][f.Half] = f
	}

	set := &Set{migrations: make([]Migration, 0, len(halves))}

	for version, pair := range halves {
		up, ok := pair[naming.Up]
		if !ok {
			problems = append(problems, &SourceError{
				File: pair[naming.Down].Raw, Err: ErrOrphanDownFile,
			})

			continue
		}

		m, errs := readMigration(fsys, version, up, pair[naming.Down], len(pair) == 2)
		if len(errs) > 0 {
			problems = append(problems, errs...)

			continue
		}

		set.migrations = append(set.migrations, m)
	}

	// cmp.Compare rather than a subtraction: the difference of two int64
	// versions overflows, and a comparator that overflows sorts wrongly for
	// exactly the large timestamps this format encourages.
	slices.SortFunc(set.migrations, func(a, b Migration) int {
		return cmp.Compare(a.Version, b.Version)
	})

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	if len(set.migrations) == 0 {
		return nil, ErrNoSource
	}

	return set, nil
}

// readMigration reads and validates one pair of files.
func readMigration(fsys fs.FS, version int64, up, down naming.File, hasDown bool) (Migration, []error) {
	var problems []error

	upBody, err := fs.ReadFile(fsys, up.Raw)
	if err != nil {
		return Migration{}, []error{&SourceError{File: up.Raw, Detail: err.Error(), Err: ErrBadFilename}}
	}

	upText := Normalise(upBody)

	stmts, err := sqlsplit.Split(upText)
	if err != nil {
		problems = append(problems, &SourceError{
			File: up.Raw, Detail: unwrapDetail(err), Err: ErrUnterminated,
		})
	}

	if err == nil && len(stmts) == 0 {
		// An empty up file is a mistake worth catching now: it would be
		// recorded as applied, and the version would then be permanently
		// spent on nothing.
		problems = append(problems, &SourceError{File: up.Raw, Err: ErrEmptyMigration})
	}

	directives, err := ParseDirectives(upText)
	if err != nil {
		problems = append(problems, &SourceError{File: up.Raw, Err: err})
	}

	m := Migration{
		Version:    version,
		Name:       up.Name,
		UpFile:     up.Raw,
		Checksum:   Checksum(upBody),
		Directives: directives,
		up:         upText,
	}

	if hasDown {
		downBody, err := fs.ReadFile(fsys, down.Raw)
		if err != nil {
			problems = append(problems, &SourceError{
				File: down.Raw, Detail: err.Error(), Err: ErrBadFilename,
			})

			return m, problems
		}

		downText := Normalise(downBody)

		if _, err := sqlsplit.Split(downText); err != nil {
			problems = append(problems, &SourceError{
				File: down.Raw, Detail: unwrapDetail(err), Err: ErrUnterminated,
			})
		}

		downDirectives, err := ParseDirectives(downText)
		if err != nil {
			problems = append(problems, &SourceError{File: down.Raw, Err: err})
		}

		m.DownFile = down.Raw
		m.DownChecksum = Checksum(downBody)
		m.DownDirectives = downDirectives
		m.down = downText
	}

	return m, problems
}

// Normalise rewrites a migration body into the canonical form the checksum is
// taken over: no BOM, LF line endings, no trailing whitespace on any line and
// none at the end of the file.
//
// This exists because a Windows checkout with core.autocrlf=true otherwise
// produces a checksum mismatch caused by nothing but line endings — and a
// mismatch that is not a real change teaches people to reach for the override,
// which is the one habit this whole mechanism cannot survive.
//
// The algorithm is part of the contract and cannot change without a new major
// version: changing it invalidates every checksum recorded in every database.
func Normalise(body []byte) string {
	s := strings.TrimPrefix(string(body), "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}

	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// Checksum reports the hex sha256 of a normalised migration body.
//
// It covers the file, not the SQL that is finally sent: pointing two
// deployments at two schemas through placeholders must not look like somebody
// having edited a released file.
func Checksum(body []byte) string {
	sum := sha256.Sum256([]byte(Normalise(body)))

	return hex.EncodeToString(sum[:])
}

// Directives are the "-- migrator:" lines at the head of a migration file.
type Directives struct {
	// NoTransaction runs the migration outside a transaction, which is what
	// CREATE INDEX CONCURRENTLY and ALTER TYPE ... ADD VALUE require.
	NoTransaction bool
	// RetrySafe declares the migration idempotent, so that a run left
	// incomplete by a crash is re-executed instead of refused. It means nothing
	// without NoTransaction, and it is a claim only the author of the migration
	// is in a position to make — which is why it lives in the file rather than
	// in a flag somebody reaches for at three in the morning.
	RetrySafe bool
	// StatementTimeout and LockTimeout override the run's settings for this
	// migration. Zero means inherit.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// Tags are free-form labels, reported by status and ignored by the runner.
	Tags []string
}

const directivePrefix = "migrator:"

// ParseDirectives reads the directives at the head of a migration body.
//
// Only the lines before the first SQL token are considered: a directive found
// on line 300 cannot retroactively change how the first 299 lines ran.
//
// An unrecognised directive is an error rather than a warning. A mistyped
// no-transacton leaves CREATE INDEX CONCURRENTLY inside a transaction, where it
// fails with SQLSTATE 25001 if you are lucky and holds a lock across the whole
// migration if you are not. Strictness is free here: the whole source is
// validated before the first connection is opened.
func ParseDirectives(body string) (Directives, error) {
	var (
		d    Directives
		errs []error
	)

	for _, comment := range sqlsplit.LeadingComments(body) {
		line := strings.TrimSpace(comment)
		if !strings.HasPrefix(line, directivePrefix) {
			continue
		}

		name, arg, _ := strings.Cut(strings.TrimPrefix(line, directivePrefix), " ")
		arg = strings.TrimSpace(arg)

		switch name {
		case "no-transaction":
			d.NoTransaction = true

		case "retry-safe":
			d.RetrySafe = true

		case "statement-timeout":
			v, err := time.ParseDuration(arg)
			if err != nil {
				errs = append(errs, fmt.Errorf("%w: statement-timeout %q: %w", ErrBadDirective, arg, err))

				continue
			}

			d.StatementTimeout = v

		case "lock-timeout":
			v, err := time.ParseDuration(arg)
			if err != nil {
				errs = append(errs, fmt.Errorf("%w: lock-timeout %q: %w", ErrBadDirective, arg, err))

				continue
			}

			d.LockTimeout = v

		case "tags":
			for tag := range strings.SplitSeq(arg, ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					d.Tags = append(d.Tags, tag)
				}
			}

		default:
			errs = append(errs, fmt.Errorf("%w: %q", ErrUnknownDirective, name))
		}
	}

	if len(errs) > 0 {
		return d, errors.Join(errs...)
	}

	return d, nil
}

// unwrapDetail reports an error's message without the sentinel prefix, for use
// as the Detail of a [SourceError] that carries the sentinel separately.
func unwrapDetail(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// strconvVersion parses a version rendered by [Create].
func strconvVersion(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrator: version %q: %w", s, err)
	}

	return v, nil
}
