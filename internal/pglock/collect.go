package pglock

// A collector gathers the predictions of one statement, merging the ones that
// land on the same relation.
//
// ALTER TABLE takes one lock per table however many actions it lists, so
// reporting each action separately would say a statement takes ACCESS EXCLUSIVE
// three times. The merge keeps the strongest claim and the reason that earned
// it.
type collector struct {
	statement int
	opts      Options
	preds     []Prediction
	index     map[string]int
}

// add records one claim about one relation.
func (c *collector) add(relation string, level Level, rewrites, scans bool, reason string) {
	p := Prediction{
		Statement: c.statement, Relation: relation, Level: level,
		Rewrites: rewrites, Scans: scans, Rows: -1, Reason: reason,
	}

	if c.index == nil {
		c.index = make(map[string]int)
	}

	at, seen := c.index[relation]
	if !seen {
		c.index[relation] = len(c.preds)
		c.preds = append(c.preds, p)

		return
	}

	c.preds[at] = merge(c.preds[at], p)
}

// merge folds a new claim into an existing one for the same relation.
func merge(a, b Prediction) Prediction {
	out := a

	if b.Level > out.Level {
		out.Level = b.Level
	}

	out.Rewrites = a.Rewrites || b.Rewrites
	out.Scans = a.Scans || b.Scans

	// The reason shown is the one a person most needs: a rewrite outranks a
	// scan, and a scan outranks a heavier lock. Showing the reason of the
	// strongest lock would hide "and it also rewrites the table" behind
	// "ACCESS EXCLUSIVE", which is the less useful half of the sentence.
	switch {
	case severity(b) > severity(a):
		out.Reason = b.Reason
	case severity(b) == severity(a) && b.Level > a.Level:
		out.Reason = b.Reason
	}

	return out
}

// severity ranks a claim by how much a person needs to hear about it.
func severity(p Prediction) int {
	switch {
	case p.Rewrites:
		return 3
	case p.Scans:
		return 2
	default:
		return 1
	}
}

// result reports what was collected, in the order the relations first appeared.
func (c *collector) result() []Prediction { return c.preds }
