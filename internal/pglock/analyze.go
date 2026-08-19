package pglock

import "strings"

// A Prediction is what one statement is expected to do to one relation.
//
// A statement can produce more than one: ALTER TABLE ... ADD FOREIGN KEY locks
// both tables, and an ALTER TABLE with several actions is reported as the
// strongest of them per relation.
type Prediction struct {
	// Statement is the 1-based index of the statement within the migration.
	Statement int
	// Relation is the table, schema-qualified when the statement qualified it.
	// It is empty when the statement does not name one, or names one this
	// predictor could not find.
	Relation string
	// Level is the lock the statement is expected to take on Relation.
	Level Level
	// Rewrites reports that the table is rewritten rather than merely locked:
	// every row is copied, the table doubles on disk while it happens, and the
	// lock is held for all of it.
	Rewrites bool
	// Scans reports that the whole table is read under the lock — cheaper than
	// a rewrite and still proportional to the table.
	Scans bool
	// Rows is pg_class.reltuples for Relation, and -1 when that is not known.
	//
	// It is the planner's estimate, maintained by ANALYZE and autovacuum, not a
	// count. Not known covers two cases that mean the same thing to a reader:
	// nothing was looked up, and the table has never been analysed. Neither is
	// the same as an empty table, which is why this is not zero.
	Rows int64
	// Reason is what in the statement decided this, in a form meant to be
	// printed next to it.
	Reason string
}

// Heavy reports a prediction worth putting in front of a person: it blocks
// writes, rewrites the table, or scans it.
func (p Prediction) Heavy() bool {
	return p.Rewrites || p.Scans || p.Level.BlocksWrites()
}

// Options tunes the rules that depend on something outside the statement.
type Options struct {
	// ServerVersion is server_version_num — 170004 for 17.4. Zero means it is
	// not known, which happens offline, and then a version-dependent rule gives
	// the answer that was true before the version that improved it. Guessing
	// the improvement is available would turn a rewrite of a large table into a
	// line that said it was cheap.
	ServerVersion int
	// DefaultSchema is what an unqualified name resolves to when reporting a
	// relation. Empty leaves names as written.
	DefaultSchema string
}

// known returns the server version and whether it is known at all.
func (o Options) known() (int, bool) { return o.ServerVersion, o.ServerVersion > 0 }

// Analyze reports what each statement would lock, in statement order.
//
// The statements are expected to come from internal/sqlsplit, which is what
// gives Statement its meaning. A statement whose form is not in the rule table
// yields one prediction with [LevelUnknown]: silence would read as "nothing
// happens here".
func Analyze(statements []string, o Options) []Prediction {
	out := make([]Prediction, 0, len(statements))

	for i, sql := range statements {
		out = append(out, analyzeOne(sql, i+1, o)...)
	}

	return out
}

// analyzeOne reports the predictions for a single statement.
func analyzeOne(sql string, index int, o Options) []Prediction {
	c := &cursor{toks: tokenize(sql)}

	acc := &collector{statement: index, opts: o}

	switch {
	case c.at("ALTER", "TABLE"):
		c.skip(2)
		alterTable(c, acc, o)

	case c.at("ALTER", "MATERIALIZED", "VIEW"):
		c.skip(3)
		acc.add(c.relation(o), AccessExclusive, false, false,
			"ALTER MATERIALIZED VIEW takes the strongest lock on the view")

	case c.at("CREATE", "INDEX"), c.at("CREATE", "UNIQUE", "INDEX"):
		createIndex(c, acc, o)

	case c.at("DROP", "INDEX"):
		c.skip(2)
		dropIndex(c, acc, o)

	case c.at("REINDEX"):
		c.skip(1)
		reindex(c, acc, o)

	case c.at("DROP", "TABLE"):
		c.skip(2)
		c.optional("IF", "EXISTS")
		acc.add(c.relation(o), AccessExclusive, false, false,
			"DROP TABLE waits for every reader and writer, then takes the table away")

	case c.at("TRUNCATE"):
		c.skip(1)
		c.optional("TABLE")
		acc.add(c.relation(o), AccessExclusive, false, false,
			"TRUNCATE blocks reads as well as writes for its whole duration")

	case c.at("CLUSTER"):
		c.skip(1)
		acc.add(c.relation(o), AccessExclusive, true, true,
			"CLUSTER rewrites the table in index order and blocks everything meanwhile")

	case c.at("VACUUM"):
		c.skip(1)
		vacuum(c, acc, o)

	case c.at("ANALYZE"):
		c.skip(1)
		acc.add(c.relation(o), ShareUpdateExclusive, false, true,
			"ANALYZE samples the table; it does not block reads or writes")

	case c.at("REFRESH", "MATERIALIZED", "VIEW"):
		c.skip(3)
		refreshMatView(c, acc, o)

	case c.at("LOCK"):
		c.skip(1)
		c.optional("TABLE")
		lockTable(c, acc, o)

	case c.at("INSERT", "INTO"):
		c.skip(2)
		acc.add(c.relation(o), RowExclusive, false, false, "INSERT takes a row-level write lock")

	case c.at("UPDATE"):
		c.skip(1)
		acc.add(c.relation(o), RowExclusive, false, false, "UPDATE takes a row-level write lock")

	case c.at("DELETE", "FROM"):
		c.skip(2)
		acc.add(c.relation(o), RowExclusive, false, false, "DELETE takes a row-level write lock")

	case c.at("SELECT"), c.at("WITH"), c.at("TABLE"):
		readStatement(c, acc, o)

	case c.atAny(harmless...):
		acc.add("", LevelNone, false, false, "no existing table is locked")

	default:
		acc.add("", LevelUnknown, false, false,
			"this statement is not in the rule table, so what it locks is unknown")
	}

	return acc.result()
}

// harmless are the leading keywords of statements that create something new or
// touch only the catalogue, and so cannot block traffic on an existing table.
//
// CREATE TABLE is here rather than absent because "no lock" and "not analysed"
// have to look different in the output.
var harmless = [][]string{
	{"CREATE", "TABLE"}, {"CREATE", "SCHEMA"}, {"CREATE", "SEQUENCE"},
	{"CREATE", "TYPE"}, {"CREATE", "FUNCTION"}, {"CREATE", "OR"},
	{"CREATE", "EXTENSION"}, {"CREATE", "VIEW"}, {"CREATE", "TRIGGER"},
	{"CREATE", "POLICY"}, {"CREATE", "DOMAIN"}, {"CREATE", "ROLE"},
	{"COMMENT"}, {"GRANT"}, {"REVOKE"}, {"SET"}, {"RESET"},
	{"DO"}, {"DROP", "FUNCTION"}, {"DROP", "TYPE"}, {"DROP", "SEQUENCE"},
	{"DROP", "VIEW"}, {"DROP", "SCHEMA"}, {"DROP", "TRIGGER"},
	{"ALTER", "SEQUENCE"}, {"ALTER", "TYPE"}, {"ALTER", "SCHEMA"},
	{"ALTER", "FUNCTION"}, {"ALTER", "DOMAIN"},
}

// createIndex handles CREATE [UNIQUE] INDEX [CONCURRENTLY] [name] ON table.
func createIndex(c *cursor, acc *collector, o Options) {
	c.skip(1) // CREATE
	c.optional("UNIQUE")
	c.skip(1) // INDEX

	concurrent := c.optional("CONCURRENTLY")

	c.optional("IF", "NOT", "EXISTS")

	// The index name is optional. Whatever is there, ON is what precedes the
	// table, so skipping to it is both simpler and more robust than deciding
	// whether a name was given.
	if !c.skipToWord("ON") {
		acc.add("", LevelUnknown, false, false, "CREATE INDEX without an ON clause")

		return
	}

	rel := c.relation(o)

	if concurrent {
		acc.add(rel, ShareUpdateExclusive, false, true,
			"CONCURRENTLY lets reads and writes continue, at the cost of two passes over the table "+
				"and a failure mode that leaves an invalid index behind")

		return
	}

	acc.add(rel, Share, false, true,
		"CREATE INDEX without CONCURRENTLY blocks every INSERT, UPDATE and DELETE "+
			"until the index is built")
}

// dropIndex handles DROP INDEX [CONCURRENTLY] name.
func dropIndex(c *cursor, acc *collector, o Options) {
	concurrent := c.optional("CONCURRENTLY")

	c.optional("IF", "EXISTS")

	rel := c.relation(o)

	if concurrent {
		acc.add(rel, ShareUpdateExclusive, false, false,
			"DROP INDEX CONCURRENTLY does not block traffic on the table")

		return
	}

	acc.add(rel, AccessExclusive, false, false,
		"DROP INDEX takes ACCESS EXCLUSIVE on the table as well as the index")
}

// reindex handles REINDEX [ (…) ] [CONCURRENTLY] {INDEX|TABLE|SCHEMA} name.
func reindex(c *cursor, acc *collector, o Options) {
	c.skipParens()

	concurrent := c.optional("CONCURRENTLY")

	for _, kw := range []string{"INDEX", "TABLE", "SCHEMA", "DATABASE", "SYSTEM"} {
		if c.optional(kw) {
			break
		}
	}

	// After the object keyword CONCURRENTLY may come second: the server accepts
	// both orders.
	if !concurrent {
		concurrent = c.optional("CONCURRENTLY")
	}

	rel := c.relation(o)

	if concurrent {
		acc.add(rel, ShareUpdateExclusive, false, true,
			"REINDEX CONCURRENTLY rebuilds alongside the live index")

		return
	}

	acc.add(rel, AccessExclusive, false, true,
		"REINDEX without CONCURRENTLY blocks reads and writes for the whole rebuild")
}

// vacuum handles VACUUM [FULL] [options] [table].
func vacuum(c *cursor, acc *collector, o Options) {
	full := false

	// Both spellings exist: VACUUM FULL t and VACUUM (FULL) t.
	if c.optional("FULL") {
		full = true
	} else if c.peekPunct("(") {
		full = c.parenContains("FULL")
		c.skipParens()
	}

	c.optional("FREEZE")
	c.optional("VERBOSE")
	c.optional("ANALYZE")
	c.optional("TABLE")

	rel := c.relation(o)

	if full {
		acc.add(rel, AccessExclusive, true, true,
			"VACUUM FULL rewrites the table into a new file and blocks everything until it is done")

		return
	}

	acc.add(rel, ShareUpdateExclusive, false, true,
		"plain VACUUM runs alongside reads and writes")
}

// refreshMatView handles REFRESH MATERIALIZED VIEW [CONCURRENTLY] name.
func refreshMatView(c *cursor, acc *collector, o Options) {
	concurrent := c.optional("CONCURRENTLY")

	rel := c.relation(o)

	if concurrent {
		acc.add(rel, Exclusive, false, true,
			"REFRESH CONCURRENTLY keeps the view readable, and still blocks writes to it")

		return
	}

	acc.add(rel, AccessExclusive, true, true,
		"REFRESH without CONCURRENTLY replaces the contents and blocks reads of the view meanwhile")
}

// lockTable handles LOCK [TABLE] name IN mode MODE, where the statement says
// outright what it wants.
func lockTable(c *cursor, acc *collector, o Options) {
	rel := c.relation(o)

	level := AccessExclusive // the default when no mode is given
	reason := "LOCK without a mode means ACCESS EXCLUSIVE"

	if c.optional("IN") {
		var words []string

		for !c.eof() && !c.at("MODE") {
			t := c.next()
			if t.kind == tokWord {
				words = append(words, t.upper)
			}
		}

		if parsed, ok := ParseLevel(strings.Join(words, " ")); ok {
			level = parsed
			reason = "LOCK asks for this mode outright"
		}
	}

	acc.add(rel, level, false, false, reason)
}

// alterTable handles the form that matters most, and the only one with more
// than one action per statement.
func alterTable(c *cursor, acc *collector, o Options) {
	c.optional("IF", "EXISTS")
	c.optional("ONLY")

	rel := c.relation(o)

	c.optionalPunct("*")

	for _, action := range c.actions() {
		alterTableAction(action, rel, acc, o)
	}
}

// alterTableAction applies the rule table to one comma-separated action.
func alterTableAction(a *cursor, rel string, acc *collector, o Options) {
	switch {
	case a.at("ADD"):
		a.skip(1)
		alterTableAdd(a, rel, acc, o)

	case a.at("DROP", "CONSTRAINT"):
		acc.add(rel, AccessExclusive, false, false,
			"DROP CONSTRAINT only edits the catalogue, and still needs the strongest lock to do it")

	case a.at("DROP"):
		acc.add(rel, AccessExclusive, false, false,
			"DROP COLUMN marks the column dead in the catalogue; the space is reclaimed later")

	case a.at("ALTER"):
		a.skip(1)
		a.optional("COLUMN")
		alterTableAlterColumn(a, rel, acc, o)

	case a.at("VALIDATE", "CONSTRAINT"):
		acc.add(rel, ShareUpdateExclusive, false, true,
			"VALIDATE CONSTRAINT scans the table without blocking reads or writes — "+
				"which is the whole point of adding it NOT VALID first")

	case a.at("SET", "LOGGED"), a.at("SET", "UNLOGGED"):
		acc.add(rel, AccessExclusive, true, true,
			"changing the logged state rewrites the table")

	case a.at("SET", "TABLESPACE"):
		acc.add(rel, AccessExclusive, true, true,
			"moving to another tablespace copies every row to the new location")

	case a.at("SET", "SCHEMA"), a.at("OWNER"), a.at("RENAME"):
		acc.add(rel, AccessExclusive, false, false,
			"a catalogue-only change, taken under the strongest lock")

	case a.at("CLUSTER", "ON"), a.at("SET", "WITHOUT", "CLUSTER"):
		acc.add(rel, ShareUpdateExclusive, false, false,
			"this records a preference and does not reorder anything now")

	case a.at("ATTACH", "PARTITION"):
		attachPartition(a, rel, acc, o)

	case a.at("DETACH", "PARTITION"):
		detachPartition(a, rel, acc, o)

	case a.at("ENABLE"), a.at("DISABLE"):
		acc.add(rel, ShareRowExclusive, false, false,
			"enabling or disabling a trigger blocks writes but not reads")

	case a.at("SET"):
		acc.add(rel, ShareUpdateExclusive, false, false,
			"a storage parameter change does not block reads or writes")

	default:
		acc.add(rel, AccessExclusive, false, false,
			"ALTER TABLE takes ACCESS EXCLUSIVE unless the form is one of the documented exceptions, "+
				"and this form is not one this predictor recognises")
	}
}

// readStatement handles SELECT and its relatives.
//
// The relation is taken from the first FROM, which is the one a lock report
// wants: a migration that reads at all almost always reads one table, and a
// prediction naming no relation cannot be checked against the server.
func readStatement(c *cursor, acc *collector, o Options) {
	rel := ""

	probe := &cursor{toks: c.toks, i: c.i}
	if probe.skipToWord("FROM") {
		rel = probe.relation(o)
	}

	if c.hasWords("FOR", "UPDATE") || c.hasWords("FOR", "SHARE") ||
		c.hasWords("FOR", "NO", "KEY", "UPDATE") || c.hasWords("FOR", "KEY", "SHARE") {
		acc.add(rel, RowShare, false, false,
			"a locking read blocks the DDL that would change the rows out from under it")

		return
	}

	acc.add(rel, AccessShare, false, false,
		"a read takes the weakest lock and conflicts only with ACCESS EXCLUSIVE")
}
