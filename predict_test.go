package migrator

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestLockAcknowledgedDirective covers the half of the gate that lives in the
// file. A directive that parsed silently into nothing would waive a limit that
// was never actually waived, which is the worst of the three outcomes.
func TestLockAcknowledgedDirective(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body string
		want LockLevel
		ok   bool
	}{
		"hyphenated": {"-- migrator:lock-acknowledged access-exclusive\nSELECT 1;", AccessExclusive, true},
		"spaced":     {"-- migrator:lock-acknowledged share update exclusive\nSELECT 1;", ShareUpdateExclusive, true},
		"upper":      {"-- migrator:lock-acknowledged SHARE\nSELECT 1;", Share, true},
		"absent":     {"SELECT 1;", LockUnknown, true},
		"nonsense":   {"-- migrator:lock-acknowledged very-exclusive\nSELECT 1;", LockUnknown, false},
		"empty":      {"-- migrator:lock-acknowledged\nSELECT 1;", LockUnknown, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			set, err := Load(fstest.MapFS{
				"1_x.up.sql": &fstest.MapFile{Data: []byte(tc.body)},
			})

			if !tc.ok {
				if err == nil {
					t.Fatal("a lock level that is not one must fail the load, not be ignored")
				}

				if !strings.Contains(err.Error(), "lock-acknowledged") {
					t.Errorf("the error does not name the directive: %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("load: %v", err)
			}

			mig, _ := set.ByVersion(1)
			if got := mig.Directives.LockAcknowledged; got != tc.want {
				t.Errorf("LockAcknowledged = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestAcknowledgementChangesTheChecksum is why the directive belongs in the
// hash: it changes what the run is allowed to do, so a file that gains one is
// not the file that was reviewed.
func TestAcknowledgementChangesTheChecksum(t *testing.T) {
	t.Parallel()

	plain := checksumOf(t, "ALTER TABLE t ADD COLUMN a int;\n")
	acked := checksumOf(t, "-- migrator:lock-acknowledged access-exclusive\n\nALTER TABLE t ADD COLUMN a int;\n")

	if plain == acked {
		t.Error("adding the directive did not change the checksum")
	}
}

func checksumOf(t *testing.T, body string) string {
	t.Helper()

	set, err := Load(fstest.MapFS{"1_x.up.sql": &fstest.MapFile{Data: []byte(body)}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	mig, _ := set.ByVersion(1)

	return mig.Checksum
}

// TestAnalysisStatementsSplitsATransactionalBody fixes the thing that would
// otherwise make every multi-statement migration look like one statement: a
// transactional step sends its body whole, and predicting from that would
// classify the first command and call the rest nothing.
func TestAnalysisStatementsSplitsATransactionalBody(t *testing.T) {
	t.Parallel()

	step := Step{
		Transactional: true,
		SQL:           []string{"ALTER TABLE a ADD COLUMN x int;\nCREATE INDEX i ON a (x);"},
	}

	got := analysisStatements(step)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}

	if !strings.HasPrefix(got[1], "CREATE INDEX") {
		t.Errorf("second statement = %q", got[1])
	}
}

func TestGroupDigits(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0: "0", 7: "7", 42: "42", 999: "999", 1000: "1 000",
		41200000: "41 200 000", 8900000: "8 900 000", -1: "-1",
	}

	for in, want := range cases {
		if got := groupDigits(in); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWrapKeepsWords(t *testing.T) {
	t.Parallel()

	text := "the old type is not in the statement, so a rewrite is assumed here"

	for _, line := range wrap(text, 20) {
		if len(line) > 20 && !strings.Contains(line, " ") {
			continue // a single word longer than the limit has nowhere to break
		}

		if len(line) > 20 {
			t.Errorf("line is %d characters: %q", len(line), line)
		}
	}

	if strings.Join(wrap(text, 20), " ") != text {
		t.Error("wrapping lost or reordered words")
	}
}

// TestDescribeSaysTheScaryPartLast keeps the most important word where the eye
// lands, and keeps the three states distinguishable.
func TestDescribeSaysTheScaryPartLast(t *testing.T) {
	t.Parallel()

	rewrite := describe(LockPrediction{Relation: "orders", Level: AccessExclusive, Rewrites: true, Rows: 8900000})
	if !strings.HasSuffix(rewrite, "REWRITES THE TABLE") {
		t.Errorf("describe = %q", rewrite)
	}

	if !strings.Contains(rewrite, "~8 900 000 rows") {
		t.Errorf("describe did not group the digits: %q", rewrite)
	}

	scan := describe(LockPrediction{Relation: "orders", Level: Share, Scans: true, Rows: -1})
	if !strings.HasSuffix(scan, "scans the table") {
		t.Errorf("describe = %q", scan)
	}

	if strings.Contains(scan, "rows") {
		t.Error("an unknown row count must not be printed at all")
	}

	cheap := describe(LockPrediction{Relation: "orders", Level: AccessExclusive, Rows: 0})
	if !strings.HasSuffix(cheap, "no rewrite") {
		t.Errorf("describe = %q", cheap)
	}

	if !strings.Contains(cheap, "~0 rows") {
		t.Error("an analysed empty table is a known count, not an unknown one")
	}
}

// TestHeavyPredictionsFiltersTheQuietOnes keeps the summary useful.
func TestHeavyPredictionsFiltersTheQuietOnes(t *testing.T) {
	t.Parallel()

	step := Step{Predictions: []LockPrediction{
		{Statement: 1, Level: AccessShare},
		{Statement: 2, Level: RowExclusive},
		{Statement: 3, Level: Share},
		{Statement: 4, Level: ShareUpdateExclusive, Scans: true},
	}}

	heavy := step.HeavyPredictions()
	if len(heavy) != 2 {
		t.Fatalf("got %d heavy predictions, want 2: %+v", len(heavy), heavy)
	}

	worst, ok := step.Heaviest()
	if !ok || worst.Level != Share {
		t.Errorf("Heaviest = %s, %v; want SHARE", worst.Level, ok)
	}
}
