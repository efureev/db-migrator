package cli

import (
	"context"
	"errors"

	migrator "github.com/efureev/db-migrator/v2"
)

// Exit codes.
//
// More than the three a small tool needs, for one reason: in CI the right
// reaction to "another deploy holds the lock" is to retry in a minute, and the
// right reaction to "the SQL is wrong" is to wake somebody up. Those cannot be
// told apart from stderr, so they are told apart here.
const (
	// ExitOK is success, including "there was nothing to do".
	ExitOK = 0
	// ExitFailure is a run that failed: bad SQL, a broken connection, a full disk.
	ExitFailure = 1
	// ExitUsage is a command that could not be understood.
	ExitUsage = 2
	// ExitPending is "there are unapplied migrations", for status --check.
	ExitPending = 3
	// ExitDrift is files and journal disagreeing. A person has to look.
	ExitDrift = 4
	// ExitLocked is another migrator holding the lock. Retrying is correct.
	ExitLocked = 5
	// ExitRefused is a destructive operation turned down by a guard. Retrying
	// is pointless; the pipeline is what needs changing, not the database.
	ExitRefused = 6
	// ExitInterrupted is 128 + SIGINT, as a shell reports it. SIGTERM is
	// reported the same way rather than as 143: a signal handler that cancels
	// the context and unwinds cannot tell the caller which signal arrived by
	// the time it exits, and inventing a second code would be a distinction
	// this program does not actually make.
	ExitInterrupted = 130
)

// ExitCode reports the code an error should exit with.
//
// The mapping is by sentinel rather than by message, which is what lets a
// caller's pipeline branch on the outcome without parsing English.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK

	case errors.Is(err, context.Canceled):
		return ExitInterrupted

	// "There is work to do" is neither success nor failure, and a CI step that
	// gates a deploy on it needs to tell it from both.
	case errors.As(err, new(*pendingError)):
		return ExitPending

	// Guards first: a refusal is not a failure, and telling them apart is the
	// difference between "fix the pipeline" and "fix the database".
	case errors.Is(err, migrator.ErrDownNotAllowed),
		errors.Is(err, migrator.ErrProductionGuard),
		errors.Is(err, migrator.ErrWipeRefused),
		errors.Is(err, migrator.ErrLockTooHeavy),
		errors.Is(err, migrator.ErrNotConfirmed):
		return ExitRefused

	case errors.Is(err, migrator.ErrLockTimeout),
		errors.Is(err, migrator.ErrSessionNotPinned):
		return ExitLocked

	// A missing privilege and a foreign journal are both "somebody has to
	// change something outside this database" — retrying changes nothing, and
	// neither is a drift between files and journal.
	case errors.Is(err, migrator.ErrInsufficientPrivilege),
		errors.Is(err, migrator.ErrForeignJournal),
		errors.Is(err, migrator.ErrAlreadyAdopted),
		errors.Is(err, migrator.ErrDirtyJournal):
		return ExitRefused

	case errors.Is(err, migrator.ErrChecksumMismatch),
		errors.Is(err, migrator.ErrMissingMigration),
		errors.Is(err, migrator.ErrIncomplete),
		errors.Is(err, migrator.ErrOutOfOrder),
		errors.Is(err, migrator.ErrUnresolvedPlaceholder):
		return ExitDrift

	// A malformed source is a usage error: the command cannot run, and no
	// amount of retrying will change that.
	case errors.Is(err, migrator.ErrNoSource),
		errors.Is(err, migrator.ErrBadFilename),
		errors.Is(err, migrator.ErrDuplicateVersion),
		errors.Is(err, migrator.ErrOrphanDownFile),
		errors.Is(err, migrator.ErrEmptyMigration),
		errors.Is(err, migrator.ErrUnknownDirective),
		errors.Is(err, migrator.ErrBadDirective),
		errors.Is(err, migrator.ErrUnterminated),
		errors.Is(err, migrator.ErrMissingDownFile),
		errors.Is(err, migrator.ErrInvalidIdentifier),
		errors.Is(err, migrator.ErrBadName),
		errors.Is(err, migrator.ErrNoBaseline),
		errors.Is(err, errUsage):
		return ExitUsage

	default:
		return ExitFailure
	}
}

// errUsage marks an error as a mistake in how the command was invoked.
var errUsage = errors.New("usage")

// usageErrorf builds a usage error.
func usageErrorf(format string, args ...any) error {
	return joinUsage(errorf(format, args...))
}

func joinUsage(err error) error { return errors.Join(err, errUsage) }
