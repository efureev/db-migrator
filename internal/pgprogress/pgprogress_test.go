package pgprogress

import (
	"strings"
	"testing"
	"time"
)

// A Go table and not a testdata corpus, deliberately. The corpus next door in
// internal/pglock exists for the reason its own header gives: an open-ended
// domain, dozens of statement forms, rules that turn on the server version.
// What is tested here is a handful of pure functions over a struct, and a table
// in Go gets them type-checked. Adding a second format would hide the cases,
// not organise them.

func all(present bool) [viewCount]bool {
	var out [viewCount]bool
	for i := range out {
		out[i] = present
	}

	return out
}

func TestBuiltQueryNamesOnlyTheViewsThatExist(t *testing.T) {
	t.Parallel()

	views := map[int]string{
		viewCreateIndex: "pg_stat_progress_create_index",
		viewVacuum:      "pg_stat_progress_vacuum",
		viewAnalyze:     "pg_stat_progress_analyze",
		viewCluster:     "pg_stat_progress_cluster",
		viewCopy:        "pg_stat_progress_copy",
	}

	// One missing view at a time: the PostgreSQL 13 case is copy, and the query
	// that names a view this server has not got fails whole rather than losing
	// one branch of itself.
	for missing, name := range views {
		present := all(true)
		present[missing] = false

		q := buildQuery(present)

		if strings.Contains(q, name) {
			t.Errorf("the query names %s, which this server does not have:\n%s", name, q)
		}

		for other, otherName := range views {
			if other != missing && !strings.Contains(q, otherName) {
				t.Errorf("%s is missing from the query built without %s", otherName, name)
			}
		}
	}
}

func TestAServerWithNoProgressViewsStillAnswers(t *testing.T) {
	t.Parallel()

	q := buildQuery(all(false))

	if strings.Contains(q, "pg_stat_progress") {
		t.Errorf("the query reads a progress view on a server that has none:\n%s", q)
	}

	// The activity half is the whole point: it is what reports a rewrite, a
	// backfill and a lock wait, none of which any progress view knows about.
	for _, want := range []string{"pg_stat_activity", "wait_event", "clock_timestamp()", "$1"} {
		if !strings.Contains(q, want) {
			t.Errorf("the query does not mention %s:\n%s", want, q)
		}
	}
}

func TestEveryQueryReadsTheStatementAgeFromTheClock(t *testing.T) {
	t.Parallel()

	// now() is the transaction's start time. Nothing wraps a poll in a
	// transaction today, and the day something does, the age of the statement
	// would stop moving and the line would say a live migration was frozen.
	for _, present := range [][viewCount]bool{all(true), all(false)} {
		q := buildQuery(present)

		if strings.Contains(q, "now()") {
			t.Errorf("the query ages the statement with now() instead of clock_timestamp():\n%s", q)
		}
	}
}

func TestOneViewNeedsNoUnion(t *testing.T) {
	t.Parallel()

	var present [viewCount]bool
	present[viewVacuum] = true

	if q := buildQuery(present); strings.Contains(q, "UNION ALL") {
		t.Errorf("a single branch was joined to itself:\n%s", q)
	}
}

func TestPercent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		done, total int64
		wantPct     int
		wantKnown   bool
	}{
		{name: "the ordinary case", done: 30, total: 100, wantPct: 30, wantKnown: true},
		// An index build in its first phase genuinely reports 0 of 0. It is the
		// normal state, not an exotic one, and it must not read as 0% — still
		// less as a division by zero.
		{name: "the total is not known yet", done: 0, total: 0, wantKnown: false},
		{name: "nothing done of a known total", done: 0, total: 41200000, wantPct: 0, wantKnown: true},
		// A partitioned index build counts partitions in one and blocks in the
		// other. Saying 140% is honest; clamping it to 100 leaves somebody
		// watching a number that has stopped moving.
		{name: "a partitioned build overshoots", done: 140, total: 100, wantPct: 140, wantKnown: true},
		{name: "large enough to overflow a naive multiply", done: 1 << 60, total: 1 << 61, wantPct: 50, wantKnown: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got, known := Snapshot{Done: c.done, Total: c.total}.Percent()
			if known != c.wantKnown {
				t.Fatalf("Percent() known = %v, want %v", known, c.wantKnown)
			}

			if known && got != c.wantPct {
				t.Errorf("Percent() = %d, want %d", got, c.wantPct)
			}
		})
	}
}

func TestWaitReadsLikeThePostgresDocumentation(t *testing.T) {
	t.Parallel()

	cases := []struct{ kind, event, want string }{
		{"Lock", "relation", "Lock:relation"},
		{"Timeout", "PgSleep", "Timeout:PgSleep"},
		{"", "", ""},
		{"Lock", "", "Lock"},
	}

	for _, c := range cases {
		s := Snapshot{WaitEventType: c.kind, WaitEvent: c.event}
		if got := s.Wait(); got != c.want {
			t.Errorf("Wait() of %q/%q = %q, want %q", c.kind, c.event, got, c.want)
		}
	}
}

func TestRestrictedIsABlankedRowAndNotAnEmptyOne(t *testing.T) {
	t.Parallel()

	if !(Snapshot{}).Restricted() {
		t.Error("a row with no state at all is what a role that may not look sees")
	}

	live := Snapshot{State: "active", StatementAge: time.Minute}
	if live.Restricted() {
		t.Error("an active backend was reported as invisible")
	}
}
