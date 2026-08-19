package migrator

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChecksumErrorReportsBothSums(t *testing.T) {
	t.Parallel()

	err := &ChecksumError{
		Version:  20240118120000,
		Name:     "add_users_email_index",
		File:     "20240118120000_add_users_email_index.up.sql",
		Recorded: "9f2a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8",
		Actual:   "0b77c8d9eaf0112233445566778899aabbccddeeff00112233445566778899aa",
	}

	msg := err.Error()

	// Both sums must appear, shortened: an operator comparing them by eye needs
	// them side by side, and sixty-four hex characters twice is unreadable.
	for _, want := range []string{err.File, "9f2a1b3c4d5e", "0b77c8d9eaf0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}

	if strings.Contains(msg, err.Recorded) {
		t.Errorf("message %q printed the full checksum", msg)
	}

	// Both matching styles must work: the sentinel for control flow, the
	// concrete type for the detail.
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Error("errors.Is does not match ErrChecksumMismatch")
	}

	var target *ChecksumError
	if !errors.As(error(err), &target) || target.Version != err.Version {
		t.Error("errors.As did not recover the detail")
	}
}

func TestMigrationErrorNamesThePlace(t *testing.T) {
	t.Parallel()

	cause := errors.New(`relation "sessions" does not exist`)

	withLine := &MigrationError{
		Version: 20240501080000, Name: "drop_legacy_sessions",
		Direction: DirectionUp,
		File:      "20240501080000_drop_legacy_sessions.up.sql",
		Statement: 2, Line: 7,
		Err: cause,
	}

	if got, want := withLine.Error(), "20240501080000_drop_legacy_sessions.up.sql:7"; !strings.Contains(got, want) {
		t.Errorf("message %q does not start with %q", got, want)
	}

	if !strings.Contains(withLine.Error(), cause.Error()) {
		t.Errorf("message %q does not include the cause", withLine.Error())
	}

	if !errors.Is(withLine, cause) {
		t.Error("errors.Is does not reach the cause")
	}

	// PostgreSQL does not always report a position; then the file is named
	// without a line rather than with a misleading line 0.
	noLine := &MigrationError{File: "0001_a.up.sql", Err: cause}
	if got := noLine.Error(); strings.Contains(got, ":0") {
		t.Errorf("message %q invented a line number", got)
	}
}

func TestSourceErrorNamesTheFile(t *testing.T) {
	t.Parallel()

	withDetail := &SourceError{
		File: "0001_a.up.sql", Detail: "version 1 is already claimed by b",
		Err: ErrDuplicateVersion,
	}

	msg := withDetail.Error()
	for _, want := range []string{"0001_a.up.sql", ErrDuplicateVersion.Error(), "already claimed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}

	if !errors.Is(withDetail, ErrDuplicateVersion) {
		t.Error("errors.Is does not match the sentinel")
	}

	plain := &SourceError{File: "0002_b.down.sql", Err: ErrOrphanDownFile}
	if got := plain.Error(); strings.Contains(got, "()") {
		t.Errorf("message %q has an empty detail in brackets", got)
	}
}

func TestShort(t *testing.T) {
	t.Parallel()

	long := "0123456789abcdef"
	if got, want := short(long), "0123456789ab"; got != want {
		t.Errorf("short(%q) = %q, want %q", long, got, want)
	}

	for _, in := range []string{"", "abc", "0123456789ab"} {
		if got := short(in); got != in {
			t.Errorf("short(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestPlanAccessors(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	empty := &Plan{Direction: DirectionUp, Target: All()}
	if !empty.Empty() || empty.Len() != 0 || len(empty.Versions()) != 0 {
		t.Errorf("empty plan reports %d steps", empty.Len())
	}

	var steps []Step

	for m := range set.All() {
		steps = append(steps, Step{Migration: m, Transactional: true, SQL: []string{m.UpSQL()}})
	}

	p := &Plan{Direction: DirectionUp, Target: All(), Steps: steps}

	if p.Empty() {
		t.Error("a plan with three steps reports itself empty")
	}

	if p.Len() != 3 {
		t.Errorf("Len = %d, want 3", p.Len())
	}

	if got := p.Versions(); !equal(got, []int64{1, 2, 3}) {
		t.Errorf("Versions = %v, want [1 2 3]", got)
	}
}

func TestSetAccessors(t *testing.T) {
	t.Parallel()

	set := threeMigrations(t)

	var seen []int64
	for m := range set.All() {
		seen = append(seen, m.Version)

		if m.UpSQL() == "" {
			t.Errorf("version %d has an empty up body", m.Version)
		}

		if m.HasDown() && m.DownSQL() == "" {
			t.Errorf("version %d claims a down file with an empty body", m.Version)
		}
	}

	if !equal(seen, []int64{1, 2, 3}) {
		t.Errorf("All yielded %v, want [1 2 3]", seen)
	}

	// All must be restartable: an iterator consumed once and then empty would
	// make a second pass over the set silently do nothing.
	var again int
	for range set.All() {
		again++
	}

	if again != 3 {
		t.Errorf("second pass over All yielded %d, want 3", again)
	}

	var none Set
	if _, ok := none.Latest(); ok {
		t.Error("Latest found something in an empty set")
	}
}

func TestStatusAccessors(t *testing.T) {
	t.Parallel()

	now := time.Now()
	finished := &now

	status := &Status{
		Schema: "public", Table: "schema_migrations", Initialised: true,
		Entries: []Entry{
			{Version: 1, Name: "a", State: StateApplied, Record: &Record{Version: 1, FinishedAt: finished}},
			{Version: 2, Name: "b", State: StateModified, Record: &Record{Version: 2, FinishedAt: finished}},
			{Version: 3, Name: "c", State: StateRolledBack, Record: &Record{
				Version: 3, FinishedAt: finished, RolledBackAt: finished,
			}},
			{Version: 4, Name: "d", State: StatePending},
			{Version: 5, Name: "e", State: StateMissing, Record: &Record{Version: 5, FinishedAt: finished}},
			{Version: 6, Name: "f", State: StateIncomplete, Record: &Record{Version: 6}},
		},
	}

	// Current is the highest version in force: 5 is recorded and in force even
	// though its file is gone, 3 was rolled back, 6 never finished.
	if got := status.Current(); got != 5 {
		t.Errorf("Current = %d, want 5", got)
	}

	pending := status.Pending()
	if len(pending) != 2 || pending[0].Version != 3 || pending[1].Version != 4 {
		t.Errorf("Pending = %v, want versions [3 4]", entryVersions(pending))
	}

	drifted := status.Drifted()
	if len(drifted) != 3 {
		t.Errorf("Drifted = %v, want versions [2 5 6]", entryVersions(drifted))
	}

	var none Status
	if got := none.Current(); got != 0 {
		t.Errorf("Current of an empty status = %d, want 0", got)
	}
}

func entryVersions(entries []Entry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.Version
	}

	return out
}

func TestStateStringUnknown(t *testing.T) {
	t.Parallel()

	if got := State(99).String(); got != "unknown" {
		t.Errorf("State(99) = %q, want %q", got, "unknown")
	}

	if got := Direction(99).String(); got != "up" {
		t.Errorf("Direction(99) = %q, want the safe default %q", got, "up")
	}

	if got := (Target{kind: targetKind(99)}).String(); got != "all" {
		t.Errorf("unknown target = %q, want %q", got, "all")
	}
}

func TestUnwrapDetail(t *testing.T) {
	t.Parallel()

	if got := unwrapDetail(nil); got != "" {
		t.Errorf("unwrapDetail(nil) = %q, want empty", got)
	}

	if got := unwrapDetail(errors.New("boom")); got != "boom" {
		t.Errorf("unwrapDetail = %q, want %q", got, "boom")
	}
}
