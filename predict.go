package migrator

import (
	"context"

	"github.com/efureev/db-migrator/v2/internal/pglock"
)

// A LockLevel is a PostgreSQL table-level lock mode.
//
// The values are ordered by strength, so [WithMaxLockLevel] is one comparison.
// That ordering is the one PostgreSQL documents.
type LockLevel = pglock.Level

// The lock modes, weakest first.
//
// [LockUnknown] is not a mode: it reports a statement whose lock could not be
// determined, which is a different thing from a statement that takes no lock.
const (
	LockUnknown          = pglock.LevelUnknown
	LockNone             = pglock.LevelNone
	AccessShare          = pglock.AccessShare
	RowShare             = pglock.RowShare
	RowExclusive         = pglock.RowExclusive
	ShareUpdateExclusive = pglock.ShareUpdateExclusive
	Share                = pglock.Share
	ShareRowExclusive    = pglock.ShareRowExclusive
	Exclusive            = pglock.Exclusive
	AccessExclusive      = pglock.AccessExclusive
)

// A LockPrediction is what one statement is expected to do to one relation:
// which lock it takes, whether it rewrites the table, whether it scans it, and
// how large the table is.
//
// It is a heuristic over the text of the statement, not a planner. See
// [Migrator.Plan] for what it does not know.
type LockPrediction = pglock.Prediction

// ParseLockLevel reads a level from the spelling a person types: the mode name
// in any case, with spaces, hyphens or underscores between the words.
func ParseLockLevel(s string) (LockLevel, bool) { return pglock.ParseLevel(s) }

// LockLevels reports every real lock mode, weakest first, for help text.
func LockLevels() []LockLevel { return pglock.Levels() }

// predict analyses the statements a step would send and fills in what only the
// server knows.
//
// Enrichment failing is not an error the caller hears about: a row count is a
// nicety, and refusing to plan because pg_class could not be read would make
// the useful part hostage to the decorative one. The prediction still stands,
// with Rows at -1.
func (m *Migrator) predict(ctx context.Context, s Session, steps []Step) {
	version, err := pglock.ServerVersion(ctx, s)
	if err != nil {
		m.cfg.logger.Debug("migrator: could not read the server version", "error", err)
	}

	opts := pglock.Options{ServerVersion: version, DefaultSchema: ""}

	for i := range steps {
		steps[i].Predictions = pglock.Analyze(analysisStatements(steps[i]), opts)

		if err := pglock.Enrich(ctx, s, steps[i].Predictions); err != nil {
			m.cfg.logger.Debug("migrator: could not size the tables a migration touches", "error", err)
		}
	}
}

// heaviest reports the strongest predicted lock over a set of predictions, and
// the prediction that earned it.
func heaviest(preds []LockPrediction) (LockPrediction, bool) {
	var (
		out   LockPrediction
		found bool
	)

	for _, p := range preds {
		if !found || p.Level > out.Level {
			out, found = p, true
		}
	}

	return out, found
}

// analysisStatements reports the statements of a step as a person counts them
// in the file.
//
// A transactional step sends its body to the server whole, so Step.SQL holds
// one element — but predicting from that would classify only the first command
// of the migration and call the rest nothing. What a lock report is about is
// each statement, however many round trips they take.
func analysisStatements(step Step) []string {
	if !step.Transactional || len(step.SQL) != 1 {
		return step.SQL
	}

	stmts, err := splitForPlan(step.SQL[0])
	if err != nil {
		// The body did not lex. Load would have caught that, and if something
		// slipped through, a prediction is not the place to report it.
		return step.SQL
	}

	return stmts
}

// guardLockLevel refuses a run whose plan would take a heavier lock than the
// policy allows, before anything has run.
//
// The check happens under the advisory lock and against the same statements
// that are about to be sent, so it cannot be defeated by the plan changing
// between the look and the leap. A migration may raise its own limit with
// "-- migrator:lock-acknowledged <level>"; nothing outside the file can.
func (m *Migrator) guardLockLevel(ctx context.Context, s Session, migs []Migration, d Direction) error {
	if m.cfg.maxLockLevel == LockUnknown || len(migs) == 0 {
		return nil
	}

	steps, err := m.stepsFor(migs, d)
	if err != nil {
		return err
	}

	m.predict(ctx, s, steps)

	for _, step := range steps {
		// The directives of the half that is about to run: a down file that
		// drops what the up file created takes its own locks, and it is the
		// down file that has to acknowledge them.
		_, directives, file := halfOf(step.Migration, d)

		limit := m.cfg.maxLockLevel

		// The acknowledgement raises the limit for this migration only, and
		// only upwards: a file cannot make the policy stricter by accident, and
		// a file that acknowledges less than it takes is still refused.
		if ack := directives.LockAcknowledged; ack > limit {
			limit = ack
		}

		for _, p := range step.Predictions {
			if p.Level <= limit {
				continue
			}

			return &LockLevelError{
				Version: step.Migration.Version, Name: step.Migration.Name,
				File: file, Limit: limit, Prediction: p,
			}
		}
	}

	return nil
}
