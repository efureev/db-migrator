// Package pglock predicts what a migration will lock before it runs.
//
// The prediction is a heuristic over the text of each statement, not a
// planner. It knows the rules PostgreSQL documents for DDL — which lock a form
// of ALTER TABLE takes, whether it rewrites the table, whether it scans it —
// and it applies them to what it can read out of the SQL. It does not know
// about triggers, rules, inheritance or the queue in front of the lock, and a
// statement it cannot parse is reported as unknown rather than as safe.
//
// The point is the difference between "this migration takes ACCESS EXCLUSIVE on
// a table with forty million rows and rewrites it" and finding that out during
// the deploy. Everything here exists to make that sentence printable before
// anything runs.
package pglock

import "strings"

// A Level is a PostgreSQL table-level lock mode.
//
// The values are ordered by strength, so they can be compared: a gate that
// refuses anything above [Share] is a single comparison. That ordering is the
// one PostgreSQL documents, and it is total — every pair of modes either
// conflicts or does not, consistently with the order.
type Level uint8

// The lock modes, weakest first. [LevelUnknown] is not a mode: it reports a
// statement whose lock could not be determined, which is different from a
// statement that takes no lock at all.
const (
	LevelUnknown Level = iota
	LevelNone
	AccessShare
	RowShare
	RowExclusive
	ShareUpdateExclusive
	Share
	ShareRowExclusive
	Exclusive
	AccessExclusive
)

// levelNames maps each level to the spelling PostgreSQL itself uses in
// pg_locks.mode, minus the "Lock" suffix. Keeping the server's spelling means
// an operator can paste the name straight into a query.
var levelNames = map[Level]string{
	LevelUnknown:         "unknown",
	LevelNone:            "none",
	AccessShare:          "ACCESS SHARE",
	RowShare:             "ROW SHARE",
	RowExclusive:         "ROW EXCLUSIVE",
	ShareUpdateExclusive: "SHARE UPDATE EXCLUSIVE",
	Share:                "SHARE",
	ShareRowExclusive:    "SHARE ROW EXCLUSIVE",
	Exclusive:            "EXCLUSIVE",
	AccessExclusive:      "ACCESS EXCLUSIVE",
}

// String reports the level as PostgreSQL spells it.
func (l Level) String() string {
	if s, ok := levelNames[l]; ok {
		return s
	}

	return "unknown"
}

// MarshalText implements [encoding.TextMarshaler], so that --json output and a
// terminal use one spelling.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// PgMode reports the level as pg_locks.mode spells it, which is the same name
// with "Lock" appended and the spaces removed.
//
// It exists so that a prediction can be compared with what the server actually
// took, which is the only check that keeps the rule table honest.
func (l Level) PgMode() string {
	switch l {
	case AccessShare:
		return "AccessShareLock"
	case RowShare:
		return "RowShareLock"
	case RowExclusive:
		return "RowExclusiveLock"
	case ShareUpdateExclusive:
		return "ShareUpdateExclusiveLock"
	case Share:
		return "ShareLock"
	case ShareRowExclusive:
		return "ShareRowExclusiveLock"
	case Exclusive:
		return "ExclusiveLock"
	case AccessExclusive:
		return "AccessExclusiveLock"
	case LevelUnknown, LevelNone:
		return ""
	default:
		return ""
	}
}

// ParseLevel reads a level from the spelling a person would type: the mode
// name, in any case, with spaces or hyphens between the words.
//
// It accepts hyphens because that is what a command line and a migration
// directive want — "--max-lock-level access-exclusive" needs no quoting, and
// "-- migrator:lock-acknowledged access exclusive" would be ambiguous about
// where the argument ends.
func ParseLevel(s string) (Level, bool) {
	norm := strings.ToUpper(strings.TrimSpace(s))
	norm = strings.ReplaceAll(norm, "-", " ")
	norm = strings.ReplaceAll(norm, "_", " ")
	norm = strings.Join(strings.Fields(norm), " ")

	norm = strings.TrimSuffix(norm, " LOCK")

	for level, name := range levelNames {
		if norm == name {
			return level, true
		}
	}

	return LevelUnknown, false
}

// Levels reports every real lock mode, weakest first, for help text.
func Levels() []Level {
	return []Level{
		AccessShare, RowShare, RowExclusive, ShareUpdateExclusive,
		Share, ShareRowExclusive, Exclusive, AccessExclusive,
	}
}

// BlocksWrites reports whether the level keeps ordinary INSERT, UPDATE and
// DELETE out.
//
// This is the question people actually ask, and it is not the same as "is the
// lock strong": SHARE blocks writes while allowing reads, and
// SHARE UPDATE EXCLUSIVE — which sorts lower — blocks neither.
func (l Level) BlocksWrites() bool { return l >= Share }

// BlocksReads reports whether the level keeps ordinary SELECT out. Only
// ACCESS EXCLUSIVE does.
func (l Level) BlocksReads() bool { return l >= AccessExclusive }
