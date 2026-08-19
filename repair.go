package migrator

import (
	"context"
	"fmt"
	"slices"
)

// repairKind distinguishes the operations [Migrator.Repair] can perform.
type repairKind uint8

const (
	repairChecksum repairKind = iota
	repairComplete
	repairDiscard
	repairPrune
)

// A RepairOp is one repair.
//
// Repairs are named values rather than flags so that a command line says which
// version it is about to edit. "migrator repair --rehash --version 20240118120000"
// is a sentence somebody can be held to; "migrator up --force" is not.
type RepairOp struct {
	kind    repairKind
	version int64
}

// RepairChecksum rewrites the recorded checksum of v to match the file.
//
// It is the escape hatch for two situations and no others: a released file was
// reformatted in a way that changed its bytes but not its meaning, or a
// database was migrated by a different tool and is being brought under this
// one. In every other case the file is what should be fixed, not the record.
func RepairChecksum(v int64) RepairOp { return RepairOp{kind: repairChecksum, version: v} }

// RepairComplete marks an incomplete no-transaction migration as finished, for
// use after a person has looked at the schema and confirmed it is right.
func RepairComplete(v int64) RepairOp { return RepairOp{kind: repairComplete, version: v} }

// RepairDiscard removes the row for v, so that the migration runs again.
func RepairDiscard(v int64) RepairOp { return RepairOp{kind: repairDiscard, version: v} }

// RepairPrune removes the rows of versions that no longer exist in the source.
func RepairPrune() RepairOp { return RepairOp{kind: repairPrune} }

// A RepairResult is one thing a repair did.
type RepairResult struct {
	// Version is the row it touched.
	Version int64
	// Action is what it did: "rehash", "complete", "discard" or "prune".
	Action string
	// Before and After are the recorded checksum either side of a rehash.
	Before string
	After  string
}

// String reports the result as it appears in output.
func (r RepairResult) String() string {
	s := fmt.Sprintf("%s %d", r.Action, r.Version)
	if r.Before != "" || r.After != "" {
		s += fmt.Sprintf(": %s -> %s", short(r.Before), short(r.After))
	}

	return s
}

// Repair edits the bookkeeping table without touching the schema.
//
// This is the only operation that does, and that is the point of it existing.
// The alternative — a --force flag on the command that applies migrations — is
// reached for in a hurry, at the moment somebody most wants the loud refusal to
// go away, and it turns that refusal into silence. Separating them means the
// dangerous thing has to be typed deliberately, names the version it will edit,
// and prints what it changed.
//
// A rehash records checksum_previous and checksum_repaired_at and leaves
// applied_at and applied_by alone: repairing metadata must not erase the
// history of what actually ran.
func (m *Migrator) Repair(ctx context.Context, ops ...RepairOp) (*Report, error) {
	if len(ops) == 0 {
		return nil, ErrNothingToDo
	}

	report := &Report{Direction: DirectionUp}

	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { s.Release(context.WithoutCancel(ctx)) }()

	release, err := m.lock(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := m.bootstrap(ctx, s); err != nil {
		return nil, err
	}

	applied, err := m.recorded(ctx, s)
	if err != nil {
		return nil, err
	}

	results := make([]RepairResult, 0, len(ops))

	for _, op := range ops {
		got, err := m.repairOne(ctx, s, op, applied)
		if err != nil {
			return nil, err
		}

		results = append(results, got...)
	}

	m.cfg.logger.Info("migrator: repaired", "operations", len(results))

	report.Repairs = results

	return report, nil
}

// repairOne performs one operation and reports what it did.
func (m *Migrator) repairOne(
	ctx context.Context, s Session, op RepairOp, applied map[int64]Record,
) ([]RepairResult, error) {
	switch op.kind {
	case repairChecksum:
		rec, ok := applied[op.version]
		if !ok {
			return nil, fmt.Errorf("%w: version %d is not recorded", ErrNothingToDo, op.version)
		}

		mig, ok := m.set.ByVersion(op.version)
		if !ok {
			return nil, fmt.Errorf("%w: version %d", ErrMissingMigration, op.version)
		}

		if rec.Checksum == mig.Checksum {
			return nil, nil
		}

		_, err := s.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET checksum = $2, down_checksum = $3,
			                checksum_previous = checksum, checksum_repaired_at = now()
			  WHERE version = $1`, m.qualified()),
			op.version, mig.Checksum, nullable(mig.DownChecksum))
		if err != nil {
			return nil, fmt.Errorf("%w: rehash %d: %w", ErrBookkeeping, op.version, redact(err))
		}

		return []RepairResult{{
			Version: op.version, Action: "rehash",
			Before: rec.Checksum, After: mig.Checksum,
		}}, nil

	case repairComplete:
		rec, ok := applied[op.version]
		if !ok {
			return nil, fmt.Errorf("%w: version %d is not recorded", ErrNothingToDo, op.version)
		}

		if !rec.Incomplete() {
			return nil, nil
		}

		if _, err := s.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET finished_at = now() WHERE version = $1 AND finished_at IS NULL`,
			m.qualified()), op.version); err != nil {
			return nil, fmt.Errorf("%w: complete %d: %w", ErrBookkeeping, op.version, redact(err))
		}

		return []RepairResult{{Version: op.version, Action: "complete"}}, nil

	case repairDiscard:
		if _, ok := applied[op.version]; !ok {
			return nil, nil
		}

		if _, err := s.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE version = $1`, m.qualified()), op.version); err != nil {
			return nil, fmt.Errorf("%w: discard %d: %w", ErrBookkeeping, op.version, redact(err))
		}

		return []RepairResult{{Version: op.version, Action: "discard"}}, nil

	case repairPrune:
		var orphans []int64

		for version := range applied {
			if _, ok := m.set.ByVersion(version); !ok {
				orphans = append(orphans, version)
			}
		}

		slices.Sort(orphans)

		out := make([]RepairResult, 0, len(orphans))

		for _, version := range orphans {
			if _, err := s.Exec(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE version = $1`, m.qualified()), version); err != nil {
				return nil, fmt.Errorf("%w: prune %d: %w", ErrBookkeeping, version, redact(err))
			}

			out = append(out, RepairResult{Version: version, Action: "prune"})
		}

		return out, nil

	default:
		return nil, fmt.Errorf("%w: unknown repair", ErrNothingToDo)
	}
}
