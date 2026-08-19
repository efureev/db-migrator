package pglock

import "strings"

// A tokKind is the kind of one lexical token.
type tokKind uint8

const (
	tokEOF tokKind = iota
	// tokWord is an unquoted identifier or keyword. PostgreSQL folds those to
	// lower case, so the two cannot be told apart lexically and the parser
	// decides by position.
	tokWord
	// tokIdent is a double-quoted identifier, whose case is significant.
	tokIdent
	// tokString is a literal: '…', E'…', $tag$…$tag$.
	tokString
	tokNumber
	tokPunct
)

// A token is one lexical token of a statement.
type token struct {
	kind tokKind
	// text is the token as written, with quotes removed for tokIdent and
	// tokString.
	text string
	// upper is text folded to upper case, and is empty for anything but
	// tokWord. Keywords are matched against it so that no comparison in the
	// rule table has to remember to fold.
	upper string
}

// tokenize reads sql into tokens, discarding comments and whitespace.
//
// It never fails. A statement this lexer cannot make sense of yields tokens
// that the rules do not match, and an unmatched statement is reported as
// unknown — which is the honest answer and the safe one. Refusing to run a
// migration because a predictor could not parse it would be the tail wagging
// the dog.
func tokenize(sql string) []token {
	var out []token

	for i := 0; i < len(sql); {
		c := sql[i]

		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++

		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i)

		case c == '"':
			text, next := readQuoted(sql, i, '"')
			out = append(out, token{kind: tokIdent, text: text})
			i = next

		case c == '\'':
			_, next := readQuoted(sql, i, '\'')
			out = append(out, token{kind: tokString, text: sql[i:next]})
			i = next

		case (c == 'e' || c == 'E') && i+1 < len(sql) && sql[i+1] == '\'':
			_, next := readQuoted(sql, i+1, '\'')
			out = append(out, token{kind: tokString, text: sql[i:next]})
			i = next

		case c == '$':
			if tag, body, next, ok := readDollar(sql, i); ok {
				_ = tag
				out = append(out, token{kind: tokString, text: body})
				i = next

				continue
			}

			out = append(out, token{kind: tokPunct, text: "$"})
			i++

		case isDigit(c):
			start := i
			for i < len(sql) && (isDigit(sql[i]) || sql[i] == '.') {
				i++
			}

			out = append(out, token{kind: tokNumber, text: sql[start:i]})

		case isWordStart(c):
			start := i
			for i < len(sql) && isWordPart(sql[i]) {
				i++
			}

			word := sql[start:i]
			out = append(out, token{kind: tokWord, text: word, upper: strings.ToUpper(word)})

		default:
			out = append(out, token{kind: tokPunct, text: string(c)})
			i++
		}
	}

	return out
}

// skipBlockComment reports the offset just past the comment starting at i.
// PostgreSQL nests block comments, which most naive splitters get wrong.
func skipBlockComment(sql string, i int) int {
	depth := 0

	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2

			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}

	return i
}

// readQuoted reads a quoted run starting at the quote at i, honouring the
// doubled-quote escape, and reports the unquoted text and the offset past it.
func readQuoted(sql string, i int, quote byte) (text string, next int) {
	var b strings.Builder

	i++ // the opening quote

	for i < len(sql) {
		if sql[i] == quote {
			if i+1 < len(sql) && sql[i+1] == quote {
				b.WriteByte(quote)
				i += 2

				continue
			}

			return b.String(), i + 1
		}

		if quote == '\'' && sql[i] == '\\' && i+1 < len(sql) {
			b.WriteByte(sql[i])
			b.WriteByte(sql[i+1])
			i += 2

			continue
		}

		b.WriteByte(sql[i])
		i++
	}

	return b.String(), i
}

// readDollar reads a dollar-quoted literal starting at i.
func readDollar(sql string, i int) (tag, body string, next int, ok bool) {
	end := strings.IndexByte(sql[i+1:], '$')
	if end < 0 {
		return "", "", i, false
	}

	tag = sql[i : i+end+2]

	for j := 1; j < len(tag)-1; j++ {
		if !isWordPart(tag[j]) {
			return "", "", i, false
		}
	}

	rest := i + len(tag)

	closing := strings.Index(sql[rest:], tag)
	if closing < 0 {
		return tag, sql[rest:], len(sql), true
	}

	return tag, sql[rest : rest+closing], rest + closing + len(tag), true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordPart(c byte) bool { return isWordStart(c) || isDigit(c) || c == '$' }
