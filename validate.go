package migrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// A Severity says how badly a [Problem] matters.
type Severity uint8

const (
	// SeverityError is a problem that stops a run.
	SeverityError Severity = iota
	// SeverityWarning is a problem worth knowing about that stops nothing.
	SeverityWarning
)

// String reports the severity as compilers and linters spell it.
func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}

	return "error"
}

// MarshalText implements [encoding.TextMarshaler].
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// A ProblemKind says what sort of problem was found.
type ProblemKind uint8

const (
	// ProblemChecksum is a file edited after it was applied.
	ProblemChecksum ProblemKind = iota
	// ProblemMissing is a recorded migration whose file is gone.
	ProblemMissing
	// ProblemIncomplete is a migration started and never confirmed.
	ProblemIncomplete
	// ProblemOutOfOrder is a pending migration below the current version.
	ProblemOutOfOrder
	// ProblemNoDownFile is a migration that ships no rollback.
	ProblemNoDownFile
	// ProblemDownChanged is a down file edited after its migration was applied.
	ProblemDownChanged
	// ProblemPending is a migration that has not been applied. It is a warning:
	// pending migrations are the normal state of a repository between deploys.
	ProblemPending
)

// String reports the kind as it appears in output.
func (k ProblemKind) String() string {
	switch k {
	case ProblemChecksum:
		return "checksum"
	case ProblemMissing:
		return "missing"
	case ProblemIncomplete:
		return "incomplete"
	case ProblemOutOfOrder:
		return "out-of-order"
	case ProblemNoDownFile:
		return "no-down-file"
	case ProblemDownChanged:
		return "down-changed"
	case ProblemPending:
		return "pending"
	default:
		return "unknown"
	}
}

// MarshalText implements [encoding.TextMarshaler].
func (k ProblemKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// A Problem is one finding, in the shape compilers print.
type Problem struct {
	// Severity says whether this stops a run.
	Severity Severity `json:"severity"`
	// Kind says what sort of problem it is.
	Kind ProblemKind `json:"kind"`
	// Version is the migration it concerns, or 0.
	Version int64 `json:"version,omitempty"`
	// File is the file it concerns, or "".
	File string `json:"file,omitempty"`
	// Message says what is wrong, in one sentence.
	Message string `json:"message"`
}

// String reports the problem the way a compiler reports one, so that an editor
// and a person read it the same way.
func (p Problem) String() string {
	where := p.File
	if where == "" {
		where = "version " + strconv.FormatInt(p.Version, 10)
	}

	return where + ": " + p.Severity.String() + ": " + p.Message
}

// A ValidationReport is everything [Migrator.Validate] found.
type ValidationReport struct {
	problems []Problem
}

// OK reports whether nothing of error severity was found.
func (r *ValidationReport) OK() bool {
	return !slices.ContainsFunc(r.problems, func(p Problem) bool {
		return p.Severity == SeverityError
	})
}

// Problems reports every finding, most serious first.
func (r *ValidationReport) Problems() []Problem { return slices.Clone(r.problems) }

// Err reports the error-severity findings joined, or nil.
func (r *ValidationReport) Err() error {
	var errs []error

	for _, p := range r.problems {
		if p.Severity != SeverityError {
			continue
		}

		errs = append(errs, fmt.Errorf("%s: %w", p.String(), sentinelFor(p.Kind)))
	}

	return errors.Join(errs...)
}

// sentinelFor maps a problem kind onto the sentinel a caller matches with
// errors.Is, so that control flow does not have to switch on a string.
func sentinelFor(k ProblemKind) error {
	switch k {
	case ProblemChecksum:
		return ErrChecksumMismatch
	case ProblemMissing:
		return ErrMissingMigration
	case ProblemIncomplete:
		return ErrIncomplete
	case ProblemOutOfOrder:
		return ErrOutOfOrder
	case ProblemNoDownFile:
		return ErrMissingDownFile
	case ProblemPending, ProblemDownChanged:
		return nil
	default:
		return nil
	}
}

// Text writes the report the way a linter writes one.
func (r *ValidationReport) Text(w io.Writer) error {
	if len(r.problems) == 0 {
		_, err := io.WriteString(w, "  No problems found.\n")

		return err
	}

	var b strings.Builder

	for _, p := range r.problems {
		b.WriteString("  ")
		b.WriteString(p.String())
		b.WriteString("\n")
	}

	var errs, warns int

	for _, p := range r.problems {
		if p.Severity == SeverityError {
			errs++
		} else {
			warns++
		}
	}

	fmt.Fprintf(&b, "\n  %d error(s), %d warning(s).\n", errs, warns)

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the report in the shape a CI step parses.
func (r *ValidationReport) JSON(w io.Writer) error {
	out := struct {
		Format   int       `json:"format"`
		OK       bool      `json:"ok"`
		Problems []Problem `json:"problems"`
	}{Format: JSONFormat, OK: r.OK(), Problems: r.problems}

	if out.Problems == nil {
		out.Problems = []Problem{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// String reports a one-line summary.
func (r *ValidationReport) String() string {
	if len(r.problems) == 0 {
		return "no problems found"
	}

	return fmt.Sprintf("%d problem(s) found", len(r.problems))
}

// Validate reports every problem it can find, rather than stopping at the
// first.
//
// Fixing a drifted deployment one restart at a time is how a five-minute job
// becomes an hour, so the whole picture is gathered in one pass. The error
// return is reserved for not being able to check at all — no connection, no
// permission; problems found live in the report.
//
// Pending migrations are reported as warnings, not errors: having migrations
// waiting to be applied is the normal state of a repository between deploys.
func (m *Migrator) Validate(ctx context.Context) (*ValidationReport, error) {
	applied, initialised, err := m.readState(ctx)
	if err != nil {
		return nil, err
	}

	report := &ValidationReport{}
	current := currentVersion(applied)

	for mig := range m.set.All() {
		// Checked for every migration, applied or not. A missing down file is
		// most worth knowing about before the migration runs — at review time,
		// which is when it can still be written — and reporting it only for
		// applied ones would say nothing until it was too late to matter.
		if !mig.HasDown() {
			report.add(Problem{
				Severity: SeverityWarning, Kind: ProblemNoDownFile,
				Version: mig.Version, File: mig.UpFile,
				Message: "ships no down file, so it cannot be rolled back",
			})
		}

		rec, recorded := applied[mig.Version]

		if !recorded || rec.RolledBackAt != nil {
			report.add(Problem{
				Severity: SeverityWarning, Kind: ProblemPending,
				Version: mig.Version, File: mig.UpFile,
				Message: "not applied",
			})

			if mig.Version < current && !m.cfg.allowOutOfOrder {
				report.add(Problem{
					Severity: SeverityError, Kind: ProblemOutOfOrder,
					Version: mig.Version, File: mig.UpFile,
					Message: fmt.Sprintf("pending, but below the current version %d", current),
				})
			}

			continue
		}

		if rec.Incomplete() {
			report.add(Problem{
				Severity: SeverityError, Kind: ProblemIncomplete,
				Version: mig.Version, File: mig.UpFile,
				Message: "started at " + rec.AppliedAt.Format("2006-01-02 15:04:05") +
					" by " + rec.AppliedBy + " and never confirmed",
			})

			continue
		}

		if rec.Checksum != mig.Checksum {
			report.add(Problem{
				Severity: SeverityError, Kind: ProblemChecksum,
				Version: mig.Version, File: mig.UpFile,
				Message: "applied as " + short(rec.Checksum) + ", on disk " + short(mig.Checksum),
			})
		}

		// The down file is reported, never gated on: it does not decide whether
		// the schema is right, only whether a rollback would do what its author
		// intended. Saying so at the moment somebody is about to run it is the
		// whole value.
		if mig.HasDown() && rec.DownChecksum != "" && rec.DownChecksum != mig.DownChecksum {
			report.add(Problem{
				Severity: SeverityWarning, Kind: ProblemDownChanged,
				Version: mig.Version, File: mig.DownFile,
				Message: "changed since the migration was applied",
			})
		}
	}

	if initialised {
		for version, rec := range applied {
			if _, ok := m.set.ByVersion(version); ok {
				continue
			}

			if rec.RolledBackAt != nil {
				continue
			}

			report.add(Problem{
				Severity: SeverityError, Kind: ProblemMissing,
				Version: version,
				Message: "recorded as applied, but no file with this version exists",
			})
		}
	}

	report.sort()

	return report, nil
}

// Validate checks everything that can be checked without a database.
//
// It exists so that `migrator validate --offline` works in a pre-commit hook
// and in a CI job that has no PostgreSQL, where the useful question is "are
// these files well formed", not "does this database match them".
//
// A method on Set rather than a free function taking one: everything it looks
// at is the set, and a package-level ValidateSource(set, …) reads like there is
// some other kind of source it might have taken.
func (s *Set) Validate(strict bool) *ValidationReport {
	report := &ValidationReport{}

	for mig := range s.All() {
		if !mig.HasDown() {
			severity := SeverityWarning
			if strict {
				severity = SeverityError
			}

			report.add(Problem{
				Severity: severity, Kind: ProblemNoDownFile,
				Version: mig.Version, File: mig.UpFile,
				Message: "ships no down file, so it cannot be rolled back",
			})
		}
	}

	report.sort()

	return report
}

func (r *ValidationReport) add(p Problem) { r.problems = append(r.problems, p) }

// sort orders problems by severity and then by version, so that the thing that
// stops a deploy is the first line of output.
func (r *ValidationReport) sort() {
	slices.SortStableFunc(r.problems, func(a, b Problem) int {
		if a.Severity != b.Severity {
			if a.Severity == SeverityError {
				return -1
			}

			return 1
		}

		switch {
		case a.Version < b.Version:
			return -1
		case a.Version > b.Version:
			return 1
		default:
			return 0
		}
	})
}
