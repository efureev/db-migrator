package pglock

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCorpus runs testdata/corpus.txt: every statement is analysed and compared
// with the predictions written next to it.
func TestCorpus(t *testing.T) {
	t.Parallel()

	for _, c := range loadCorpus(t, "testdata/corpus.txt") {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := Analyze([]string{c.sql}, Options{ServerVersion: c.version})

			if len(got) != len(c.want) {
				t.Fatalf("got %d prediction(s), want %d\nsql:  %s\ngot:  %s\nwant: %s",
					len(got), len(c.want), c.sql, format(got), format(c.want))
			}

			for i := range got {
				if !same(got[i], c.want[i]) {
					t.Errorf("prediction %d differs\nsql:  %s\ngot:  %s\nwant: %s",
						i+1, c.sql, one(got[i]), one(c.want[i]))
				}
			}
		})
	}
}

// TestReasonIsAlwaysGiven keeps the output useful. A prediction without a
// reason is a number somebody has to look up somewhere else, which is the
// failure mode this package was written to avoid.
func TestReasonIsAlwaysGiven(t *testing.T) {
	t.Parallel()

	for _, c := range loadCorpus(t, "testdata/corpus.txt") {
		for _, p := range Analyze([]string{c.sql}, Options{ServerVersion: c.version}) {
			if strings.TrimSpace(p.Reason) == "" {
				t.Errorf("no reason for %s on %q", p.Level, c.sql)
			}
		}
	}
}

// TestRowsStartUnknown fixes the difference between "this table is empty" and
// "nobody asked the server". A zero would read as the first.
func TestRowsStartUnknown(t *testing.T) {
	t.Parallel()

	for _, p := range Analyze([]string{"ALTER TABLE users ADD COLUMN a int"}, Options{}) {
		if p.Rows != -1 {
			t.Errorf("Rows = %d before enrichment, want -1", p.Rows)
		}
	}
}

// TestStatementIndexIsOneBased keeps the number in a message the same as the
// number a person counts to in the file.
func TestStatementIndexIsOneBased(t *testing.T) {
	t.Parallel()

	got := Analyze([]string{"SELECT 1", "SELECT 2", "SELECT 3"}, Options{})
	for i, p := range got {
		if p.Statement != i+1 {
			t.Errorf("statement %d reported as %d", i+1, p.Statement)
		}
	}
}

// TestUnknownIsNotSilent is the property that keeps an unparsed statement from
// reading as a harmless one.
func TestUnknownIsNotSilent(t *testing.T) {
	t.Parallel()

	got := Analyze([]string{"CALL something_unrecognised()"}, Options{})
	if len(got) != 1 || got[0].Level != LevelUnknown {
		t.Fatalf("got %s, want exactly one unknown", format(got))
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	cases := map[string]Level{
		"access exclusive":       AccessExclusive,
		"ACCESS-EXCLUSIVE":       AccessExclusive,
		"access_exclusive":       AccessExclusive,
		"  Access Exclusive  ":   AccessExclusive,
		"AccessExclusiveLock":    LevelUnknown, // the pg_locks spelling is not an input format
		"share update exclusive": ShareUpdateExclusive,
		"SHARE":                  Share,
		"row exclusive lock":     RowExclusive,
	}

	for in, want := range cases {
		got, ok := ParseLevel(in)
		if want == LevelUnknown {
			if ok {
				t.Errorf("ParseLevel(%q) accepted it as %s", in, got)
			}

			continue
		}

		if !ok || got != want {
			t.Errorf("ParseLevel(%q) = %s, %v; want %s", in, got, ok, want)
		}
	}
}

// TestLevelsAreOrdered is what the gate depends on: refusing anything above a
// level has to mean the same thing as PostgreSQL's own ordering.
func TestLevelsAreOrdered(t *testing.T) {
	t.Parallel()

	levels := Levels()
	for i := 1; i < len(levels); i++ {
		if levels[i-1] >= levels[i] {
			t.Fatalf("%s is not weaker than %s", levels[i-1], levels[i])
		}
	}

	if !AccessExclusive.BlocksReads() || Exclusive.BlocksReads() {
		t.Error("only ACCESS EXCLUSIVE blocks reads")
	}

	if !Share.BlocksWrites() || ShareUpdateExclusive.BlocksWrites() {
		t.Error("SHARE blocks writes and SHARE UPDATE EXCLUSIVE does not")
	}
}

// TestPgModeRoundTrips keeps Level.PgMode usable as the bridge to pg_locks,
// which is what the oracle test compares against.
func TestPgModeRoundTrips(t *testing.T) {
	t.Parallel()

	for _, l := range Levels() {
		mode := l.PgMode()
		if mode == "" {
			t.Errorf("%s has no pg_locks spelling", l)
		}

		if strings.Contains(mode, " ") {
			t.Errorf("%s reported %q, which pg_locks would not", l, mode)
		}
	}

	if LevelNone.PgMode() != "" || LevelUnknown.PgMode() != "" {
		t.Error("none and unknown are not lock modes and must have no spelling")
	}
}

// A corpusCase is one statement and what it should predict.
type corpusCase struct {
	name    string
	sql     string
	version int
	want    []Prediction
}

// loadCorpus reads the file described at the top of testdata/corpus.txt.
func loadCorpus(t *testing.T, path string) []corpusCase {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the corpus: %v", err)
	}

	defer func() { _ = f.Close() }()

	var (
		out     []corpusCase
		version = 170000
		line    int
	)

	scan := bufio.NewScanner(f)

	for scan.Scan() {
		line++

		text := strings.TrimSpace(scan.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		key, value, found := strings.Cut(text, ":")
		if !found {
			t.Fatalf("%s:%d: neither a comment nor key: value", path, line)
		}

		value = strings.TrimSpace(value)

		switch strings.TrimSpace(key) {
		case "version":
			v, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("%s:%d: %v", path, line, err)
			}

			version = v

		case "sql":
			out = append(out, corpusCase{
				name: fmt.Sprintf("line %d: %s", line, truncate(value, 60)),
				sql:  value, version: version,
			})

		case "want":
			if len(out) == 0 {
				t.Fatalf("%s:%d: want before any sql", path, line)
			}

			p, err := parseWant(value)
			if err != nil {
				t.Fatalf("%s:%d: %v", path, line, err)
			}

			last := &out[len(out)-1]
			p.Statement = 1
			last.want = append(last.want, p)

		default:
			t.Fatalf("%s:%d: unknown key %q", path, line, key)
		}
	}

	if err := scan.Err(); err != nil {
		t.Fatalf("read the corpus: %v", err)
	}

	if len(out) == 0 {
		t.Fatal("the corpus is empty")
	}

	return out
}

// parseWant reads "<relation> | <LEVEL> | <flags>".
func parseWant(s string) (Prediction, error) {
	fields := strings.Split(s, "|")
	if len(fields) < 2 {
		return Prediction{}, fmt.Errorf("want needs at least a relation and a level, got %q", s)
	}

	relation := strings.TrimSpace(fields[0])
	if relation == "-" {
		relation = ""
	}

	levelText := strings.TrimSpace(fields[1])

	var level Level

	switch levelText {
	case "none":
		level = LevelNone
	case "unknown":
		level = LevelUnknown
	default:
		parsed, ok := ParseLevel(levelText)
		if !ok {
			return Prediction{}, fmt.Errorf("unknown lock level %q", levelText)
		}

		level = parsed
	}

	p := Prediction{Relation: relation, Level: level, Rows: -1}

	if len(fields) > 2 {
		for flag := range strings.FieldsSeq(fields[2]) {
			switch flag {
			case "rewrite":
				p.Rewrites = true
			case "scan":
				p.Scans = true
			default:
				return Prediction{}, fmt.Errorf("unknown flag %q", flag)
			}
		}
	}

	return p, nil
}

// same compares everything a corpus line states, and nothing it does not: the
// reason is prose and is checked for existence elsewhere.
func same(got, want Prediction) bool {
	return got.Relation == want.Relation &&
		got.Level == want.Level &&
		got.Rewrites == want.Rewrites &&
		got.Scans == want.Scans &&
		got.Statement == want.Statement
}

func one(p Prediction) string {
	out := fmt.Sprintf("%s | %s", nonEmpty(p.Relation), p.Level)

	if p.Rewrites {
		out += " rewrite"
	}

	if p.Scans {
		out += " scan"
	}

	return out
}

func format(ps []Prediction) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = one(p)
	}

	return "[" + strings.Join(parts, "; ") + "]"
}

func nonEmpty(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
