package migrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/efureev/db-migrator/v2/internal/sqlsplit"
)

// sqlstateLockNotAvailable is what PostgreSQL reports when lock_timeout fired.
const sqlstateLockNotAvailable = "55P03"

// applySettings issues the run-scoped GUCs.
//
// SET LOCAL inside a transaction, plain SET outside one, because there is no
// transaction to scope to on the no-transaction path. LOCAL matters on a pooled
// connection: without it the hour-long statement_timeout a migration needed
// travels back into the pool attached to the connection.
//
// The values are always sent, including zeros. The point is not the value, it
// is overriding whatever the session inherited: a pool configured with
// statement_timeout=30s otherwise kills exactly the migrations this tool exists
// to run.
func (m *Migrator) applySettings(ctx context.Context, c Conn, d Directives, local bool) error {
	scope := "SET "
	if local {
		scope = "SET LOCAL "
	}

	statement := m.cfg.statementTimeout
	if d.StatementTimeout != 0 {
		statement = d.StatementTimeout
	}

	lock := m.cfg.lockTimeout
	if d.LockTimeout != 0 {
		lock = d.LockTimeout
	}

	stmts := []string{
		scope + "statement_timeout = " + milliseconds(statement),
		scope + "lock_timeout = " + milliseconds(lock),
	}

	if local {
		// Only meaningful inside a transaction, and a cheap guard against a
		// migration that opens one and then waits on something.
		stmts = append(stmts, scope+"idle_in_transaction_session_timeout = '30000ms'")
	}

	for _, stmt := range stmts {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migrator: apply run settings: %w", redact(err))
		}
	}

	return nil
}

// runSettings are the GUCs a run overrides.
var runSettings = []string{"statement_timeout", "lock_timeout"}

// captureSettings reads the values the session had before the run changed them.
//
// RESET would be shorter and wrong: it restores the server or role default, not
// what this session actually had. A pool configured with statement_timeout=30s
// would get its connection back with no timeout at all, and every later
// application query on it would run unbounded.
func (m *Migrator) captureSettings(ctx context.Context, c Conn) map[string]string {
	out := make(map[string]string, len(runSettings))

	for _, name := range runSettings {
		var value string
		if err := c.QueryRow(ctx, `SELECT current_setting($1, true)`, name).Scan(&value); err != nil {
			m.cfg.logger.Debug("migrator: could not read a run setting", "setting", name, "error", redact(err))

			continue
		}

		out[name] = value
	}

	return out
}

// restoreSettings puts back what captureSettings read.
//
// Only reached on the no-transaction path; the transactional path uses SET
// LOCAL and is undone by COMMIT or ROLLBACK.
func (m *Migrator) restoreSettings(c Conn, saved map[string]string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	for name, value := range saved {
		stmt := "RESET " + name
		if value != "" {
			stmt = fmt.Sprintf("SET %s = %s", name, quoteLiteral(value))
		}

		if _, err := c.Exec(ctx, stmt); err != nil {
			m.cfg.logger.Debug("migrator: could not restore a run setting",
				"setting", name, "error", redact(err))
		}
	}
}

// milliseconds renders a duration as the PostgreSQL literal for it.
//
// Always milliseconds, never "30min" or "1h": the interval syntax PostgreSQL
// accepts here depends on IntervalStyle, and a unit-free integer is read as
// milliseconds regardless of it.
func milliseconds(d time.Duration) string {
	return fmt.Sprintf("'%dms'", d.Milliseconds())
}

// applyTransactional runs one migration and records it in a single transaction.
//
// A partly applied migration that is nonetheless recorded is the failure mode a
// runner exists to prevent, so both happen together or neither does.
func (m *Migrator) applyTransactional(
	ctx context.Context, s Session, mig Migration, d Direction,
) (Record, error) {
	body, directives, file := halfOf(mig, d)

	sql, err := m.substitute(body)
	if err != nil {
		return Record{}, &MigrationError{
			Version: mig.Version, Name: mig.Name, Direction: d, File: file, Err: err,
		}
	}

	attempts := max(m.cfg.lockRetries, 0) + 1

	var last error

	for attempt := range attempts {
		if attempt > 0 {
			m.cfg.logger.Info("migrator: retrying after lock_timeout",
				"version", mig.Version, "attempt", attempt+1, "of", attempts)

			select {
			case <-ctx.Done():
				return Record{}, ctx.Err()
			case <-time.After(backoffFor(m.cfg.lockRetryBackoff, attempt)):
			}
		}

		rec, err := m.attemptTransactional(ctx, s, mig, d, sql, file, directives)
		if err == nil {
			return rec, nil
		}

		last = err

		// Retrying is offered only here, on the path where the failed attempt
		// rolled back whole. A no-transaction migration is never retried.
		if !isLockTimeout(err) {
			return Record{}, err
		}
	}

	return Record{}, last
}

// attemptTransactional is one try of [Migrator.applyTransactional].
func (m *Migrator) attemptTransactional(
	ctx context.Context, s Session, mig Migration, d Direction,
	sql, file string, directives Directives,
) (Record, error) {
	started := time.Now()

	tx, err := s.Begin(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("migrator: begin: %w", redact(err))
	}

	defer func() {
		// A detached context: a caller cancelled mid-migration still needs the
		// transaction rolled back, and ROLLBACK on a cancelled context is a
		// no-op that leaves it open.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_ = tx.Rollback(rollbackCtx)
	}()

	if err := m.applySettings(ctx, tx, directives, true); err != nil {
		return Record{}, err
	}

	if _, err := tx.Exec(ctx, sql); err != nil {
		return Record{}, m.migrationError(mig, d, file, sql, err)
	}

	rec, err := m.record(ctx, tx, mig, d, time.Since(started), true)
	if err != nil {
		return Record{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, fmt.Errorf("migrator: commit %s: %w", mig, redact(err))
	}

	return rec, nil
}

// applyNoTransaction runs a migration that cannot be wrapped in a transaction.
//
// Three phases: claim, execute, confirm. The claim commits a row with
// finished_at NULL, so from that moment the database durably says "version N
// started and did not come back". That row is the only lasting evidence a crash
// between the DDL and the bookkeeping ever happened, and it is why finished_at
// is nullable at all.
//
// Statements are sent one at a time. This is the entire reason
// internal/sqlsplit exists: pgx sends an argument-less query over the simple
// protocol, where PostgreSQL wraps a multi-statement string in an implicit
// transaction — so "just don't call Begin" still leaves CREATE INDEX
// CONCURRENTLY inside a transaction, failing with SQLSTATE 25001.
func (m *Migrator) applyNoTransaction(
	ctx context.Context, s Session, mig Migration, d Direction,
) (Record, error) {
	body, directives, file := halfOf(mig, d)

	sql, err := m.substitute(body)
	if err != nil {
		return Record{}, &MigrationError{
			Version: mig.Version, Name: mig.Name, Direction: d, File: file, Err: err,
		}
	}

	stmts, err := sqlsplit.Split(sql)
	if err != nil {
		return Record{}, &MigrationError{
			Version: mig.Version, Name: mig.Name, Direction: d, File: file, Err: err,
		}
	}

	started := time.Now()

	if err := m.claim(ctx, s, mig, d); err != nil {
		return Record{}, err
	}

	saved := m.captureSettings(ctx, s)

	if err := m.applySettings(ctx, s, directives, false); err != nil {
		return Record{}, err
	}

	defer m.restoreSettings(s, saved)

	for i, stmt := range stmts {
		if _, err := s.Exec(ctx, stmt.SQL); err != nil {
			me := m.migrationError(mig, d, file, sql, err)
			me.Statement = i + 1
			me.Line = stmt.Line

			return Record{}, me
		}
	}

	return m.confirm(ctx, s, mig, d, time.Since(started))
}

// claim commits the "started" row of a no-transaction migration.
func (m *Migrator) claim(ctx context.Context, s Session, mig Migration, d Direction) error {
	if d == DirectionDown {
		// Rolling back a no-transaction migration does not create a row; it
		// clears the one that is there, and does so at the end. There is
		// nothing to claim.
		return nil
	}

	_, err := s.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (version, name, checksum, down_checksum, applied_at, finished_at,
		                applied_by, transactional, migrator)
		VALUES ($1, $2, $3, $4, now(), NULL, $5, FALSE, $6)
		ON CONFLICT (version) DO UPDATE
		   SET name = EXCLUDED.name, checksum = EXCLUDED.checksum,
		       down_checksum = EXCLUDED.down_checksum,
		       applied_at = now(), finished_at = NULL, rolled_back_at = NULL,
		       applied_by = EXCLUDED.applied_by, transactional = FALSE
		 WHERE %s.rolled_back_at IS NOT NULL OR %s.finished_at IS NULL`,
		m.qualified(), m.qualified(), m.qualified()),
		mig.Version, mig.Name, mig.Checksum, nullable(mig.DownChecksum),
		m.cfg.appliedBy, m.cfg.migratorTag)
	if err != nil {
		return fmt.Errorf("%w: claim %s: %w", ErrBookkeeping, mig, redact(err))
	}

	return nil
}

// confirm closes out a no-transaction migration.
//
// Going up that means marking the claimed row finished. Going down it means
// marking the row rolled back, exactly as the transactional path does — a
// rollback that only set finished_at would leave InForce true, so status would
// report a migration as applied while the object it created was gone, and the
// next Down would happily roll it back again.
func (m *Migrator) confirm(
	ctx context.Context, s Session, mig Migration, d Direction, took time.Duration,
) (Record, error) {
	set := `finished_at = now(), execution_ms = $2`
	if d == DirectionDown {
		set = `rolled_back_at = now(), execution_ms = $2`
	}

	rec, err := scanRecord(s.QueryRow(ctx, fmt.Sprintf(
		`UPDATE %s SET %s WHERE version = $1 RETURNING %s`,
		m.qualified(), set, recordColumns()),
		mig.Version, took.Milliseconds()))
	if err != nil {
		return Record{}, fmt.Errorf("%w: confirm %s: %w", ErrBookkeeping, mig, redact(err))
	}

	return rec, nil
}

// record writes the bookkeeping row for a migration inside its transaction.
func (m *Migrator) record(
	ctx context.Context, tx pgx.Tx, mig Migration, d Direction, took time.Duration, transactional bool,
) (Record, error) {
	if d == DirectionDown {
		// A rollback updates the row rather than deleting it: the history of
		// what ran is worth more than a tidy table, and status can then say
		// "applied, then rolled back" instead of "never applied".
		rec, err := scanRecord(tx.QueryRow(ctx, fmt.Sprintf(
			`UPDATE %s SET rolled_back_at = now(), execution_ms = $2
			  WHERE version = $1 RETURNING %s`, m.qualified(), recordColumns()),
			mig.Version, took.Milliseconds()))
		if err != nil {
			return Record{}, fmt.Errorf("%w: record rollback of %s: %w", ErrBookkeeping, mig, redact(err))
		}

		return rec, nil
	}

	rec, err := scanRecord(tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s (version, name, checksum, down_checksum, applied_at, finished_at,
		                execution_ms, applied_by, transactional, migrator)
		VALUES ($1, $2, $3, $4, now(), now(), $5, $6, $7, $8)
		ON CONFLICT (version) DO UPDATE
		   SET name = EXCLUDED.name, checksum = EXCLUDED.checksum,
		       down_checksum = EXCLUDED.down_checksum,
		       applied_at = now(), finished_at = now(),
		       execution_ms = EXCLUDED.execution_ms, applied_by = EXCLUDED.applied_by,
		       transactional = EXCLUDED.transactional, rolled_back_at = NULL
		 WHERE %s.rolled_back_at IS NOT NULL OR %s.finished_at IS NULL
		RETURNING %s`,
		m.qualified(), m.qualified(), m.qualified(), recordColumns()),
		mig.Version, mig.Name, mig.Checksum, nullable(mig.DownChecksum),
		took.Milliseconds(), m.cfg.appliedBy, transactional, m.cfg.migratorTag))
	if err != nil {
		return Record{}, fmt.Errorf("%w: record %s: %w", ErrBookkeeping, mig, redact(err))
	}

	return rec, nil
}

// migrationError turns a PostgreSQL failure into one that names the place.
//
// PgError.Position is a 1-based character offset into the string that was sent.
// Mapping it back to a line is best-effort and documented as such: for a
// failure found while executing rather than parsing, PostgreSQL reports no
// position at all, and the line is then left at zero rather than guessed.
func (m *Migrator) migrationError(mig Migration, d Direction, file, sent string, err error) *MigrationError {
	me := &MigrationError{
		Version: mig.Version, Name: mig.Name, Direction: d, File: file, Err: redact(err),
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Position > 0 {
		me.Line = sqlsplit.LineAt(sent, int(pg.Position)-1)
	}

	return me
}

// halfOf reports the body, directives and file name for one direction.
func halfOf(m Migration, d Direction) (body string, directives Directives, file string) {
	if d == DirectionDown {
		return m.DownSQL(), m.DownDirectives, m.DownFile
	}

	return m.UpSQL(), m.Directives, m.UpFile
}

// noTransaction reports whether the given half must run outside a transaction.
func noTransaction(m Migration, d Direction) bool {
	_, directives, _ := halfOf(m, d)

	return directives.NoTransaction
}

// isLockTimeout reports a failure caused by lock_timeout firing.
func isLockTimeout(err error) bool {
	var pg *pgconn.PgError

	return errors.As(err, &pg) && pg.Code == sqlstateLockNotAvailable
}

// backoffFor reports how long to wait before retry number attempt, with jitter
// so that concurrent deployers do not retry in unison.
func backoffFor(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Second
	}

	d := base << min(attempt, 4)

	return d/2 + time.Duration(randN(int64(d)))
}

// nullable reports s as a value pgx writes as NULL when empty.
func nullable(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
