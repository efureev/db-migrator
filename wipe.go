package migrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// Kind is "table", "foreign table", "view", "materialized view",
	// "sequence", "routine" or "type".
	Kind string
	// Reason says why it was kept; it is empty for an object that was dropped.
	Reason string

	// identity is what DROP is given. For most kinds it is the quoted qualified
	// name; for a routine it is the signature, because a schema holding f(int)
	// and f(text) has two rows both named "f" and DROP ROUTINE by bare name
	// fails with 42725 — aborting the whole wipe transaction.
	identity string
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
	// Schema is the schema it emptied. Singular: a Migrator is built over one
	// schema, and a field that is always a one-element slice invites code that
	// pretends otherwise.
	Schema string
	// Dropped and Kept are the objects it removed and the objects it left.
	Dropped []Object
	Kept    []Object
	// Dependents are objects in *other* schemas that depend on this one, and
	// that DROP ... CASCADE would therefore take with it. They are the reason a
	// wipe can refuse: a view two schemas away vanishing because somebody reset
	// their development schema is not something to discover later.
	Dependents []Object
	// DryRun reports a wipe that decided what to drop and dropped nothing.
	DryRun bool
}

// String reports a one-line summary.
func (r *WipeReport) String() string {
	verb := "dropped"
	if r.DryRun {
		verb = "would be dropped"
	}

	return fmt.Sprintf("%d objects %s, %d kept in %s", len(r.Dropped), verb, len(r.Kept), r.Database)
}

// Text writes the report as a person reads it.
func (r *WipeReport) Text(w io.Writer) error {
	var b strings.Builder

	verb := "dropped"
	if r.DryRun {
		verb = "would drop"
	}

	for _, o := range r.Dropped {
		fmt.Fprintf(&b, "  %-10s %s\n", verb, o)
	}

	for _, o := range r.Kept {
		fmt.Fprintf(&b, "  %-10s %s\n", "kept", o)
	}

	for _, o := range r.Dependents {
		fmt.Fprintf(&b, "  %-10s %s\n", "dependent", o)
	}

	fmt.Fprintf(&b, "\n  %s\n", r)

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the report in the shape a CI step parses.
func (r *WipeReport) JSON(w io.Writer) error {
	type object struct {
		Schema string `json:"schema"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Reason string `json:"reason,omitempty"`
	}

	out := struct {
		Format   int      `json:"format"`
		Database string   `json:"database"`
		Schema   string   `json:"schema"`
		DryRun   bool     `json:"dry_run"`
		Dropped  []object `json:"dropped"`
		Kept     []object `json:"kept"`
	}{
		Format: JSONFormat, Database: r.Database, Schema: r.Schema, DryRun: r.DryRun,
		Dropped: make([]object, 0, len(r.Dropped)),
		Kept:    make([]object, 0, len(r.Kept)),
	}

	for _, o := range r.Dropped {
		out.Dropped = append(out.Dropped, object{Schema: o.Schema, Name: o.Name, Kind: o.Kind})
	}

	for _, o := range r.Kept {
		out.Kept = append(out.Kept, object{Schema: o.Schema, Name: o.Name, Kind: o.Kind, Reason: o.Reason})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// dependenciesOutside reports objects in other schemas that depend on this one.
//
// DROP ... CASCADE is silent about what it takes. A view in another schema
// selecting from a table here disappears with it, and nothing in the output of
// a successful wipe would ever mention it.
func (m *Migrator) dependenciesOutside(ctx context.Context, s Session) ([]Object, error) {
	const q = `
SELECT DISTINCT dn.nspname::text AS schema,
       dc.relname::text          AS name,
       CASE dc.relkind WHEN 'm' THEN 'materialized view' WHEN 'v' THEN 'view'
                       WHEN 'S' THEN 'sequence' ELSE 'table' END AS kind
  FROM pg_depend d
  JOIN pg_rewrite r  ON r.oid = d.objid
  JOIN pg_class dc   ON dc.oid = r.ev_class
  JOIN pg_namespace dn ON dn.oid = dc.relnamespace
  JOIN pg_class rc   ON rc.oid = d.refobjid
  JOIN pg_namespace rn ON rn.oid = rc.relnamespace
 WHERE d.classid = 'pg_rewrite'::regclass
   AND d.refclassid = 'pg_class'::regclass
   AND rn.nspname = $1
   AND dn.nspname <> $1
 ORDER BY schema, name`

	rows, err := s.Query(ctx, q, m.cfg.schema)
	if err != nil {
		return nil, fmt.Errorf("migrator: look for dependants outside the schema: %w", redact(err))
	}
	defer rows.Close()

	var out []Object

	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.Schema, &o.Name, &o.Kind); err != nil {
			return nil, fmt.Errorf("migrator: look for dependants outside the schema: %w", redact(err))
		}

		o.Reason = "depends on " + m.cfg.schema

		out = append(out, o)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrator: look for dependants outside the schema: %w", redact(err))
	}

	return out, nil
}

// joinObjects renders a short list of objects for an error message.
func joinObjects(objects []Object) string {
	const limit = 5

	names := make([]string, 0, min(len(objects), limit))
	for _, o := range objects[:min(len(objects), limit)] {
		names = append(names, o.Kind+" "+o.Schema+"."+o.Name)
	}

	s := strings.Join(names, ", ")
	if len(objects) > limit {
		s += fmt.Sprintf(" and %d more", len(objects)-limit)
	}

	return s
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

	report := &WipeReport{Database: database, Schema: m.cfg.schema}

	objects, err := m.wipeTargets(ctx, s)
	if err != nil {
		return nil, err
	}

	outside, err := m.dependenciesOutside(ctx, s)
	if err != nil {
		return nil, err
	}

	if len(outside) > 0 {
		report.Dependents = outside

		if !m.cfg.forceWipe {
			return report, fmt.Errorf("%w: %d object(s) outside %q depend on this schema and "+
				"CASCADE would take them too: %s — pass WithForceWipe to accept that",
				ErrWipeRefused, len(outside), m.cfg.schema, joinObjects(outside))
		}

		m.cfg.logger.Warn("migrator: dropping objects outside the schema along with it",
			"count", len(outside))
	}

	if m.cfg.dryRun {
		report.DryRun = true

		for _, o := range objects {
			if o.Reason != "" {
				report.Kept = append(report.Kept, o)

				continue
			}

			report.Dropped = append(report.Dropped, o)
		}

		return report, nil
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

		if _, err := tx.Exec(ctx, o.dropStatement()); err != nil {
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
	// identity is what DROP is given, and it is not always the name. A routine
	// must be dropped by signature: a schema holding f(int) and f(text) has two
	// pg_proc rows both called "f", and DROP ROUTINE by bare name fails with
	// 42725 — which, inside the single transaction a wipe runs in, discards
	// every drop already issued and leaves the operator with nothing done and
	// no way forward.
	const q = `
WITH ext AS (
  SELECT objid FROM pg_depend WHERE deptype = 'e'
)
SELECT kind, name, identity, owned FROM (
  -- ordinary and partitioned tables
  SELECT 1 AS ord, 'table' AS kind,
         c.relname::text AS name,
         format('%I.%I', n.nspname, c.relname) AS identity,
         pg_get_userbyid(c.relowner) = CURRENT_USER AS owned
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind IN ('r','p')
     AND c.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  -- foreign tables need DROP FOREIGN TABLE; DROP TABLE is rejected outright
  SELECT 2, 'foreign table', c.relname::text,
         format('%I.%I', n.nspname, c.relname),
         pg_get_userbyid(c.relowner) = CURRENT_USER
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind = 'f'
     AND c.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  SELECT 3, CASE c.relkind WHEN 'm' THEN 'materialized view' ELSE 'view' END,
         c.relname::text, format('%I.%I', n.nspname, c.relname),
         pg_get_userbyid(c.relowner) = CURRENT_USER
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind IN ('m','v')
     AND c.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  -- sequences not owned by a column: those went with their table
  SELECT 4, 'sequence', c.relname::text, format('%I.%I', n.nspname, c.relname),
         pg_get_userbyid(c.relowner) = CURRENT_USER
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = $1 AND c.relkind = 'S'
     AND c.oid NOT IN (SELECT objid FROM ext)
     AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.objid = c.oid AND d.deptype = 'a')

  UNION ALL

  -- by signature, not by name: see the note above.
  SELECT 5, 'routine', p.proname::text, p.oid::regprocedure::text,
         pg_get_userbyid(p.proowner) = CURRENT_USER
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
   WHERE n.nspname = $1 AND p.oid NOT IN (SELECT objid FROM ext)

  UNION ALL

  -- composite, enum, domain and range types, excluding the row types that
  -- belong to tables: those go with the table.
  SELECT 6, 'type', t.typname::text, format('%I.%I', n.nspname, t.typname),
         pg_get_userbyid(t.typowner) = CURRENT_USER
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
			kind, name, identity string
			owned                bool
		)

		if err := rows.Scan(&kind, &name, &identity, &owned); err != nil {
			return nil, fmt.Errorf("migrator: enumerate objects to wipe: %w", redact(err))
		}

		o := Object{Schema: m.cfg.schema, Name: name, Kind: kind, identity: identity}
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

// dropStatement reports the statement that removes this object.
func (o Object) dropStatement() string {
	target := o.identity
	if target == "" {
		target = pgx.Identifier{o.Schema, o.Name}.Sanitize()
	}

	return "DROP " + dropKeyword(o.Kind) + " IF EXISTS " + target + " CASCADE"
}

// dropKeyword reports the DROP statement for an object kind.
func dropKeyword(kind string) string {
	switch kind {
	case "materialized view":
		return "MATERIALIZED VIEW"
	case "foreign table":
		return "FOREIGN TABLE"
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
