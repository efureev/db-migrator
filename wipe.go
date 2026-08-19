package migrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A Confirmation names the database the caller believes it is about to wipe.
type Confirmation struct {
	database string
	given    bool
}

// Confirm names the database.
//
// Wipe compares it against current_database() and refuses on a mismatch. This
// is the one guard that survives a command copy-pasted with the wrong
// PGDATABASE in the environment, which is how the accident actually happens.
func Confirm(database string) Confirmation {
	return Confirmation{database: database, given: true}
}

// An Object is one database object a wipe dropped or declined to drop.
type Object struct {
	// Schema and Name identify the object.
	Schema string
	Name   string
	// Kind is "table", "view", "materialized view", "sequence", "routine" or
	// "type".
	Kind string
	// Reason says why it was kept; it is empty for an object that was dropped.
	Reason string
}

// String reports the object as it appears in output.
func (o Object) String() string {
	s := o.Kind + " " + o.Schema + "." + o.Name
	if o.Reason != "" {
		s += " (kept: " + o.Reason + ")"
	}

	return s
}

// A WipeReport is what a wipe did.
type WipeReport struct {
	// Database is the database it ran against.
	Database string
	// Schemas are the schemas it emptied.
	Schemas []string
	// Dropped and Kept are the objects it removed and the objects it left.
	Dropped []Object
	Kept    []Object
	// DryRun reports a wipe that decided what to drop and dropped nothing.
	DryRun bool
}

// String reports a one-line summary.
func (r *WipeReport) String() string {
	return fmt.Sprintf("%d objects dropped, %d kept in %s", len(r.Dropped), len(r.Kept), r.Database)
}

// systemSchemas are never touched, whatever the configuration says.
var systemSchemas = map[string]bool{
	"pg_catalog": true, "information_schema": true, "pg_toast": true,
}

// Wipe drops the contents of the managed schema.
//
// # What it does not do
//
// It does not DROP SCHEMA ... CASCADE, for two reasons that are easy to
// discover the expensive way. That statement takes the extensions installed
// into the schema with it — pg_trgm, uuid-ossp, postgis — so a developer who
// wiped a database to re-migrate it gets a failure on the first
// gen_random_uuid(). And in PostgreSQL 15 and later a hand-recreated public
// schema has different default privileges from the one initdb made: PUBLIC no
// longer has CREATE. A wipe that quietly changes the security model of the
// database is not a wipe anybody asked for.
//
// So the objects are enumerated and dropped individually, leaving the schema
// itself, its extensions, and anything owned by another role.
//
// # What holds it together
//
// All of it runs in one transaction. Every DROP involved is transactional in
// PostgreSQL, which people are often surprised by — so a wipe either happened
// or did not, and a half-erased database is not a state this can produce.
func (m *Migrator) Wipe(ctx context.Context, c Confirmation) (*WipeReport, error) {
	if err := m.guardWipe(); err != nil {
		return nil, err
	}

	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { s.Release(context.WithoutCancel(ctx)) }()

	var database string
	if err := s.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return nil, fmt.Errorf("migrator: read the current database: %w", redact(err))
	}

	if err := m.confirmWipe(c, database); err != nil {
		return nil, err
	}

	release, err := m.lock(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	report := &WipeReport{Database: database, Schemas: []string{m.cfg.schema}}

	objects, err := m.wipeTargets(ctx, s)
	if err != nil {
		return nil, err
	}

	tx, err := s.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrator: begin: %w", redact(err))
	}

	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	for _, o := range objects {
		if o.Reason != "" {
			report.Kept = append(report.Kept, o)

			continue
		}

		stmt := fmt.Sprintf("DROP %s IF EXISTS %s CASCADE",
			dropKeyword(o.Kind), pgx.Identifier{o.Schema, o.Name}.Sanitize())

		if _, err := tx.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("migrator: drop %s: %w", o, redact(err))
		}

		report.Dropped = append(report.Dropped, o)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("migrator: commit the wipe: %w", redact(err))
	}

	m.cfg.logger.Info("migrator: wiped", "database", database,
		"dropped", len(report.Dropped), "kept", len(report.Kept))

	return report, nil
}

// guardWipe enforces the gates that do not depend on the database.
func (m *Migrator) guardWipe() error {
	// Production first and unconditionally: it is the refusal no option
	// overrides, and saying so before the missing flag stops anyone from
	// thinking the flag would help.
	if m.cfg.environment == EnvProduction {
		return fmt.Errorf("%w: wipe is not available when the environment is production",
			ErrProductionGuard)
	}

	if !m.cfg.allowWipe {
		return ErrWipeRefused
	}

	if systemSchemas[m.cfg.schema] || strings.HasPrefix(m.cfg.schema, "pg_") {
		return fmt.Errorf("%w: %q is a system schema", ErrWipeRefused, m.cfg.schema)
	}

	return nil
}

// confirmWipe checks the confirmation against the database actually connected
// to, and against the name pattern that guards production-looking databases.
func (m *Migrator) confirmWipe(c Confirmation, database string) error {
	// A confirmation that was given must match, in every environment. The
	// earlier version skipped this check under EnvDevelopment, which turned the
	// guard off exactly where wipe is actually run: a mistyped database name
	// then wiped the database the operator was connected to instead of
	// refusing. Whether a confirmation is *required* may depend on the
	// environment; whether a given one is *checked* may not.
	if c.given && c.database != database {
		return fmt.Errorf("%w: confirmation names %q, connected to %q",
			ErrNotConfirmed, c.database, database)
	}

	if !c.given && m.cfg.environment != EnvDevelopment {
		return fmt.Errorf("%w: name the database to confirm (it is %q)", ErrNotConfirmed, database)
	}

	if m.cfg.wipeProtectPattern != "" {
		re, err := regexp.Compile(m.cfg.wipeProtectPattern)
		if err != nil {
			return fmt.Errorf("migrator: wipe protection pattern: %w", err)
		}

		if re.MatchString(database) {
			return fmt.Errorf("%w: the database name %q matches %s",
				ErrWipeRefused, database, m.cfg.wipeProtectPattern)
		}
	}

	return nil
}

// wipeTargets lists what a wipe would drop, in dependency-friendly order.
//
// Objects belonging to an extension are excluded by pg_depend.deptype = 'e':
// dropping them would take the extension with them, which is exactly the
// failure DROP SCHEMA CASCADE produces and this exists to avoid.
func (m *Migrator) wipeTargets(ctx context.Context, s Session) ([]Object, error) {
	const q = `
WITH ext AS (
  SELECT objid FROM pg_depend WHERE deptype = 'e'
)
SELECT kind, name, owned FROM (
  -- tables, partitioned tables, foreign tables
  SELECT 1 AS ord,
         CASE c.relkind WHEN 'm' THEN 'materialized view' WHEN 'v' THEN 'view'
                        WHEN 'S' THEN 'sequence' ELSE 'table' END AS kind,
         c.relname::text AS name,
         pg_get_userbyid(c.relowner) = CURRENT_USER AS owned
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind IN ('r','p','f')
     AND c.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  SELECT 2, CASE c.relkind WHEN 'm' THEN 'materialized view' ELSE 'view' END,
         c.relname::text, pg_get_userbyid(c.relowner) = CURRENT_USER
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind IN ('m','v')
     AND c.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  -- sequences not owned by a column: those went with their table
  SELECT 3, 'sequence', c.relname::text, pg_get_userbyid(c.relowner) = CURRENT_USER
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind = 'S'
     AND c.oid NOT IN (SELECT objid FROM ext)
     AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.objid = c.oid AND d.deptype = 'a')

  UNION ALL

  SELECT 4, 'routine', p.proname::text, pg_get_userbyid(p.proowner) = CURRENT_USER
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
   WHERE n.nspname = $1 AND p.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  -- composite, enum, domain and range types, excluding the row types that
  -- belong to tables: those go with the table.
  SELECT 5, 'type', t.typname::text, pg_get_userbyid(t.typowner) = CURRENT_USER
    FROM pg_type t
    JOIN pg_namespace n ON n.oid = t.typnamespace
   WHERE n.nspname = $1 AND t.typtype IN ('c','e','d','r')
     AND t.oid NOT IN (SELECT objid FROM ext)
     AND NOT EXISTS (SELECT 1 FROM pg_class c
                      WHERE c.reltype = t.oid AND c.relkind IN ('r','p','f','m','v','S'))
) o
ORDER BY ord, name`

	rows, err := s.Query(ctx, q, m.cfg.schema)
	if err != nil {
		return nil, fmt.Errorf("migrator: enumerate objects to wipe: %w", redact(err))
	}
	defer rows.Close()

	var out []Object

	for rows.Next() {
		var (
			kind, name string
			owned      bool
		)

		if err := rows.Scan(&kind, &name, &owned); err != nil {
			return nil, fmt.Errorf("migrator: enumerate objects to wipe: %w", redact(err))
		}

		o := Object{Schema: m.cfg.schema, Name: name, Kind: kind}
		if !owned {
			// Reported rather than attempted: a wipe that dies halfway through
			// on somebody else's table is worse than one that says which
			// tables it will not touch.
			o.Reason = "owned by another role"
		}

		out = append(out, o)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrator: enumerate objects to wipe: %w", redact(err))
	}

	return out, nil
}

// dropKeyword reports the DROP statement for an object kind.
func dropKeyword(kind string) string {
	switch kind {
	case "materialized view":
		return "MATERIALIZED VIEW"
	case "view":
		return "VIEW"
	case "sequence":
		return "SEQUENCE"
	case "routine":
		return "ROUTINE"
	case "type":
		return "TYPE"
	default:
		return "TABLE"
	}
}
