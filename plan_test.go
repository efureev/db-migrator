package migrator

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// applied builds a set of bookkeeping rows in force at the given versions.
func appliedAt(versions ...int64) map[int64]Record {
	out := map[int64]Record{}
	now := time.Now()

	for _, v := range versions {
		finished := now
		out[v] = Record{Version: v, FinishedAt: &finished}
	}

	return out
}

// threeMigrations is the fixture most planning tests run against: versions
// 1, 2 and 3, all with down files.
func threeMigrations(t *testing.T) *Set {
	t.Helper()

	return mustLoad(t, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);", "1_a.down.sql": "DROP TABLE a;",
		"2_b.up.sql": "CREATE TABLE b (id int);", "2_b.down.sql": "DROP TABLE b;",
		"3_c.up.sql": "CREATE TABLE c (id int);", "3_c.down.sql": "DROP TABLE c;",
	})
}

func versionsOf(ms []Migration) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Version
	}

	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestPlanUp(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	cases := []struct {
		name    string
		target  Target
		applied map[int64]Record
		want    []int64
	}{
		{name: "everything from empty", target: All(), applied: appliedAt(), want: []int64{1, 2, 3}},
		{name: "nothing left", target: All(), applied: appliedAt(1, 2, 3), want: nil},
		{name: "the rest", target: All(), applied: appliedAt(1), want: []int64{2, 3}},
		{name: "one step", target: Steps(1), applied: appliedAt(), want: []int64{1}},
		{name: "two steps", target: Steps(2), applied: appliedAt(), want: []int64{1, 2}},
		{name: "more steps than exist", target: Steps(99), applied: appliedAt(), want: []int64{1, 2, 3}},
		{name: "zero steps", target: Steps(0), applied: appliedAt(), want: nil},
		{name: "negative steps", target: Steps(-1), applied: appliedAt(), want: nil},
		{name: "to version 2", target: ToVersion(2), applied: appliedAt(), want: []int64{1, 2}},
		{name: "to a version already applied", target: ToVersion(1), applied: appliedAt(1), want: nil},
		{name: "to a version that does not exist", target: ToVersion(99), applied: appliedAt(), want: []int64{1, 2, 3}},
		{name: "to version 0", target: ToVersion(0), applied: appliedAt(), want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := planUp(set, tc.target, tc.applied, false)
			if err != nil {
				t.Fatalf("planUp: %v", err)
			}

			if !equal(versionsOf(got), tc.want) {
				t.Errorf("planUp(%v) = %v, want %v", tc.target, versionsOf(got), tc.want)
			}
		})
	}
}

func TestPlanDown(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	cases := []struct {
		name    string
		target  Target
		applied map[int64]Record
		want    []int64
	}{
		// Descending: a rollback undoes the most recent change first.
		{name: "everything", target: All(), applied: appliedAt(1, 2, 3), want: []int64{3, 2, 1}},
		{name: "one step", target: Steps(1), applied: appliedAt(1, 2, 3), want: []int64{3}},
		{name: "two steps", target: Steps(2), applied: appliedAt(1, 2, 3), want: []int64{3, 2}},
		{name: "nothing applied", target: All(), applied: appliedAt(), want: nil},
		// ToVersion leaves the named version in force and rolls back above it.
		{name: "to version 1", target: ToVersion(1), applied: appliedAt(1, 2, 3), want: []int64{3, 2}},
		{name: "to version 0 unwinds everything", target: ToVersion(0), applied: appliedAt(1, 2, 3), want: []int64{3, 2, 1}},
		{name: "to the current version does nothing", target: ToVersion(3), applied: appliedAt(1, 2, 3), want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := planDown(set, tc.target, tc.applied)
			if err != nil {
				t.Fatalf("planDown: %v", err)
			}

			if !equal(versionsOf(got), tc.want) {
				t.Errorf("planDown(%v) = %v, want %v", tc.target, versionsOf(got), tc.want)
			}
		})
	}
}

// TestPlanUpRefusesOutOfOrder pins the decision that decides schemas: a pending
// migration older than the current version means two branches merged, and
// applying it would make the resulting schema depend on the merge order.
func TestPlanUpRefusesOutOfOrder(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	// 1 and 3 are in force, 2 arrived later from another branch.
	applied := appliedAt(1, 3)

	_, err := planUp(set, All(), applied, false)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("planUp error = %v, want %v", err, ErrOutOfOrder)
	}

	// The error must say which migration and which version, or it is not
	// actionable at three in the morning.
	if !strings.Contains(err.Error(), "2_b") || !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q does not name the migration and the current version", err)
	}

	got, err := planUp(set, All(), applied, true)
	if err != nil {
		t.Fatalf("planUp with out-of-order allowed: %v", err)
	}

	if !equal(versionsOf(got), []int64{2}) {
		t.Errorf("with out-of-order allowed = %v, want [2]", versionsOf(got))
	}
}

// TestPlanUpTreatsRolledBackAsPending: a migration that was applied and then
// rolled back is not in force, so it runs again.
func TestPlanUpTreatsRolledBackAsPending(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	now := time.Now()
	applied := appliedAt(1, 2)

	rec := applied[2]
	rec.RolledBackAt = &now
	applied[2] = rec

	got, err := planUp(set, All(), applied, false)
	if err != nil {
		t.Fatalf("planUp: %v", err)
	}

	if !equal(versionsOf(got), []int64{2, 3}) {
		t.Errorf("planUp = %v, want [2 3]", versionsOf(got))
	}
}

// TestPlanUpSkipsIncomplete: a row with no FinishedAt is not in force, so the
// planner offers it again. Refusing to run at all while one exists is the
// runner's job, not the planner's — the planner only says what would run.
func TestPlanUpSkipsIncomplete(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	applied := map[int64]Record{1: {Version: 1}} // started, never confirmed

	got, err := planUp(set, All(), applied, false)
	if err != nil {
		t.Fatalf("planUp: %v", err)
	}

	if !equal(versionsOf(got), []int64{1, 2, 3}) {
		t.Errorf("planUp = %v, want [1 2 3]", versionsOf(got))
	}
}

// TestPlanDownRefusesWithoutADownFile checks the rule that matters most about
// ordering: the refusal happens while planning, so a rollback either runs whole
// or does not start. Discovering the gap three versions in is the worst case.
func TestPlanDownRefusesWithoutADownFile(t *testing.T) {
	t.Parallel()

	set := mustLoad(t, map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);", "1_a.down.sql": "DROP TABLE a;",
		"2_b.up.sql": "CREATE TABLE b (id int);", // no down file
		"3_c.up.sql": "CREATE TABLE c (id int);", "3_c.down.sql": "DROP TABLE c;",
	})

	applied := appliedAt(1, 2, 3)

	_, err := planDown(set, All(), applied)
	if !errors.Is(err, ErrMissingDownFile) {
		t.Fatalf("planDown error = %v, want %v", err, ErrMissingDownFile)
	}

	if !strings.Contains(err.Error(), "2_b") {
		t.Errorf("error %q does not name the migration", err)
	}

	// Rolling back only version 3 is fine: the gap is never reached.
	got, err := planDown(set, Steps(1), applied)
	if err != nil {
		t.Fatalf("planDown one step: %v", err)
	}

	if !equal(versionsOf(got), []int64{3}) {
		t.Errorf("planDown one step = %v, want [3]", versionsOf(got))
	}
}

// TestPlanDownRefusesWhenTheFileIsGone: a recorded migration whose file has
// been deleted cannot be rolled back, and saying so is better than skipping it.
func TestPlanDownRefusesWhenTheFileIsGone(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	applied := appliedAt(1, 2, 3)
	applied[4] = Record{Version: 4, Name: "gone", FinishedAt: ptr(time.Now())}

	_, err := planDown(set, All(), applied)
	if !errors.Is(err, ErrMissingMigration) {
		t.Fatalf("planDown error = %v, want %v", err, ErrMissingMigration)
	}

	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error %q does not name the missing migration", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestTargetString(t *testing.T) {
	t.Parallel()

	cases := map[string]Target{
		"all":           All(),
		"1 step":        Steps(1),
		"3 steps":       Steps(3),
		"to version 42": ToVersion(42),
		"to version 0":  ToVersion(0),
	}

	for want, target := range cases {
		if got := target.String(); got != want {
			t.Errorf("Target.String() = %q, want %q", got, want)
		}
	}
}

func TestDirectionString(t *testing.T) {
	t.Parallel()

	if got := DirectionUp.String(); got != "up" {
		t.Errorf("DirectionUp = %q", got)
	}

	if got := DirectionDown.String(); got != "down" {
		t.Errorf("DirectionDown = %q", got)
	}
}

func TestCurrentVersion(t *testing.T) {
	t.Parallel()

	if got := currentVersion(appliedAt()); got != 0 {
		t.Errorf("currentVersion of nothing = %d, want 0", got)
	}

	if got := currentVersion(appliedAt(1, 5, 3)); got != 5 {
		t.Errorf("currentVersion = %d, want 5", got)
	}

	// A rolled-back migration is not in force and does not count as current.
	applied := appliedAt(1, 5)
	rec := applied[5]
	rec.RolledBackAt = ptr(time.Now())
	applied[5] = rec

	if got := currentVersion(applied); got != 1 {
		t.Errorf("currentVersion with 5 rolled back = %d, want 1", got)
	}
}

func TestRecordInForce(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name       string
		rec        Record
		inForce    bool
		incomplete bool
	}{
		{name: "applied", rec: Record{FinishedAt: &now}, inForce: true},
		{name: "started, never confirmed", rec: Record{}, incomplete: true},
		{name: "rolled back", rec: Record{FinishedAt: &now, RolledBackAt: &now}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.rec.InForce(); got != tc.inForce {
				t.Errorf("InForce = %v, want %v", got, tc.inForce)
			}

			if got := tc.rec.Incomplete(); got != tc.incomplete {
				t.Errorf("Incomplete = %v, want %v", got, tc.incomplete)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()

	want := map[State]string{
		StatePending: "pending", StateApplied: "applied", StateModified: "changed",
		StateMissing: "missing", StateRolledBack: "rolled back", StateIncomplete: "incomplete",
	}

	for state, s := range want {
		if got := state.String(); got != s {
			t.Errorf("State(%d).String() = %q, want %q", state, got, s)
		}

		text, err := state.MarshalText()
		if err != nil || string(text) != s {
			t.Errorf("State(%d).MarshalText() = %q, %v; want %q", state, text, err, s)
		}
	}

	for _, state := range []State{StateModified, StateMissing, StateIncomplete} {
		if !state.NeedsAttention() {
			t.Errorf("%v should need attention", state)
		}
	}

	for _, state := range []State{StatePending, StateApplied, StateRolledBack} {
		if state.NeedsAttention() {
			t.Errorf("%v should not need attention", state)
		}
	}
}
