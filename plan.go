package migrator

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/efureev/db-migrator/v2/internal/sqlsplit"
)

// A Direction says which way a run goes.
type Direction uint8

const (
	// DirectionUp applies migrations.
	DirectionUp Direction = iota
	// DirectionDown rolls them back.
	DirectionDown
)

// String reports the direction as it appears in output.
func (d Direction) String() string {
	if d == DirectionDown {
		return "down"
	}

	return "up"
}

// targetKind distinguishes the three shapes a [Target] can take.
type targetKind uint8

const (
	targetAll targetKind = iota
	targetSteps
	targetVersion
)

// A Target says how far a run should go.
//
// It is an opaque value rather than an int because "one step" and "up to
// version 1" are different instructions that would otherwise be spelled the
// same at a call site — and in a rollback that difference is the difference
// between losing a column and losing the schema.
type Target struct {
	kind    targetKind
	n       int
	version int64
}

// All runs as far as there is anything to run.
//
// It is a function rather than a package-level variable so that no caller can
// reassign it: migrator.All = migrator.Steps(0) would be undiscoverable.
func All() Target { return Target{kind: targetAll} }

// Steps runs at most n migrations. A non-positive n runs nothing.
func Steps(n int) Target { return Target{kind: targetSteps, n: n} }

// ToVersion runs until the database stands at v.
//
// Up stops having applied v. Down stops having rolled back everything above v,
// leaving v itself in force — so ToVersion(0) rolls back everything.
func ToVersion(v int64) Target { return Target{kind: targetVersion, version: v} }

// String reports the target as it appears in output.
func (t Target) String() string {
	switch t.kind {
	case targetSteps:
		if t.n == 1 {
			return "1 step"
		}

		return fmt.Sprintf("%d steps", t.n)

	case targetVersion:
		return fmt.Sprintf("to version %d", t.version)

	case targetAll:
		return "all"

	default:
		return "all"
	}
}

// A Step is one migration a plan would run.
type Step struct {
	// Migration is the migration this step would run.
	Migration Migration
	// Transactional reports whether the step runs inside a transaction. It is
	// false for a migration carrying the no-transaction directive, and that
	// changes what a failure leaves behind — see [Report].
	Transactional bool
	// SQL is what would actually be sent, after placeholder substitution, split
	// into the statements it would be sent as. For a transactional step that is
	// one element: the body goes to the server whole.
	SQL []string
	// Predictions is what each statement is expected to lock, rewrite or scan.
	//
	// It is empty when the plan was built without a server to ask, and it is a
	// heuristic in any case — see [Migrator.Plan].
	Predictions []LockPrediction
}

// Heaviest reports the strongest lock this step is predicted to take, and false
// when nothing was predicted.
func (s Step) Heaviest() (LockPrediction, bool) { return heaviest(s.Predictions) }

// HeavyPredictions reports the predictions worth putting in front of a person:
// the ones that block writes, rewrite a table or scan one.
func (s Step) HeavyPredictions() []LockPrediction {
	out := make([]LockPrediction, 0, len(s.Predictions))

	for _, p := range s.Predictions {
		if p.Heavy() {
			out = append(out, p)
		}
	}

	return out
}

// A Plan is what a run would do, without doing it.
type Plan struct {
	// Direction says which way the run would go.
	Direction Direction
	// Target is how far it was asked to go.
	Target Target
	// Steps are the migrations it would run, in the order it would run them.
	Steps []Step
}

// Len reports how many migrations the plan would run.
func (p *Plan) Len() int { return len(p.Steps) }

// Versions reports the versions the plan would run, in order.
func (p *Plan) Versions() []int64 {
	out := make([]int64, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s.Migration.Version
	}

	return out
}

// Empty reports whether the plan would do nothing.
func (p *Plan) Empty() bool { return len(p.Steps) == 0 }

// planUp selects the migrations to apply, in ascending order.
//
// applied holds the versions currently in force, keyed by version. A version
// that was applied and then rolled back is not in force and is pending again.
func planUp(set *Set, t Target, applied map[int64]Record, allowOutOfOrder bool) ([]Migration, error) {
	current := currentVersion(applied)

	var pending []Migration

	for _, m := range set.migrations {
		if rec, ok := applied[m.Version]; ok && rec.InForce() {
			continue
		}

		// A pending migration older than the current version means two branches
		// were merged in some order. Refused by default: whichever order the
		// merge happened in would otherwise decide the schema, which is exactly
		// the class of divergence the checksum exists to catch.
		if m.Version < current && !allowOutOfOrder {
			return nil, fmt.Errorf("%w: %s is below the current version %d",
				ErrOutOfOrder, m, current)
		}

		pending = append(pending, m)
	}

	return limit(pending, t, DirectionUp), nil
}

// planDown selects the migrations to roll back, in descending order.
func planDown(set *Set, t Target, applied map[int64]Record) ([]Migration, error) {
	inForce := make([]int64, 0, len(applied))

	for v, rec := range applied {
		if rec.InForce() {
			inForce = append(inForce, v)
		}
	}

	// Descending: a rollback undoes the most recent change first.
	slices.Sort(inForce)
	slices.Reverse(inForce)

	out := make([]Migration, 0, len(inForce))

	for _, v := range inForce {
		m, ok := set.ByVersion(v)
		if !ok {
			// Rolling back needs the down file, and there is none to be had.
			// Reported here rather than mid-run: rolling back three of five
			// versions and stopping at a missing file is the worst outcome.
			return nil, fmt.Errorf("%w: version %d (%s)",
				ErrMissingMigration, v, applied[v].Name)
		}

		out = append(out, m)
	}

	out = limit(out, t, DirectionDown)

	for _, m := range out {
		if !m.HasDown() {
			return nil, fmt.Errorf("%w: %s", ErrMissingDownFile, m)
		}
	}

	return out, nil
}

// limit trims an ordered candidate list to what the target allows.
func limit(ms []Migration, t Target, d Direction) []Migration {
	switch t.kind {
	case targetAll:
		return ms

	case targetSteps:
		if t.n <= 0 {
			return nil
		}

		return ms[:min(t.n, len(ms))]

	case targetVersion:
		for i, m := range ms {
			// Going up, the target version is applied and the run stops after
			// it. Going down, the target version stays in force and the run
			// stops before it.
			if d == DirectionUp && m.Version > t.version {
				return ms[:i]
			}

			if d == DirectionDown && m.Version <= t.version {
				return ms[:i]
			}
		}

		return ms

	default:
		return ms
	}
}

// currentVersion reports the highest version currently in force, or 0.
func currentVersion(applied map[int64]Record) int64 {
	var current int64

	for v, rec := range applied {
		if rec.InForce() && v > current {
			current = v
		}
	}

	return current
}

// splitForPlan renders the statements a no-transaction step would send, so a
// reviewer sees exactly what goes to the server one message at a time.
func splitForPlan(sql string) ([]string, error) {
	stmts, err := sqlsplit.Split(sql)
	if err != nil {
		return nil, err
	}

	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.SQL
	}

	return out, nil
}

// Text writes the plan as a person reads it.
//
// The lock line under each statement is the reason this command is worth
// running at all: "ACCESS EXCLUSIVE on orders (~8 900 000 rows), REWRITES THE
// TABLE" is a sentence somebody can act on before the deploy, and the same
// fact discovered during one is an incident.
func (p *Plan) Text(w io.Writer) error {
	var b strings.Builder

	if p.Empty() {
		_, err := io.WriteString(w, "  Nothing to do.\n")

		return err
	}

	fmt.Fprintf(&b, "  Plan  %d migration(s) %s\n", p.Len(), p.Direction)

	for _, step := range p.Steps {
		kind := "transactional"
		if !step.Transactional {
			kind = "no-transaction"
		}

		stmts := analysisStatements(step)

		fmt.Fprintf(&b, "\n    %s  %s, %d statement(s)\n", step.Migration, kind, len(stmts))

		if len(step.Predictions) == 0 {
			continue
		}

		for i, sql := range stmts {
			preds := predictionsFor(step.Predictions, i+1)
			if len(preds) == 0 {
				continue
			}

			fmt.Fprintf(&b, "      %s\n", firstLine(sql))

			for _, pred := range preds {
				fmt.Fprintf(&b, "        %s\n", describe(pred))

				// The advice is worth the lines only when there is something to
				// decide. Printing it under every catalogue-only ALTER would
				// train people to skip the whole block.
				if pred.Heavy() {
					for _, line := range wrap(pred.Reason, 72) {
						fmt.Fprintf(&b, "        %s\n", line)
					}
				}
			}
		}
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the plan in the shape a CI step parses.
func (p *Plan) JSON(w io.Writer) error {
	type prediction struct {
		Statement int    `json:"statement"`
		Relation  string `json:"relation,omitempty"`
		Level     string `json:"level"`
		Rewrites  bool   `json:"rewrites"`
		Scans     bool   `json:"scans"`
		Rows      int64  `json:"rows"`
		Reason    string `json:"reason"`
	}

	type step struct {
		Version       int64        `json:"version"`
		Name          string       `json:"name"`
		Transactional bool         `json:"transactional"`
		Statements    []string     `json:"statements"`
		Predictions   []prediction `json:"predictions"`
	}

	out := struct {
		Format    int    `json:"format"`
		Direction string `json:"direction"`
		Target    string `json:"target"`
		Count     int    `json:"count"`
		Steps     []step `json:"steps"`
	}{
		Format: JSONFormat, Direction: p.Direction.String(),
		Target: p.Target.String(), Count: p.Len(),
		Steps: make([]step, 0, len(p.Steps)),
	}

	for _, s := range p.Steps {
		item := step{
			Version: s.Migration.Version, Name: s.Migration.Name,
			Transactional: s.Transactional, Statements: analysisStatements(s),
			Predictions: make([]prediction, 0, len(s.Predictions)),
		}

		for _, pred := range s.Predictions {
			item.Predictions = append(item.Predictions, prediction{
				Statement: pred.Statement, Relation: pred.Relation,
				Level: pred.Level.String(), Rewrites: pred.Rewrites,
				Scans: pred.Scans, Rows: pred.Rows, Reason: pred.Reason,
			})
		}

		out.Steps = append(out.Steps, item)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// predictionsFor reports the predictions of one statement.
func predictionsFor(preds []LockPrediction, statement int) []LockPrediction {
	out := make([]LockPrediction, 0, 2)

	for _, p := range preds {
		if p.Statement == statement {
			out = append(out, p)
		}
	}

	return out
}

// describe reports one prediction as the single line under a statement.
func describe(p LockPrediction) string {
	switch p.Level {
	case LockNone:
		return "no table is locked"
	case LockUnknown:
		return "what this locks is unknown — this predictor does not recognise the statement"
	}

	where := p.Relation
	if where == "" {
		where = "an unnamed relation"
	}

	out := p.Level.String() + " on " + where

	if p.Rows >= 0 {
		out += " (~" + groupDigits(p.Rows) + " rows)"
	}

	switch {
	case p.Rewrites:
		out += ", REWRITES THE TABLE"
	case p.Scans:
		out += ", scans the table"
	default:
		out += ", no rewrite"
	}

	return out
}

// groupDigits writes a row count the way a person reads one, because the
// difference between 8900000 and 890000 is the difference between a warning
// worth heeding and one worth ignoring, and nobody counts digits.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)

	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}

	var b strings.Builder

	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}

		b.WriteRune(c)
	}

	return sign + b.String()
}

// wrap breaks prose into lines of at most n characters, on word boundaries.
func wrap(s string, n int) []string {
	var (
		out  []string
		line string
	)

	for word := range strings.FieldsSeq(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= n:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}

	if line != "" {
		out = append(out, line)
	}

	return out
}
