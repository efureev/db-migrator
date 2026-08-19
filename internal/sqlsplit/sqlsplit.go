// Package sqlsplit splits a PostgreSQL script into the statements it contains.
//
// # Why this exists
//
// It would be pleasant not to need it. Transactional migrations do not: their
// body goes to the server in one Exec, inside an explicit transaction, and
// PostgreSQL is welcome to parse it however it likes.
//
// The no-transaction path needs it, and needs it for correctness rather than
// tidiness. pgx sends a query with no arguments over the simple protocol, where
// a multi-statement string is legal — and where PostgreSQL wraps the whole
// string in an implicit transaction. So the obvious implementation of
// "-- migrator:no-transaction", which is to skip the BEGIN, does not work:
// CREATE INDEX CONCURRENTLY inside that implicit transaction fails with
// SQLSTATE 25001, exactly as it would have inside an explicit one. The only way
// to run it outside a transaction is to send each statement on its own.
//
// # What it must understand
//
// Everything that can contain a semicolon without ending a statement:
// single-quoted strings with ” doubling, E” escape strings where a backslash
// escapes the next character, double-quoted identifiers with "" doubling,
// line comments, block comments — which nest in PostgreSQL, unlike C — and
// dollar quoting with and without a tag, which is how function bodies are
// written and therefore where the semicolons live.
//
// It is not a parser and does not try to be. It knows where a statement ends
// and nothing else.
package sqlsplit

import (
	"errors"
	"fmt"
	"strings"
)

// Errors reported by [Split].
var (
	// ErrUnterminatedString reports a quote that never closes.
	ErrUnterminatedString = errors.New("unterminated string literal")
	// ErrUnterminatedIdentifier reports a double quote that never closes.
	ErrUnterminatedIdentifier = errors.New("unterminated quoted identifier")
	// ErrUnterminatedComment reports a block comment that never closes.
	ErrUnterminatedComment = errors.New("unterminated block comment")
	// ErrUnterminatedDollar reports a dollar-quoted body that never closes.
	ErrUnterminatedDollar = errors.New("unterminated dollar-quoted string")
)

// A Statement is one statement of a script.
type Statement struct {
	// SQL is the statement text, without its terminating semicolon. It begins
	// at the first character that is neither whitespace nor a comment, so a
	// comment between two statements belongs to neither.
	SQL string
	// Line is the 1-based line of the script on which SQL begins.
	Line int
	// Off is the byte offset of SQL within the script.
	Off int
}

// String reports the statement text.
func (s Statement) String() string { return s.SQL }

// Split reads script and reports its statements in order.
//
// A script holding nothing but whitespace and comments yields no statements and
// no error: an empty migration is a decision for the caller, not a lexing
// failure.
func Split(script string) ([]Statement, error) {
	l := lexer{src: script, line: 1}

	var (
		out   []Statement
		start = -1 // offset of the first significant byte of the statement
		sline int
	)

	for l.pos < len(l.src) {
		// Comments and whitespace are skipped wholesale, so that neither can
		// begin a statement and a semicolon inside them cannot end one.
		skipped, err := l.skipTrivia()
		if err != nil {
			return nil, err
		}

		if skipped {
			continue
		}

		c := l.src[l.pos]

		if c == ';' {
			if start >= 0 {
				out = append(out, Statement{
					SQL:  strings.TrimRight(script[start:l.pos], " \t\r\n"),
					Line: sline,
					Off:  start,
				})
				start = -1
			}

			l.advance(1)

			continue
		}

		if start < 0 {
			start, sline = l.pos, l.line
		}

		if err := l.skipToken(); err != nil {
			return nil, err
		}
	}

	// Whatever follows the last semicolon is a statement too. PostgreSQL does
	// not require a trailing semicolon and neither does this.
	if start >= 0 {
		if sql := strings.TrimRight(script[start:], " \t\r\n"); sql != "" {
			out = append(out, Statement{SQL: sql, Line: sline, Off: start})
		}
	}

	return out, nil
}

// LeadingComments reports the line comments that precede the first SQL token,
// in order and with the leading "--" removed but the rest of the line intact.
//
// Only line comments count, and only before any SQL. A directive found on line
// 300 cannot retroactively change how the first 299 lines were executed, so the
// scan stops at the first thing that is not a comment or blank.
func LeadingComments(script string) []string {
	var out []string

	for rest := script; rest != ""; {
		line, tail, _ := strings.Cut(rest, "\n")
		rest = tail

		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "":
			// A blank line separates comments from each other, not comments
			// from SQL, so scanning continues.
		case strings.HasPrefix(trimmed, "--"):
			out = append(out, strings.TrimPrefix(trimmed, "--"))
		default:
			return out
		}
	}

	return out
}

// LineAt reports the 1-based line of script containing byte offset off.
//
// It exists to turn the position PostgreSQL reports on a failure into a place
// in a file. PostgreSQL counts characters, not bytes, so for a statement
// holding non-ASCII text before the error the answer can be one line off; the
// caller treats the result as a hint, not a fact.
func LineAt(script string, off int) int {
	if off <= 0 {
		return 0
	}

	if off > len(script) {
		off = len(script)
	}

	return 1 + strings.Count(script[:off], "\n")
}

// lexer walks a script tracking only what is needed to find statement
// boundaries: the offset and the line.
type lexer struct {
	src  string
	pos  int
	line int
}

// advance moves n bytes forward, counting newlines.
func (l *lexer) advance(n int) {
	end := min(l.pos+n, len(l.src))

	l.line += strings.Count(l.src[l.pos:end], "\n")
	l.pos = end
}

// skipTrivia consumes one run of whitespace or one comment and reports whether
// it consumed anything.
func (l *lexer) skipTrivia() (bool, error) {
	c := l.src[l.pos]

	// ASCII whitespace only, matching PostgreSQL's own lexer and deliberately
	// not unicode.IsSpace. U+2003 EM SPACE is whitespace to Go and a syntax
	// error to PostgreSQL, so treating it as whitespace here would make a
	// migration containing nothing but one look empty — and an empty migration
	// is recorded as applied. Letting it through produces a loud syntax error
	// from the server instead, which is the outcome worth having.
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v' {
		l.advance(1)

		return true, nil
	}

	if strings.HasPrefix(l.src[l.pos:], "--") {
		if end := strings.IndexByte(l.src[l.pos:], '\n'); end >= 0 {
			l.advance(end + 1)
		} else {
			l.advance(len(l.src) - l.pos)
		}

		return true, nil
	}

	if strings.HasPrefix(l.src[l.pos:], "/*") {
		return true, l.skipBlockComment()
	}

	return false, nil
}

// skipBlockComment consumes a block comment, honouring nesting.
//
// PostgreSQL nests block comments, which is the detail naive splitters miss:
// in "/* a /* b */ c */ SELECT 1;" the comment ends at the second "*/", and a
// splitter that stops at the first one hands "c */ SELECT 1" to the server.
func (l *lexer) skipBlockComment() error {
	start := l.line
	depth := 0

	for l.pos < len(l.src) {
		switch {
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			depth++

			l.advance(2)

		case strings.HasPrefix(l.src[l.pos:], "*/"):
			depth--

			l.advance(2)

			if depth == 0 {
				return nil
			}

		default:
			l.advance(1)
		}
	}

	return fmt.Errorf("line %d: %w", start, ErrUnterminatedComment)
}

// skipToken consumes one significant construct: a quoted string, a quoted
// identifier, a dollar-quoted body, or a single ordinary byte.
func (l *lexer) skipToken() error {
	c := l.src[l.pos]

	switch {
	case c == '\'':
		return l.skipString(false)

	case (c == 'E' || c == 'e') && l.peek(1) == '\'':
		// E'' is an escape string, where a backslash escapes the next
		// character — including a quote, so E'\'' is one quote and not the end
		// of the literal. Plain '' literals treat the backslash as itself,
		// because standard_conforming_strings has been on by default since 9.1.
		l.advance(1)

		return l.skipString(true)

	case (c == 'U' || c == 'u') && l.peek(1) == '&' && l.peek(2) == '\'':
		// U&'' is a Unicode escape string. Its escapes are of the form \0041,
		// and a quote is still doubled, so it lexes as a plain string.
		l.advance(2)

		return l.skipString(false)

	case c == '"':
		return l.skipIdentifier()

	case c == '$':
		if tag, ok := l.dollarTag(); ok {
			return l.skipDollar(tag)
		}

		// $1, $2 — parameter placeholders, and a bare $ is not our problem.
		l.advance(1)

		return nil

	default:
		l.advance(1)

		return nil
	}
}

// peek reports the byte n positions ahead, or 0 past the end.
func (l *lexer) peek(n int) byte {
	if l.pos+n >= len(l.src) {
		return 0
	}

	return l.src[l.pos+n]
}

// skipString consumes a single-quoted literal. In both flavours a doubled quote
// stays inside the literal; in the escape flavour a backslash also escapes.
func (l *lexer) skipString(escapes bool) error {
	start := l.line

	l.advance(1) // opening quote

	for l.pos < len(l.src) {
		switch c := l.src[l.pos]; {
		case escapes && c == '\\' && l.pos+1 < len(l.src):
			l.advance(2)

		case c == '\'':
			if l.peek(1) == '\'' {
				l.advance(2)

				continue
			}

			l.advance(1)

			return nil

		default:
			l.advance(1)
		}
	}

	return fmt.Errorf("line %d: %w", start, ErrUnterminatedString)
}

// skipIdentifier consumes a double-quoted identifier, in which a doubled quote
// stays inside and a backslash never escapes.
func (l *lexer) skipIdentifier() error {
	start := l.line

	l.advance(1)

	for l.pos < len(l.src) {
		if l.src[l.pos] == '"' {
			if l.peek(1) == '"' {
				l.advance(2)

				continue
			}

			l.advance(1)

			return nil
		}

		l.advance(1)
	}

	return fmt.Errorf("line %d: %w", start, ErrUnterminatedIdentifier)
}

// dollarTag reports the dollar-quote opener at the current position, if there
// is one.
//
// A tag may be empty ($$) or an identifier ($body$), and an identifier does not
// start with a digit — which is exactly what keeps $1 a parameter placeholder
// rather than the start of a quoted body.
func (l *lexer) dollarTag() (string, bool) {
	rest := l.src[l.pos:]

	for i := 1; i < len(rest); i++ {
		c := rest[i]

		if c == '$' {
			return rest[:i+1], true
		}

		isFirst := i == 1
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c >= 0x80:
		case (c >= '0' && c <= '9') && !isFirst:
		default:
			return "", false
		}
	}

	return "", false
}

// skipDollar consumes a dollar-quoted body, which ends at the same tag that
// opened it and at nothing else. Function bodies live here, which is why a
// splitter that does not know about dollar quoting cuts them in half at their
// first semicolon.
func (l *lexer) skipDollar(tag string) error {
	start := l.line

	l.advance(len(tag))

	end := strings.Index(l.src[l.pos:], tag)
	if end < 0 {
		return fmt.Errorf("line %d: %w %s", start, ErrUnterminatedDollar, tag)
	}

	l.advance(end + len(tag))

	return nil
}
