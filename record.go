package migrator

import (
	"slices"
	"time"
)

// A Record is one row of the bookkeeping table.
type Record struct {
	// Version and Name identify the migration. Name is kept so that "what was
	// version 20240517101122" stays answerable after the file is deleted from
	// the repository.
	Version int64
	Name    string
	// Checksum is the hash of the normalised up file as it was when the
	// migration ran, before placeholder substitution.
	Checksum string
	// DownChecksum is the hash of the down file, kept for reporting only: the
	// gate is on the up file. It lets status say "the down file of this version
	// changed since it was applied" at the moment somebody is about to run it.
	DownChecksum string
	// AppliedAt is when the migration started.
	AppliedAt time.Time
	// FinishedAt is when it was confirmed, and is nil when it never was.
	//
	// On the transactional path it is written in the same transaction as the
	// migration itself and is never nil. On the no-transaction path the row is
	// committed before the SQL runs and updated after, so nil means "started
	// and never came back" — the only durable evidence that a process died
	// between the DDL and the bookkeeping.
	FinishedAt *time.Time
	// ExecutionTime is how long the migration took.
	ExecutionTime time.Duration
	// AppliedBy is who ran it, as reported by the tool: user, host and build.
	AppliedBy string
	// AppliedRole is the PostgreSQL role that ran it, as the server saw it.
	AppliedRole string
	// Transactional reports whether it ran inside a transaction.
	Transactional bool
	// RolledBackAt is when it was rolled back, and is nil while it is in force.
	// A rollback updates this column rather than deleting the row: the history
	// of what ran is worth more than a tidy table.
	RolledBackAt *time.Time
	// ChecksumRepairedAt and ChecksumPrevious record a repair, so that a
	// rewritten checksum leaves a trace instead of quietly becoming the truth.
	ChecksumRepairedAt *time.Time
	ChecksumPrevious   string
	// Migrator is the version of the tool that applied it.
	Migrator string
	// AdoptedAt is set when the row was written by adopt rather than by a run.
	//
	// "Applied" and "we were told it was applied" are different claims, and only
	// one of them was observed here; status has to be able to say which.
	AdoptedAt *time.Time
}

// Adopted reports a migration recorded without having been watched running.
func (r Record) Adopted() bool { return r.AdoptedAt != nil }

// InForce reports whether the migration is part of the current schema: it
// finished and has not been rolled back.
func (r Record) InForce() bool {
	return r.FinishedAt != nil && r.RolledBackAt == nil
}

// Incomplete reports a migration recorded as started and never confirmed.
func (r Record) Incomplete() bool {
	return r.FinishedAt == nil && r.RolledBackAt == nil
}

// A State is where one version stands, comparing the source with the database.
type State uint8

const (
	// StatePending is in the source and not in force in the database.
	StatePending State = iota
	// StateApplied is in both, and the checksums agree.
	StateApplied
	// StateModified is in both and the checksums do not agree: the file changed
	// after it was applied.
	StateModified
	// StateMissing is recorded in the database and absent from the source.
	StateMissing
	// StateRolledBack was applied and then rolled back.
	StateRolledBack
	// StateIncomplete was recorded as started and never confirmed — a
	// no-transaction run that did not come back.
	StateIncomplete
)

// String reports the state as it appears in output.
func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateApplied:
		return "applied"
	case StateModified:
		return "changed"
	case StateMissing:
		return "missing"
	case StateRolledBack:
		return "rolled back"
	case StateIncomplete:
		return "incomplete"
	default:
		return "unknown"
	}
}

// MarshalText implements [encoding.TextMarshaler], so that --json output and a
// terminal use one spelling.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// NeedsAttention reports a state that a person has to resolve: nothing runs
// while one of these is present.
func (s State) NeedsAttention() bool {
	return s == StateModified || s == StateMissing || s == StateIncomplete
}

// An Entry pairs one version's file with its row.
type Entry struct {
	// Version and Name identify the migration. Name comes from the file when
	// there is one and from the row when there is not.
	Version int64
	Name    string
	// State is where this version stands.
	State State
	// Record is the database row, or nil when the version was never applied.
	Record *Record
	// Migration is the file, or nil when the source no longer has it.
	Migration *Migration
}

// A Status is the source and the database side by side.
type Status struct {
	// Schema and Table name the bookkeeping table this status was read from.
	Schema string
	Table  string
	// Initialised reports whether the bookkeeping table exists. When it is
	// false every entry is pending and no row was read — status does not create
	// the table, because a status command that writes is one nobody dares run
	// against production.
	Initialised bool
	// Entries are every version known to either side, ascending.
	Entries []Entry
}

// Current reports the highest version in force, or 0 when there is none.
func (s *Status) Current() int64 {
	var current int64

	for _, e := range s.Entries {
		if e.Record != nil && e.Record.InForce() && e.Version > current {
			current = e.Version
		}
	}

	return current
}

// Pending reports the entries that would run on the next Up.
func (s *Status) Pending() []Entry {
	return slices.Collect(func(yield func(Entry) bool) {
		for _, e := range s.Entries {
			if e.State == StatePending || e.State == StateRolledBack {
				if !yield(e) {
					return
				}
			}
		}
	})
}

// Drifted reports the entries a person has to resolve before anything runs.
func (s *Status) Drifted() []Entry {
	return slices.Collect(func(yield func(Entry) bool) {
		for _, e := range s.Entries {
			if e.State.NeedsAttention() {
				if !yield(e) {
					return
				}
			}
		}
	})
}

// A Version is where the database stands.
type Version struct {
	// Current is the highest version in force, or 0 when nothing is.
	Current int64
	// Name is that version's name.
	Name string
	// AppliedAt is when it was applied.
	AppliedAt time.Time
	// Pending counts the migrations that would run on the next Up.
	Pending int
	// Dirty reports an incomplete no-transaction migration on record. Nothing
	// runs while it is true.
	Dirty bool
}
