package migrator

import (
	"errors"
	"fmt"
	"strings"
)

// Errors reported while reading and validating a source. Load joins every
// problem it finds rather than returning the first: fixing a directory one
// error per run turns a minute of work into a quarter of an hour.
var (
	// ErrNoSource reports a source holding no migrations at all.
	ErrNoSource = errors.New("migrator: no migrations found")
	// ErrBadFilename reports a name that is not <version>_<name>.(up|down).sql.
	ErrBadFilename = errors.New("migrator: filename does not match <version>_<name>.(up|down).sql")
	// ErrDuplicateVersion reports two files claiming one version.
	ErrDuplicateVersion = errors.New("migrator: two files claim one version")
	// ErrOrphanDownFile reports a down file with no matching up file.
	ErrOrphanDownFile = errors.New("migrator: down file has no matching up file")
	// ErrEmptyMigration reports an up file that holds no statements.
	ErrEmptyMigration = errors.New("migrator: up file is empty")
	// ErrUnknownDirective reports a "-- migrator:" line this version does not
	// understand. It is an error rather than a warning because a mistyped
	// no-transaction leaves CREATE INDEX CONCURRENTLY inside a transaction,
	// where it fails with SQLSTATE 25001 at best and holds a lock at worst.
	ErrUnknownDirective = errors.New("migrator: unknown migrator directive")
	// ErrBadDirective reports a directive whose argument does not parse.
	ErrBadDirective = errors.New("migrator: malformed migrator directive")
	// ErrMissingDownFile reports a rollback of a migration that ships no down
	// file. It surfaces while planning, never mid-run: rolling back three of
	// five versions and stopping at a missing file is the worst outcome
	// available.
	ErrMissingDownFile = errors.New("migrator: migration ships no down file")
	// ErrUnterminated reports SQL whose quoting or comment never closes.
	ErrUnterminated = errors.New("migrator: unterminated quote or comment")
)

// Errors reported by a run.
var (
	// ErrChecksumMismatch reports a migration whose file changed after it was
	// applied. It is not recoverable automatically: either the file was edited,
	// which must be undone, or the database was migrated by a different build.
	ErrChecksumMismatch = errors.New("migrator: migration file changed after it was applied")
	// ErrMissingMigration reports a recorded migration absent from the source.
	ErrMissingMigration = errors.New("migrator: applied migration is absent from the source")
	// ErrIncomplete reports a migration recorded as started and never
	// confirmed — a no-transaction run that did not come back.
	ErrIncomplete = errors.New("migrator: migration recorded as started was never confirmed")
	// ErrOutOfOrder reports a pending migration older than the current version,
	// which happens when two long-lived branches merge. Refused by default:
	// otherwise the order in which the branches merged decides the schema.
	ErrOutOfOrder = errors.New("migrator: pending migration is older than the current version")
	// ErrUnresolvedPlaceholder reports an @name@ token left after substitution.
	ErrUnresolvedPlaceholder = errors.New("migrator: placeholder left unresolved")
	// ErrBookkeeping reports a bookkeeping table that exists but is unusable.
	ErrBookkeeping = errors.New("migrator: bookkeeping table is not usable")
	// ErrInsufficientPrivilege reports a role that cannot do what the run needs.
	// It is checked before the run starts: finding out on the third migration
	// leaves two applied and a half-migrated database.
	ErrInsufficientPrivilege = errors.New("migrator: the role lacks a privilege the run needs")
	// ErrForeignJournal reports a table under this tool's name that belongs to
	// something else — golang-migrate's journal shares the default name. Adding
	// our columns to it would corrupt it and dropping it would lose its history,
	// so the run stops and points at adopt.
	ErrForeignJournal = errors.New("migrator: the journal table belongs to another tool")
	// ErrNothingToDo reports a run with no work in it. It is returned only
	// where a caller asked for a specific step that does not exist; Up reports
	// an empty Report and a nil error.
	ErrNothingToDo = errors.New("migrator: nothing to do")
	// ErrInvalidIdentifier reports a schema or table name that is not a valid
	// unquoted PostgreSQL identifier. They reach SQL by interpolation — a
	// placeholder cannot stand in for an identifier — so they are checked
	// rather than trusted.
	ErrInvalidIdentifier = errors.New("migrator: not a valid unquoted identifier")
)

// Errors reported by the guards on destructive operations.
var (
	// ErrDownNotAllowed reports Down or Redo without [WithAllowDown].
	ErrDownNotAllowed = errors.New("migrator: down migrations require WithAllowDown")
	// ErrProductionGuard reports a destructive operation against a database
	// declared to be production. No option overrides it.
	ErrProductionGuard = errors.New("migrator: refused in a production environment")
	// ErrWipeRefused reports Wipe without [WithAllowWipe].
	ErrWipeRefused = errors.New("migrator: wipe refused")
	// ErrNotConfirmed reports a confirmation that does not name this database.
	ErrNotConfirmed = errors.New("migrator: confirmation does not name this database")
)

// Errors reported by the lock.
var (
	// ErrLockTimeout reports another run holding the advisory lock.
	ErrLockTimeout = errors.New("migrator: another migration run holds the lock")
	// ErrSessionNotPinned reports a connection that does not stay on one
	// backend — a pooler in transaction mode. Session advisory locks and SET do
	// not survive it, so the runner refuses rather than pretending to be safe.
	ErrSessionNotPinned = errors.New("migrator: connection is not pinned to one backend")
)

// A SourceError reports one malformed or misplaced file, naming it. Load joins
// these, so a caller sees every bad file at once and errors.Is still matches
// the sentinel underneath.
type SourceError struct {
	// File is the name as it appears in the source.
	File string
	// Detail is what was wrong beyond the sentinel, or "".
	Detail string
	// Err is one of the source sentinels.
	Err error
}

func (e *SourceError) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	b.WriteString(": ")
	b.WriteString(e.Err.Error())

	if e.Detail != "" {
		b.WriteString(" (")
		b.WriteString(e.Detail)
		b.WriteString(")")
	}

	return b.String()
}

// Unwrap reports the sentinel, so that errors.Is matches through the join.
func (e *SourceError) Unwrap() error { return e.Err }

// A ChecksumError reports one migration whose file no longer hashes to what was
// recorded when it ran.
type ChecksumError struct {
	// Version and Name identify the migration.
	Version int64
	Name    string
	// File is the up file the checksum covers.
	File string
	// Recorded is the checksum stored when the migration was applied.
	Recorded string
	// Actual is the checksum of the file as it is now.
	Actual string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("%s: applied as %s, on disk %s",
		e.File, short(e.Recorded), short(e.Actual))
}

// Unwrap reports [ErrChecksumMismatch], so that a caller may match either the
// sentinel with errors.Is or the detail with errors.AsType.
func (e *ChecksumError) Unwrap() error { return ErrChecksumMismatch }

// A MigrationError reports a migration that failed, carrying enough to find the
// offending statement without re-reading the file.
type MigrationError struct {
	// Version and Name identify the migration.
	Version int64
	Name    string
	// Direction says which half ran.
	Direction Direction
	// File is the file that was sent.
	File string
	// Statement is the 1-based index of the statement that failed and Line the
	// file line it starts on. Both are 0 when PostgreSQL reported no position,
	// which happens for failures found while executing rather than parsing.
	Statement int
	Line      int
	// Err is the underlying failure; it wraps *pgconn.PgError when PostgreSQL
	// rejected the statement.
	Err error
}

func (e *MigrationError) Error() string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.File, e.Line)
	}

	return fmt.Sprintf("%s: %s", loc, e.Err)
}

// Unwrap reports the underlying failure.
func (e *MigrationError) Unwrap() error { return e.Err }

// short trims a hex checksum to the twelve characters that make it readable in
// a terminal while staying unambiguous in practice.
func short(sum string) string {
	const n = 12
	if len(sum) <= n {
		return sum
	}

	return sum[:n]
}

// Errors reported by a misused constructor. They are unexported because a
// caller cannot recover from them: passing a nil connection is a bug in the
// calling code, not a condition to branch on.
var (
	errNilConn = errors.New("FromConn was given a nil connection")
	errNilPool = errors.New("FromPool was given a nil pool")
)

// redact strips anything password-shaped from an error before it can be
// printed or logged.
//
// pgconn puts the whole connection configuration into the text of a connection
// error, password included, so an error printed verbatim leaks the credential
// this tool is otherwise careful with. The replacement is deliberately blunt:
// there is no shape of connection error worth reading closely enough to justify
// keeping the secret in it.
func redact(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()

	// Scanning forward rather than repeatedly searching from the start. The
	// replacement keeps the key and replaces only the value, so a search that
	// restarts finds the same key again and loops forever — which is exactly
	// what the first version of this function did.
	for _, key := range []string{"password=", "password:"} {
		msg = redactAfter(msg, key)
	}

	// A URL of the form scheme://user:secret@host loses its middle field.
	if i := strings.Index(msg, "://"); i >= 0 {
		if at := strings.IndexByte(msg[i:], '@'); at > 0 {
			seg := msg[i+3 : i+at]
			if colon := strings.IndexByte(seg, ':'); colon >= 0 {
				msg = msg[:i+3+colon+1] + redactedValue + msg[i+at:]
			}
		}
	}

	if msg == err.Error() {
		return err
	}

	return errors.New(msg)
}

// redactedValue is what stands in for a secret, chosen to be obviously not one.
const redactedValue = "xxxxx"

// redactAfter replaces the value following every occurrence of key.
func redactAfter(msg, key string) string {
	var (
		b    strings.Builder
		rest = msg
	)

	for {
		i := strings.Index(rest, key)
		if i < 0 {
			b.WriteString(rest)

			break
		}

		b.WriteString(rest[:i+len(key)])

		// "password: secret" puts a space between the key and the value, and
		// that space is not part of the secret.
		j := i + len(key)
		for j < len(rest) && rest[j] == ' ' {
			b.WriteByte(' ')
			j++
		}

		end := j
		for end < len(rest) && !isValueDelimiter(rest[end]) {
			end++
		}

		if end > j {
			b.WriteString(redactedValue)
		}

		rest = rest[end:]
	}

	return b.String()
}

// isValueDelimiter reports the bytes that can end a credential in the shapes
// pgconn and libpq print.
func isValueDelimiter(c byte) bool {
	switch c {
	case ' ', '@', '"', '\n', '\r', ',', '}', ')', ';', '\'':
		return true
	default:
		return false
	}
}
