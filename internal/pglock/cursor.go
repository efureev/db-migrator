package pglock

import "strings"

// A cursor walks a statement's tokens.
//
// It is deliberately forgiving. Every method that fails to find what it was
// looking for leaves the position alone and reports false, so a statement the
// rules half-understand degrades into a weaker claim rather than a wrong one.
type cursor struct {
	toks []token
	i    int
}

func (c *cursor) eof() bool { return c.i >= len(c.toks) }

// peek reports the token at the cursor, or an end token at the end.
func (c *cursor) peek() token {
	if c.eof() {
		return token{kind: tokEOF}
	}

	return c.toks[c.i]
}

// next reports the token at the cursor and advances past it.
func (c *cursor) next() token {
	t := c.peek()
	c.i++

	return t
}

func (c *cursor) skip(n int) { c.i += n }

// at reports whether the words starting at the cursor are exactly these
// keywords, without moving.
func (c *cursor) at(words ...string) bool {
	for n, w := range words {
		j := c.i + n
		if j >= len(c.toks) || c.toks[j].kind != tokWord || c.toks[j].upper != w {
			return false
		}
	}

	return true
}

// atAny reports whether any of the keyword sequences matches at the cursor.
func (c *cursor) atAny(seqs ...[]string) bool {
	for _, seq := range seqs {
		if c.at(seq...) {
			return true
		}
	}

	return false
}

// optional consumes the keywords if they are there and reports whether it did.
func (c *cursor) optional(words ...string) bool {
	if !c.at(words...) {
		return false
	}

	c.skip(len(words))

	return true
}

// peekPunct reports whether the cursor is on this punctuation.
func (c *cursor) peekPunct(s string) bool {
	t := c.peek()

	return t.kind == tokPunct && t.text == s
}

// optionalPunct consumes the punctuation if it is there.
func (c *cursor) optionalPunct(s string) bool {
	if !c.peekPunct(s) {
		return false
	}

	c.skip(1)

	return true
}

// skipToWord advances past the next occurrence of the keyword and reports
// whether it found one. Parenthesised text is skipped whole, so the ON of a
// partial index predicate cannot be mistaken for the ON of the table.
func (c *cursor) skipToWord(word string) bool {
	for !c.eof() {
		if c.peekPunct("(") {
			c.skipParens()

			continue
		}

		t := c.next()
		if t.kind == tokWord && t.upper == word {
			return true
		}
	}

	return false
}

// skipParens advances past a balanced parenthesised group at the cursor, and
// does nothing when the cursor is not on one.
func (c *cursor) skipParens() {
	if !c.peekPunct("(") {
		return
	}

	depth := 0

	for !c.eof() {
		switch {
		case c.peekPunct("("):
			depth++
		case c.peekPunct(")"):
			depth--
		}

		c.skip(1)

		if depth == 0 {
			return
		}
	}
}

// parenContains reports whether the parenthesised group at the cursor holds
// this keyword, without moving.
func (c *cursor) parenContains(word string) bool {
	depth := 0

	for j := c.i; j < len(c.toks); j++ {
		t := c.toks[j]

		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--

				if depth == 0 {
					return false
				}
			}

			continue
		}

		if t.kind == tokWord && t.upper == word {
			return true
		}
	}

	return false
}

// relation reads a possibly schema-qualified name at the cursor.
//
// It reports "" when the cursor is not on a name, which is what makes an
// unrecognised statement report an unknown relation rather than a wrong one.
func (c *cursor) relation(o Options) string {
	t := c.peek()
	if t.kind != tokWord && t.kind != tokIdent {
		return ""
	}

	parts := []string{nameOf(t)}
	c.skip(1)

	for c.peekPunct(".") {
		c.skip(1)

		nxt := c.peek()
		if nxt.kind != tokWord && nxt.kind != tokIdent {
			break
		}

		parts = append(parts, nameOf(nxt))
		c.skip(1)
	}

	if len(parts) == 1 && o.DefaultSchema != "" {
		return o.DefaultSchema + "." + parts[0]
	}

	return strings.Join(parts, ".")
}

// nameOf reports an identifier's name, folded the way PostgreSQL folds it: an
// unquoted word becomes lower case, a quoted one keeps what it was given.
func nameOf(t token) string {
	if t.kind == tokIdent {
		return t.text
	}

	return strings.ToLower(t.text)
}

// actions splits the rest of the statement on top-level commas, which is how
// ALTER TABLE separates the things it is about to do.
func (c *cursor) actions() []*cursor {
	var (
		out   []*cursor
		start = c.i
		depth int
	)

	flush := func(end int) {
		if end > start {
			out = append(out, &cursor{toks: c.toks[start:end]})
		}
	}

	for j := c.i; j < len(c.toks); j++ {
		t := c.toks[j]
		if t.kind != tokPunct {
			continue
		}

		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				flush(j)
				start = j + 1
			}
		}
	}

	flush(len(c.toks))

	return out
}

// hasWords reports whether the keywords appear consecutively anywhere in the
// cursor's remaining tokens.
func (c *cursor) hasWords(words ...string) bool {
	probe := cursor{toks: c.toks, i: c.i}

	for ; probe.i < len(probe.toks); probe.i++ {
		if probe.at(words...) {
			return true
		}
	}

	return false
}
