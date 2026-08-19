package migrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// identifierPattern is what a schema or table name must look like.
//
// Names reach SQL by interpolation, because a bind parameter cannot stand in
// for an identifier. They are therefore checked here and quoted at the point of
// use — belt and braces, since neither alone has a good failure mode.
var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// validateIdentifier reports whether name is safe to interpolate.
func validateIdentifier(kind, name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("%w: %s %q", ErrInvalidIdentifier, kind, name)
	}

	return nil
}

// qualified reports the quoted schema-qualified table name.
func (m *Migrator) qualified() string {
	return pgx.Identifier{m.cfg.schema, m.cfg.table}.Sanitize()
}

// bookkeepingDDL is the schema of the table this package owns.
//
// There are no secondary indexes and there will not be: the table holds tens of
// rows and is always read whole in one SELECT. An index on it is noise in \d
// and in every schema dump.
const bookkeepingDDL = `CREATE TABLE IF NOT EXISTS %s (
	version              BIGINT       PRIMARY KEY,
	name                 TEXT         NOT NULL,
	checksum             TEXT         NOT NULL,
	down_checksum        TEXT,
	applied_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
	finished_at          TIMESTAMPTZ,
	execution_ms         BIGINT,
	applied_by           TEXT         NOT NULL DEFAULT '',
	applied_role         NAME         NOT NULL DEFAULT CURRENT_USER,
	transactional        BOOLEAN      NOT NULL DEFAULT TRUE,
	rolled_back_at       TIMESTAMPTZ,
	checksum_repaired_at TIMESTAMPTZ,
	checksum_previous    TEXT,
	migrator             TEXT         NOT NULL DEFAULT ''
)`

const recordColumns = `version, name, checksum, down_checksum, applied_at, finished_at,
	execution_ms, applied_by, applied_role, transactional, rolled_back_at,
	checksum_repaired_at, checksum_previous, migrator`

// bootstrap creates the schema and the bookkeeping table.
//
// It runs only while the advisory lock is held. Concurrent CREATE TABLE IF NOT
// EXISTS in PostgreSQL is not as idempotent as it reads: two sessions doing it
// at the same instant can fail with a duplicate key on pg_type_typname_nsp_index,
// which is the classic bootstrap race and is invisible until the day two
// replicas start together against an empty database.
func (m *Migrator) bootstrap(ctx context.Context, s Session) error {
	stmts := []string{
		"CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{m.cfg.schema}.Sanitize(),
		fmt.Sprintf(bookkeepingDDL, m.qualified()),
		fmt.Sprintf(
			`COMMENT ON TABLE %s IS %s`,
			m.qualified(),
			quoteLiteral("Applied migrations, managed by github.com/efureev/db-migrator. Do not edit by hand."),
		),
	}

	for _, stmt := range stmts {
		if _, err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
		}
	}

	return nil
}

// initialised reports whether the bookkeeping table exists.
//
// Status and Version use this to answer without writing: a status command that
// creates a table is one nobody dares run against production, and a read-only
// role must be able to answer "what is applied here".
func (m *Migrator) initialised(ctx context.Context, s Session) (bool, error) {
	var exists bool

	err := s.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r','p')
		 )`, m.cfg.schema, m.cfg.table).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}

	return exists, nil
}

// recorded reads every row of the bookkeeping table, keyed by version.
func (m *Migrator) recorded(ctx context.Context, s Session) (map[int64]Record, error) {
	rows, err := s.Query(ctx, "SELECT "+recordColumns+" FROM "+m.qualified())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}
	defer rows.Close()

	out := map[int64]Record{}

	for rows.Next() {
		var (
			r          Record
			downSum    *string
			execMS     *int64
			prevSum    *string
			repairedAt *time.Time
		)

		if err := rows.Scan(
			&r.Version, &r.Name, &r.Checksum, &downSum, &r.AppliedAt, &r.FinishedAt,
			&execMS, &r.AppliedBy, &r.AppliedRole, &r.Transactional, &r.RolledBackAt,
			&repairedAt, &prevSum, &r.Migrator,
		); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
		}

		if downSum != nil {
			r.DownChecksum = *downSum
		}

		if execMS != nil {
			r.ExecutionTime = time.Duration(*execMS) * time.Millisecond
		}

		if prevSum != nil {
			r.ChecksumPrevious = *prevSum
		}

		r.ChecksumRepairedAt = repairedAt
		out[r.Version] = r
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}

	return out, nil
}

// scanRecord reads one row returned by an INSERT ... RETURNING.
func scanRecord(row pgx.Row) (Record, error) {
	var (
		r          Record
		downSum    *string
		execMS     *int64
		prevSum    *string
		repairedAt *time.Time
	)

	err := row.Scan(
		&r.Version, &r.Name, &r.Checksum, &downSum, &r.AppliedAt, &r.FinishedAt,
		&execMS, &r.AppliedBy, &r.AppliedRole, &r.Transactional, &r.RolledBackAt,
		&repairedAt, &prevSum, &r.Migrator,
	)
	if err != nil {
		return r, err
	}

	if downSum != nil {
		r.DownChecksum = *downSum
	}

	if execMS != nil {
		r.ExecutionTime = time.Duration(*execMS) * time.Millisecond
	}

	if prevSum != nil {
		r.ChecksumPrevious = *prevSum
	}

	r.ChecksumRepairedAt = repairedAt

	return r, nil
}

// checkDrift compares the source with the bookkeeping table and reports every
// disagreement at once.
//
// A mismatch is refused before anything runs, and on every recorded version
// rather than only the ones about to execute: a drifted file does not say
// something about that migration, it says the files on disk are not the ones
// this database was built from, after which no later assumption holds either.
func (m *Migrator) checkDrift(applied map[int64]Record) error {
	var problems []error

	for version, rec := range applied {
		if rec.Incomplete() {
			problems = append(problems, fmt.Errorf("%w: %d_%s started at %s by %s",
				ErrIncomplete, version, rec.Name, rec.AppliedAt.Format(time.RFC3339), rec.AppliedBy))

			continue
		}

		if rec.RolledBackAt != nil {
			continue
		}

		mig, ok := m.set.ByVersion(version)
		if !ok {
			problems = append(problems, fmt.Errorf("%w: %d_%s", ErrMissingMigration, version, rec.Name))

			continue
		}

		if mig.Checksum != rec.Checksum {
			problems = append(problems, &ChecksumError{
				Version:  version,
				Name:     mig.Name,
				File:     mig.UpFile,
				Recorded: rec.Checksum,
				Actual:   mig.Checksum,
			})
		}
	}

	if len(problems) > 0 {
		return errors.Join(problems...)
	}

	return nil
}

// substitute fills the configured placeholders into a migration body and
// reports any @name@ token left behind.
//
// An unresolved token is an error rather than something passed through: a
// mistyped @tabel@ would otherwise reach PostgreSQL as a syntax error partway
// through a DDL script, which is the worst moment to discover a typo.
func (m *Migrator) substitute(body string) (string, error) {
	if len(m.replacer) > 0 {
		body = strings.NewReplacer(m.replacer...).Replace(body)
	}

	if leftover := placeholderPattern.FindString(body); leftover != "" {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedPlaceholder, leftover)
	}

	return body, nil
}

// placeholderPattern matches an unresolved @name@ token.
var placeholderPattern = regexp.MustCompile(`@[a-zA-Z_][a-zA-Z0-9_]*@`)

// quoteLiteral renders a string as a PostgreSQL literal. It is used only for
// text this package itself writes — the table comment — because a bind
// parameter is not accepted in a COMMENT statement.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
