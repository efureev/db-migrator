package pglock

import (
	"fmt"
	"strings"
)

// fastDefaultVersion is the server_version_num from which ADD COLUMN with a
// non-volatile DEFAULT stopped rewriting the table. Before PostgreSQL 11 it
// copied every row.
const fastDefaultVersion = 110000

// notNullByCheckVersion is the server_version_num from which SET NOT NULL can
// skip its scan if a valid CHECK (col IS NOT NULL) already proves it.
const notNullByCheckVersion = 120000

// concurrentAttachVersion is the server_version_num from which ATTACH PARTITION
// stopped taking ACCESS EXCLUSIVE on the parent.
const concurrentAttachVersion = 120000

// alterTableAdd handles ALTER TABLE ... ADD ….
func alterTableAdd(a *cursor, rel string, acc *collector, o Options) {
	named := a.optional("CONSTRAINT")
	if named {
		a.skip(1) // the constraint name
	}

	switch {
	case a.at("CHECK"):
		addCheck(a, rel, acc)

	case a.at("FOREIGN", "KEY"):
		addForeignKey(a, rel, acc, o)

	case a.at("UNIQUE"), a.at("PRIMARY", "KEY"):
		addIndexBackedConstraint(a, rel, acc)

	case a.at("EXCLUDE"):
		acc.add(rel, AccessExclusive, false, true,
			"an exclusion constraint builds an index over the whole table under the lock")

	case named:
		acc.add(rel, AccessExclusive, false, false,
			"adding a constraint takes the strongest lock")

	default:
		addColumn(a, rel, acc, o)
	}
}

// addCheck handles ADD CONSTRAINT … CHECK (…).
func addCheck(a *cursor, rel string, acc *collector) {
	if a.hasWords("NOT", "VALID") {
		acc.add(rel, AccessExclusive, false, false,
			"NOT VALID skips the scan; the lock is brief, and VALIDATE CONSTRAINT does the rest "+
				"later without blocking anything")

		return
	}

	acc.add(rel, AccessExclusive, false, true,
		"a CHECK constraint is verified against every existing row before the lock is released — "+
			"add it NOT VALID and VALIDATE it afterwards to avoid that")
}

// addForeignKey handles ADD CONSTRAINT … FOREIGN KEY … REFERENCES other.
//
// This is the one ALTER TABLE form that locks a table the statement is not
// about, and the one people are most surprised by: the referenced table is
// locked too, so a foreign key onto a busy lookup table blocks writes to the
// lookup table.
func addForeignKey(a *cursor, rel string, acc *collector, o Options) {
	notValid := a.hasWords("NOT", "VALID")

	other := ""

	probe := &cursor{toks: a.toks, i: a.i}
	if probe.skipToWord("REFERENCES") {
		other = probe.relation(o)
	}

	reason := "adding a foreign key scans the referencing table to check every existing row"
	if notValid {
		reason = "NOT VALID skips the scan, and the lock is still taken on both tables"
	}

	acc.add(rel, ShareRowExclusive, false, !notValid, reason)

	if other != "" && other != rel {
		acc.add(other, ShareRowExclusive, false, false,
			"the referenced table is locked too, which blocks writes to it for the duration")
	}
}

// addIndexBackedConstraint handles ADD UNIQUE / ADD PRIMARY KEY.
func addIndexBackedConstraint(a *cursor, rel string, acc *collector) {
	if a.hasWords("USING", "INDEX") {
		acc.add(rel, AccessExclusive, false, false,
			"USING INDEX adopts an index that already exists, so nothing is scanned now — "+
				"build it with CREATE INDEX CONCURRENTLY first and this is the cheap half")

		return
	}

	acc.add(rel, AccessExclusive, false, true,
		"this builds an index over the whole table while holding the strongest lock; "+
			"CREATE UNIQUE INDEX CONCURRENTLY followed by ADD CONSTRAINT … USING INDEX does not")
}

// addColumn handles ADD [COLUMN] name type ….
func addColumn(a *cursor, rel string, acc *collector, o Options) {
	a.optional("COLUMN")
	a.optional("IF", "NOT", "EXISTS")

	if a.hasWords("GENERATED", "ALWAYS", "AS") && a.hasWords("STORED") {
		acc.add(rel, AccessExclusive, true, true,
			"a stored generated column is computed for every existing row, which rewrites the table")

		return
	}

	rewrites, reason := addColumnDefault(a, o)

	scans := a.hasWords("PRIMARY", "KEY") || a.hasWords("UNIQUE")
	if scans && !rewrites {
		reason = "the column is added cheaply, and the unique index over it is built under the lock"
	}

	acc.add(rel, AccessExclusive, rewrites, scans, reason)
}

// addColumnDefault decides whether the DEFAULT on a new column rewrites the
// table, and says why.
func addColumnDefault(a *cursor, o Options) (rewrites bool, reason string) {
	expr, ok := defaultExpr(a)
	if !ok {
		return false, "adding a column without a default only writes a catalogue entry, " +
			"however large the table is"
	}

	version, known := o.known()

	switch {
	case !known:
		return true, "a DEFAULT on a new column rewrote the whole table before PostgreSQL 11, " +
			"and the server version is not known here — connect, or read this as the pessimistic answer"

	case version < fastDefaultVersion:
		return true, fmt.Sprintf(
			"this server is older than PostgreSQL 11, where a DEFAULT on a new column "+
				"rewrites every row (server_version_num %d)", version)
	}

	if fn, volatile := volatileCall(expr); volatile {
		return true, fmt.Sprintf(
			"the default calls %s(), which is volatile, so every row is written with its own value — "+
				"a constant default would not rewrite the table", fn)
	}

	if fn, unknownFn := unrecognisedCall(expr); unknownFn {
		return true, fmt.Sprintf(
			"the default calls %s(), and this predictor does not know whether it is volatile; "+
				"a volatile default rewrites the table, so it is assumed to", fn)
	}

	return false, "since PostgreSQL 11 a constant default is stored once in the catalogue " +
		"instead of being written to every row"
}

// defaultExpr reports the tokens of the DEFAULT expression of a column
// definition, and whether there was one.
func defaultExpr(a *cursor) ([]token, bool) {
	probe := &cursor{toks: a.toks, i: a.i}
	if !probe.skipToWord("DEFAULT") {
		return nil, false
	}

	start := probe.i
	depth := 0

	for j := start; j < len(probe.toks); j++ {
		t := probe.toks[j]

		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
			}

			continue
		}

		if depth == 0 && t.kind == tokWord && columnClauseEnd[t.upper] {
			return probe.toks[start:j], true
		}
	}

	return probe.toks[start:], true
}

// columnClauseEnd are the keywords that can only begin the clause after a
// DEFAULT expression, and so mark where that expression stops.
var columnClauseEnd = map[string]bool{
	"NOT": true, "NULL": true, "CHECK": true, "REFERENCES": true,
	"UNIQUE": true, "PRIMARY": true, "COLLATE": true, "CONSTRAINT": true,
	"GENERATED": true, "DEFERRABLE": true,
}

// volatileFunctions are the ones whose value differs per row, which is exactly
// what forces the rewrite.
var volatileFunctions = map[string]bool{
	"random": true, "gen_random_uuid": true, "uuid_generate_v1": true,
	"uuid_generate_v1mc": true, "uuid_generate_v4": true, "clock_timestamp": true,
	"timeofday": true, "nextval": true, "currval": true, "setval": true,
	"pg_backend_pid": true, "random_normal": true,
}

// stableFunctions are evaluated once when the column is added, so the value is
// the same for every row and the fast path applies.
var stableFunctions = map[string]bool{
	"now": true, "current_timestamp": true, "transaction_timestamp": true,
	"statement_timestamp": true, "current_date": true, "current_time": true,
	"localtime": true, "localtimestamp": true, "current_user": true,
	"session_user": true, "user": true, "current_schema": true,
	"current_database": true, "version": true, "md5": true, "upper": true,
	"lower": true, "coalesce": true, "concat": true, "abs": true, "length": true,
}

// calls reports the function names called in an expression: a word immediately
// followed by an opening parenthesis.
func calls(expr []token) []string {
	var out []string

	for i, t := range expr {
		if t.kind != tokWord || i+1 >= len(expr) {
			continue
		}

		if nxt := expr[i+1]; nxt.kind == tokPunct && nxt.text == "(" {
			out = append(out, strings.ToLower(t.text))
		}
	}

	return out
}

// volatileCall reports the first known-volatile function in the expression.
func volatileCall(expr []token) (string, bool) {
	for _, fn := range calls(expr) {
		if volatileFunctions[fn] {
			return fn, true
		}
	}

	return "", false
}

// unrecognisedCall reports the first function the predictor has no opinion
// about, so that the caller can be pessimistic about it out loud.
func unrecognisedCall(expr []token) (string, bool) {
	for _, fn := range calls(expr) {
		if !stableFunctions[fn] && !volatileFunctions[fn] {
			return fn, true
		}
	}

	return "", false
}

// alterTableAlterColumn handles ALTER TABLE ... ALTER [COLUMN] name ….
func alterTableAlterColumn(a *cursor, rel string, acc *collector, o Options) {
	a.relation(o) // the column name, which no rule below needs

	switch {
	case a.at("TYPE"), a.at("SET", "DATA", "TYPE"):
		alterColumnType(a, rel, acc)

	case a.at("SET", "NOT", "NULL"):
		setNotNull(rel, acc, o)

	case a.at("DROP", "NOT", "NULL"):
		acc.add(rel, AccessExclusive, false, false,
			"dropping NOT NULL proves nothing about existing rows, so nothing is scanned")

	case a.at("SET", "DEFAULT"), a.at("DROP", "DEFAULT"):
		acc.add(rel, AccessExclusive, false, false,
			"a default applies to future rows only; existing rows are untouched")

	case a.at("SET", "STATISTICS"):
		acc.add(rel, ShareUpdateExclusive, false, false,
			"a planner statistics target is metadata and blocks nothing")

	case a.at("SET", "STORAGE"), a.at("SET", "COMPRESSION"):
		acc.add(rel, AccessExclusive, false, false,
			"the setting applies to rows written from now on")

	case a.at("SET", "EXPRESSION"):
		acc.add(rel, AccessExclusive, true, true,
			"recomputing a generated column writes every row")

	case a.at("ADD", "GENERATED"), a.at("DROP", "IDENTITY"), a.at("SET", "GENERATED"):
		acc.add(rel, AccessExclusive, false, false,
			"an identity change edits the catalogue and attaches or detaches a sequence")

	case a.at("SET"):
		acc.add(rel, ShareUpdateExclusive, false, false,
			"a per-column attribute change blocks nothing")

	default:
		acc.add(rel, AccessExclusive, false, false,
			"an ALTER COLUMN form this predictor does not recognise; ACCESS EXCLUSIVE is the default")
	}
}

// alterColumnType handles the form that most often surprises people.
func alterColumnType(a *cursor, rel string, acc *collector) {
	if a.hasWords("USING") {
		acc.add(rel, AccessExclusive, true, true,
			"a USING clause means every value is recomputed, which always rewrites the table")

		return
	}

	acc.add(rel, AccessExclusive, true, true,
		"changing a column's type rewrites the table unless the two types are binary-coercible "+
			"(varchar(n) to a wider varchar or to text, and little else). The old type is not in "+
			"the statement, so a rewrite is assumed")
}

// setNotNull handles SET NOT NULL, whose cost depends on the server version and
// on a CHECK this predictor cannot see.
func setNotNull(rel string, acc *collector, o Options) {
	version, known := o.known()

	if known && version >= notNullByCheckVersion {
		acc.add(rel, AccessExclusive, false, true,
			"SET NOT NULL scans the table to prove no row violates it. Since PostgreSQL 12 the scan "+
				"is skipped when a valid CHECK (col IS NOT NULL) already proves it — add that NOT VALID, "+
				"validate it, then this becomes instant")

		return
	}

	acc.add(rel, AccessExclusive, false, true,
		"SET NOT NULL scans the whole table under the lock to prove no row violates it")
}

// attachPartition handles ATTACH PARTITION, which locks two tables differently.
func attachPartition(a *cursor, parent string, acc *collector, o Options) {
	a.skip(2)

	child := a.relation(o)

	version, known := o.known()

	parentLevel := AccessExclusive
	parentReason := "before PostgreSQL 12 attaching a partition blocked all access to the parent"

	if known && version >= concurrentAttachVersion {
		parentLevel = ShareUpdateExclusive
		parentReason = "since PostgreSQL 12 the parent stays readable and writable while a partition is attached"
	}

	acc.add(parent, parentLevel, false, false, parentReason)

	if child != "" {
		acc.add(child, AccessExclusive, false, true,
			"the table being attached is scanned to prove every row belongs in the partition, "+
				"unless a matching CHECK constraint already proves it")
	}
}

// detachPartition handles DETACH PARTITION.
func detachPartition(a *cursor, parent string, acc *collector, o Options) {
	a.skip(2)

	child := a.relation(o)

	if a.optional("CONCURRENTLY") || a.hasWords("CONCURRENTLY") {
		acc.add(parent, ShareUpdateExclusive, false, false,
			"CONCURRENTLY keeps the parent available, in exchange for running in two transactions")

		if child != "" {
			acc.add(child, ShareUpdateExclusive, false, false, "the detached table stays available too")
		}

		return
	}

	acc.add(parent, AccessExclusive, false, false,
		"detaching a partition blocks all access to the parent; DETACH PARTITION CONCURRENTLY does not")

	if child != "" {
		acc.add(child, AccessExclusive, false, false, "the table being detached is locked as well")
	}
}
