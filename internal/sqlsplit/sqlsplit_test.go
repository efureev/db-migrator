package sqlsplit

import (
	"errors"
	"strings"
	"testing"
)

func sqls(stmts []Statement) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.SQL
	}

	return out
}

func TestSplit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{
			name:   "empty",
			script: "",
			want:   nil,
		},
		{
			name:   "whitespace only",
			script: "  \n\t\n ",
			want:   nil,
		},
		{
			name:   "comments only",
			script: "-- migrator:no-transaction\n-- nothing to do\n",
			want:   nil,
		},
		{
			name:   "single statement without a terminator",
			script: "SELECT 1",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "single statement with a terminator",
			script: "SELECT 1;",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "two statements",
			script: "CREATE TABLE a (id int);\nDROP TABLE a;",
			want:   []string{"CREATE TABLE a (id int)", "DROP TABLE a"},
		},
		{
			name:   "empty statements collapse",
			script: ";;; SELECT 1 ;;;",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "semicolon inside a string",
			script: "INSERT INTO t VALUES ('a;b');",
			want:   []string{"INSERT INTO t VALUES ('a;b')"},
		},
		{
			name:   "doubled quote inside a string",
			script: "INSERT INTO t VALUES ('it''s; fine');",
			want:   []string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			name:   "escape string with a backslash-escaped quote",
			script: `INSERT INTO t VALUES (E'it\'s; fine');`,
			want:   []string{`INSERT INTO t VALUES (E'it\'s; fine')`},
		},
		{
			name:   "lower case e escape string",
			script: `SELECT e'a\';b';`,
			want:   []string{`SELECT e'a\';b'`},
		},
		{
			name:   "backslash is literal in a standard string",
			script: `SELECT 'a\', 'b;c';`,
			want:   []string{`SELECT 'a\', 'b;c'`},
		},
		{
			name:   "unicode escape string",
			script: `SELECT U&'d\0061t;a';`,
			want:   []string{`SELECT U&'d\0061t;a'`},
		},
		{
			name:   "semicolon inside a quoted identifier",
			script: `CREATE TABLE "a;b" (id int);`,
			want:   []string{`CREATE TABLE "a;b" (id int)`},
		},
		{
			name:   "doubled quote inside an identifier",
			script: `CREATE TABLE "a""b;c" (id int);`,
			want:   []string{`CREATE TABLE "a""b;c" (id int)`},
		},
		{
			name:   "semicolon inside a line comment",
			script: "SELECT 1 -- ; not a boundary\n;SELECT 2;",
			want:   []string{"SELECT 1 -- ; not a boundary", "SELECT 2"},
		},
		{
			name:   "semicolon inside a block comment",
			script: "SELECT 1 /* ; still not */ + 2;",
			want:   []string{"SELECT 1 /* ; still not */ + 2"},
		},
		{
			name:   "nested block comment",
			script: "/* a /* b ; */ c ; */ SELECT 1;",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "deeply nested block comment",
			script: "/* /* /* ; */ */ */ SELECT 1;",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "dollar quoted body without a tag",
			script: "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql;",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql",
			},
		},
		{
			name:   "dollar quoted body with a tag",
			script: "CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql;",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql",
			},
		},
		{
			name:   "inner dollar tag does not close an outer one",
			script: "SELECT $outer$ a $inner$ b ; $outer$;",
			want:   []string{"SELECT $outer$ a $inner$ b ; $outer$"},
		},
		{
			name:   "parameter placeholder is not a dollar quote",
			script: "SELECT $1; SELECT $2;",
			want:   []string{"SELECT $1", "SELECT $2"},
		},
		{
			name:   "leading comments do not join the statement",
			script: "-- migrator:no-transaction\n\nCREATE INDEX CONCURRENTLY i ON t (c);",
			want:   []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
		},
		{
			name:   "comment between statements belongs to neither",
			script: "SELECT 1;\n-- why\nSELECT 2;",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "trailing comment after the last semicolon",
			script: "SELECT 1;\n-- done\n",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "dollar sign alone",
			script: "SELECT 1 $ ;",
			want:   []string{"SELECT 1 $"},
		},
		{
			// PostgreSQL's lexer treats only ASCII whitespace as whitespace,
			// so a stray EM SPACE is a token and reaches the server as the
			// syntax error it is, rather than making the migration look empty.
			name:   "unicode whitespace is a token, not whitespace",
			script: "\u2003",
			want:   []string{"\u2003"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Split(tc.script)
			if err != nil {
				t.Fatalf("Split(%q) unexpected error: %v", tc.script, err)
			}

			gotSQL := sqls(got)
			if len(gotSQL) != len(tc.want) {
				t.Fatalf("Split(%q) = %q, want %q", tc.script, gotSQL, tc.want)
			}

			for i := range gotSQL {
				if gotSQL[i] != tc.want[i] {
					t.Errorf("statement %d = %q, want %q", i, gotSQL[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
		want   error
	}{
		{"unterminated string", "SELECT 'a", ErrUnterminatedString},
		{"unterminated escape string", `SELECT E'a\'`, ErrUnterminatedString},
		{"unterminated identifier", `SELECT "a`, ErrUnterminatedIdentifier},
		{"unterminated block comment", "/* a", ErrUnterminatedComment},
		{"unterminated nested block comment", "/* a /* b */", ErrUnterminatedComment},
		{"unterminated dollar quote", "SELECT $$ a", ErrUnterminatedDollar},
		{"unterminated tagged dollar quote", "SELECT $t$ a $u$", ErrUnterminatedDollar},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Split(tc.script)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Split(%q) error = %v, want %v", tc.script, err, tc.want)
			}

			// The line must be in the message: a 400-line migration with an
			// unclosed quote is not findable from the sentinel alone.
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("error %q does not name a line", err)
			}
		})
	}
}

func TestSplitPositions(t *testing.T) {
	t.Parallel()

	script := "-- header\n\nCREATE TABLE a (id int);\n\n/* note\n   spanning lines */\nDROP TABLE a;\n"

	got, err := Split(script)
	if err != nil {
		t.Fatal(err)
	}

	want := []Statement{
		{SQL: "CREATE TABLE a (id int)", Line: 3},
		{SQL: "DROP TABLE a", Line: 7},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d statements, want %d: %q", len(got), len(want), sqls(got))
	}

	for i := range want {
		if got[i].SQL != want[i].SQL || got[i].Line != want[i].Line {
			t.Errorf("statement %d = {%q, line %d}, want {%q, line %d}",
				i, got[i].SQL, got[i].Line, want[i].SQL, want[i].Line)
		}

		if script[got[i].Off:got[i].Off+len(got[i].SQL)] != got[i].SQL {
			t.Errorf("statement %d: Off %d does not point at its own text", i, got[i].Off)
		}
	}
}

func TestLeadingComments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{
			name:   "none",
			script: "SELECT 1;",
		},
		{
			name:   "one directive",
			script: "-- migrator:no-transaction\nCREATE INDEX CONCURRENTLY i ON t (c);",
			want:   []string{" migrator:no-transaction"},
		},
		{
			name:   "several, blank lines between",
			script: "-- migrator:no-transaction\n\n--  migrator:tags ddl\n\nSELECT 1;",
			want:   []string{" migrator:no-transaction", "  migrator:tags ddl"},
		},
		{
			name:   "stops at the first SQL token",
			script: "-- a\nSELECT 1;\n-- migrator:no-transaction\nSELECT 2;",
			want:   []string{" a"},
		},
		{
			name:   "a block comment is not a directive line",
			script: "/* migrator:no-transaction */\nSELECT 1;",
		},
		{
			name:   "indented comment still counts",
			script: "   -- migrator:retry-safe\nSELECT 1;",
			want:   []string{" migrator:retry-safe"},
		},
		{
			name:   "comment without a trailing newline",
			script: "-- migrator:no-transaction",
			want:   []string{" migrator:no-transaction"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := LeadingComments(tc.script)
			if len(got) != len(tc.want) {
				t.Fatalf("LeadingComments(%q) = %q, want %q", tc.script, got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("comment %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLineAt(t *testing.T) {
	t.Parallel()

	// Offsets:      0123 4567 8
	script := "one\ntwo\nthree"

	for _, tc := range []struct{ off, want int }{
		{0, 0},    // no position reported at all
		{1, 1},    // "n" of one
		{3, 1},    // the newline that ends line 1 belongs to line 1
		{4, 2},    // "t" of two
		{7, 2},    // the newline that ends line 2
		{8, 3},    // "t" of three
		{13, 3},   // one past the end
		{1000, 3}, // clamped
	} {
		if got := LineAt(script, tc.off); got != tc.want {
			t.Errorf("LineAt(_, %d) = %d, want %d", tc.off, got, tc.want)
		}
	}
}

// FuzzSplit asserts the two properties the no-transaction path depends on:
// splitting never loses SQL, and every reported offset really points at the
// reported text. A splitter that silently drops a statement is worse than one
// that fails, because the migration is then recorded as applied.
func FuzzSplit(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1;", "", ";;", "SELECT 'a;b';", `SELECT "a;b";`,
		"/* a /* b */ c */ SELECT 1;", "SELECT $$ a; $$;", "SELECT $t$ a; $t$;",
		`SELECT E'a\';b';`, "-- c\nSELECT 1;", "SELECT $1;", "SELECT $$",
		"CREATE FUNCTION f() AS $$ BEGIN; END; $$ LANGUAGE plpgsql;",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, script string) {
		got, err := Split(script)
		if err != nil {
			return
		}

		prev := -1
		for i, s := range got {
			if s.Off < 0 || s.Off+len(s.SQL) > len(script) {
				t.Fatalf("statement %d: offset %d, length %d, script length %d",
					i, s.Off, len(s.SQL), len(script))
			}

			if script[s.Off:s.Off+len(s.SQL)] != s.SQL {
				t.Fatalf("statement %d: text at offset %d is not the reported text", i, s.Off)
			}

			if s.Off <= prev {
				t.Fatalf("statement %d: offset %d does not advance past %d", i, s.Off, prev)
			}

			prev = s.Off

			// The lexer's whitespace is PostgreSQL's, which is ASCII;
			// strings.TrimSpace would call U+2003 blank and PostgreSQL calls
			// it a syntax error. See skipTrivia.
			if strings.Trim(s.SQL, " \t\r\n\f\v") == "" {
				t.Fatalf("statement %d is blank", i)
			}

			if s.Line != LineAt(script, s.Off+1) {
				t.Fatalf("statement %d: Line %d disagrees with LineAt %d",
					i, s.Line, LineAt(script, s.Off+1))
			}

			// Re-splitting one statement must yield that statement and nothing
			// else. This is what makes it safe to send them one at a time.
			again, err := Split(s.SQL)
			if err != nil {
				t.Fatalf("statement %d does not re-split: %v (%q)", i, err, s.SQL)
			}

			if len(again) != 1 || again[0].SQL != s.SQL {
				t.Fatalf("statement %d re-split into %q", i, sqls(again))
			}
		}
	})
}
