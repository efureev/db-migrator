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

// A bookkeepingColumn is one column of the table this package owns.
//
// The table cannot be recreated — it holds the history — so this list is the
// only way it ever gains a column, and it is the single source of truth for
// three things that used to be written out separately and could drift: the
// CREATE TABLE, the ALTER TABLE that upgrades an older installation, and the
// column list every SELECT and RETURNING uses.
type bookkeepingColumn struct {
	name string
	// create is the definition inside CREATE TABLE.
	create string
	// add is the definition for ALTER TABLE ... ADD COLUMN IF NOT EXISTS, or ""
	// for a column that cannot meaningfully be added to an existing table. A
	// table missing the primary key is not one of ours, so there is no case
	// where adding it is the right answer.
	add string
	// since names the release that introduced the column, so that the reason it
	// exists stays attached to it rather than living in a changelog entry.
	since string
	// backfill is run once, immediately after the column is added, to give the
	// rows that predate it a meaning. It is not run when the column was already
	// there, so it cannot touch rows whose NULL is the truth.
	//
	// Without this, adding a nullable column silently reinterprets history:
	// finished_at IS NULL means "started and never confirmed", so every row
	// written before that column existed would read as an interrupted migration
	// and block every later run.
	backfill string
}

// bookkeepingSchema is the table as this version expects it.
//
// Additive only. Removing a column would break the older binary still running
// in the pod next door during a rollout, so the rule is: the table grows and
// never shrinks, and a column that stops being useful simply stops being
// written.
var bookkeepingSchema = []bookkeepingColumn{
	{name: "version", create: "BIGINT PRIMARY KEY", since: "2.0"},
	{name: "name", create: "TEXT NOT NULL", add: "TEXT NOT NULL DEFAULT ''", since: "2.0"},
	{name: "checksum", create: "TEXT NOT NULL", add: "TEXT NOT NULL DEFAULT ''", since: "2.0"},
	{name: "down_checksum", create: "TEXT", add: "TEXT", since: "2.0"},
	{name: "applied_at", create: "TIMESTAMPTZ NOT NULL DEFAULT now()", add: "TIMESTAMPTZ NOT NULL DEFAULT now()", since: "2.0"},
	{
		name: "finished_at", create: "TIMESTAMPTZ", add: "TIMESTAMPTZ", since: "2.0",
		// A row in a journal that predates this column was applied: the schema
		// it came from had no way to express "incomplete".
		backfill: "SET finished_at = applied_at WHERE finished_at IS NULL",
	},
	{name: "execution_ms", create: "BIGINT", add: "BIGINT", since: "2.0"},
	{name: "applied_by", create: "TEXT NOT NULL DEFAULT ''", add: "TEXT NOT NULL DEFAULT ''", since: "2.0"},
	{name: "applied_role", create: "NAME NOT NULL DEFAULT CURRENT_USER", add: "NAME NOT NULL DEFAULT CURRENT_USER", since: "2.0"},
	{name: "transactional", create: "BOOLEAN NOT NULL DEFAULT TRUE", add: "BOOLEAN NOT NULL DEFAULT TRUE", since: "2.0"},
	{name: "rolled_back_at", create: "TIMESTAMPTZ", add: "TIMESTAMPTZ", since: "2.0"},
	{name: "checksum_repaired_at", create: "TIMESTAMPTZ", add: "TIMESTAMPTZ", since: "2.0"},
	{name: "checksum_previous", create: "TEXT", add: "TEXT", since: "2.0"},
	{name: "migrator", create: "TEXT NOT NULL DEFAULT ''", add: "TEXT NOT NULL DEFAULT ''", since: "2.0"},
	// A migration recorded by adopt was never watched running here. status has
	// to be able to say so: "applied" and "we were told it was applied" are
	// different claims, and only one of them was observed.
	{name: "adopted_at", create: "TIMESTAMPTZ", add: "TIMESTAMPTZ", since: "2.1"},
}

// ownershipMarker is the column whose presence says a table is ours.
//
// golang-migrate's journal is (version, dirty) and shares our default name, so
// "the table exists" is not enough to conclude it is ours — and quietly adding
// our columns to somebody else's journal would corrupt it.
const ownershipMarker = "checksum"

// bookkeepingDDL builds the CREATE TABLE.
//
// There are no secondary indexes and there will not be: the table holds tens of
// rows and is always read whole in one SELECT. An index on it is noise in \d
// and in every schema dump.
func bookkeepingDDL(qualified string) string {
	var b strings.Builder

	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(qualified)
	b.WriteString(" (\n")

	for i, c := range bookkeepingSchema {
		b.WriteString("\t")
		b.WriteString(c.name)
		b.WriteString(" ")
		b.WriteString(c.create)

		if i < len(bookkeepingSchema)-1 {
			b.WriteString(",")
		}

		b.WriteString("\n")
	}

	b.WriteString(")")

	return b.String()
}

// recordColumns is the column list every SELECT and RETURNING uses, in the
// order scanRecord reads them.
func recordColumns() string {
	names := make([]string, len(bookkeepingSchema))
	for i, c := range bookkeepingSchema {
		names[i] = c.name
	}

	return strings.Join(names, ", ")
}

// bootstrap creates or upgrades the schema and the bookkeeping table.
//
// It runs only while the advisory lock is held. Concurrent CREATE TABLE IF NOT
// EXISTS in PostgreSQL is not as idempotent as it reads: two sessions doing it
// at the same instant can fail with a duplicate key on pg_type_typname_nsp_index,
// which is the classic bootstrap race and is invisible until the day two
// replicas start together against an empty database.
//
// # Why this is not just CREATE TABLE IF NOT EXISTS
//
// That statement does nothing at all to a table that already exists, including
// one written by an older version of this tool. A release that added a column
// would then fail on every existing installation with "column does not exist",
// and the fix would be a hand-written ALTER TABLE in a changelog entry. So the
// table is upgraded here, additively, on every run.
func (m *Migrator) bootstrap(ctx context.Context, s Session) error {
	if err := m.preflight(ctx, s); err != nil {
		return err
	}

	if _, err := s.Exec(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{m.cfg.schema}.Sanitize()); err != nil {
		return fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}

	existing, err := m.bookkeepingColumnsPresent(ctx, s)
	if err != nil {
		return err
	}

	if len(existing) == 0 {
		return m.createBookkeeping(ctx, s)
	}

	if !existing[ownershipMarker] {
		// Somebody else's journal under our name. Adding our columns to it
		// would corrupt theirs, and dropping it would lose their history.
		return fmt.Errorf("%w: %s exists and has no %q column, so it is not this tool's journal "+
			"(golang-migrate's is version+dirty) — see \"migrator adopt\"",
			ErrForeignJournal, m.qualified(), ownershipMarker)
	}

	return m.upgradeBookkeeping(ctx, s, existing)
}

// preflight checks that the role can do what the run will need, before the run
// needs it.
//
// Discovering a missing privilege on the third migration is the worst moment to
// discover it: two are already applied, the transaction that would have made
// the third atomic never opened, and the operator now has a half-migrated
// database and a permissions ticket. One round trip up front turns that into a
// refusal that changed nothing.
//
// Only the privileges this tool itself requires are checked. What a migration's
// own SQL needs is unknowable from here — a migration may create an extension,
// or a role, or nothing at all.
func (m *Migrator) preflight(ctx context.Context, s Session) error {
	var (
		schemaExists bool
		canCreate    bool
		role         string
	)

	err := s.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1),
		       CASE
		         WHEN EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)
		           THEN has_schema_privilege(current_user, $1, 'CREATE')
		         ELSE has_database_privilege(current_user, current_database(), 'CREATE')
		       END,
		       current_user`, m.cfg.schema).Scan(&schemaExists, &canCreate, &role)
	if err != nil {
		return fmt.Errorf("%w: check privileges: %w", ErrInsufficientPrivilege, redact(err))
	}

	if canCreate {
		return nil
	}

	if schemaExists {
		return fmt.Errorf("%w: role %q cannot CREATE in schema %q, so the journal cannot be written",
			ErrInsufficientPrivilege, role, m.cfg.schema)
	}

	return fmt.Errorf("%w: role %q cannot create schema %q",
		ErrInsufficientPrivilege, role, m.cfg.schema)
}

// createBookkeeping writes the table from scratch.
func (m *Migrator) createBookkeeping(ctx context.Context, s Session) error {
	stmts := []string{
		bookkeepingDDL(m.qualified()),
		fmt.Sprintf(`COMMENT ON TABLE %s IS %s`, m.qualified(),
			quoteLiteral("Applied migrations, managed by github.com/efureev/db-migrator. Do not edit by hand.")),
	}

	for _, stmt := range stmts {
		if _, err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
		}
	}

	return nil
}

// upgradeBookkeeping adds whatever columns this version expects and the table
// does not have.
func (m *Migrator) upgradeBookkeeping(ctx context.Context, s Session, existing map[string]bool) error {
	for _, c := range bookkeepingSchema {
		if existing[c.name] || c.add == "" {
			continue
		}

		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
			m.qualified(), pgx.Identifier{c.name}.Sanitize(), c.add)

		if _, err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w: add column %s: %w", ErrBookkeeping, c.name, redact(err))
		}

		if c.backfill != "" {
			if _, err := s.Exec(ctx, "UPDATE "+m.qualified()+" "+c.backfill); err != nil {
				return fmt.Errorf("%w: backfill column %s: %w", ErrBookkeeping, c.name, redact(err))
			}
		}

		m.cfg.logger.Info("migrator: upgraded the journal",
			"column", c.name, "since", c.since, "backfilled", c.backfill != "")
	}

	return nil
}

// bookkeepingColumnsPresent reports the columns the table has, or an empty map
// when the table does not exist.
func (m *Migrator) bookkeepingColumnsPresent(ctx context.Context, s Session) (map[string]bool, error) {
	rows, err := s.Query(ctx, `
		SELECT a.attname::text
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2
		   AND c.relkind IN ('r','p') AND a.attnum > 0 AND NOT a.attisdropped`,
		m.cfg.schema, m.cfg.table)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}
	defer rows.Close()

	out := map[string]bool{}

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
		}

		out[name] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBookkeeping, redact(err))
	}

	return out, nil
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
	rows, err := s.Query(ctx, "SELECT "+recordColumns()+" FROM "+m.qualified())
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
			&repairedAt, &prevSum, &r.Migrator, &r.AdoptedAt,
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
		&repairedAt, &prevSum, &r.Migrator, &r.AdoptedAt,
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
			// A migration whose author declared it idempotent may be re-run
			// rather than refused: the decision is made in the file, by whoever
			// knows whether re-running is safe, and not by a flag reached for at
			// three in the morning. Without that declaration this is a hard
			// refusal, because a failed CREATE INDEX CONCURRENTLY leaves an
			// invalid index that a second CONCURRENTLY will not replace.
			if mig, ok := m.set.ByVersion(version); ok && mig.Directives.RetrySafe {
				m.cfg.logger.Warn("migrator: re-running an interrupted migration marked retry-safe",
					"version", version, "name", rec.Name)

				continue
			}

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
	if m.replacerOnce != nil {
		body = m.replacerOnce.Replace(body)
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
