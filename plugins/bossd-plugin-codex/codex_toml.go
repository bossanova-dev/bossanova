package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tomlStatementKind classifies one top-level statement of a TOML document.
type tomlStatementKind int

const (
	tomlStmtTable tomlStatementKind = iota + 1
	tomlStmtArrayTable
	tomlStmtKeyValue
)

// tomlStatement is one top-level statement of a TOML document, located by
// byte offsets into the source so an edit can rewrite exactly one value or
// insert after one line and leave every other byte as it was.
type tomlStatement struct {
	kind tomlStatementKind
	// path is the decoded key path: the table path for a header, and the FULL
	// path (enclosing table + dotted key) for a key/value pair. Decoding makes
	// every equivalent spelling compare equal: bare, "basic" (escapes
	// resolved) and 'literal' keys, with any whitespace around the dots.
	path []string
	// valueStart and valueEnd bound a key/value pair's value.
	valueStart, valueEnd int
	// lineEnd is the offset just past the statement's final newline (or the
	// end of the document when the statement ends without one).
	lineEnd int
}

// errTOMLSyntax marks a document the scanner cannot read. A trust edit then
// fails closed rather than guess where a table starts or ends.
var errTOMLSyntax = errors.New("toml syntax")

// parseTOMLStatements splits doc into its top-level statements. It decodes
// keys fully but only skips over values — strings (including multi-line
// ones), arrays and inline tables are walked just far enough to find where
// they end, so a bracket or newline inside a value is never read as
// structure. Anything it cannot read is an error.
func parseTOMLStatements(doc string) ([]tomlStatement, error) {
	sc := &tomlScanner{s: doc}
	if strings.HasPrefix(doc, "\uFEFF") {
		sc.i = len("\uFEFF")
	}
	var (
		stmts []tomlStatement
		table []string
	)
	for {
		sc.skipBlank()
		if sc.done() {
			return stmts, nil
		}
		var st tomlStatement
		if sc.peek() == '[' {
			st.kind = tomlStmtTable
			sc.i++
			if sc.peek() == '[' {
				st.kind = tomlStmtArrayTable
				sc.i++
			}
			path, err := sc.parseKey()
			if err != nil {
				return nil, err
			}
			sc.skipSpace()
			closing := "]"
			if st.kind == tomlStmtArrayTable {
				closing = "]]"
			}
			if !strings.HasPrefix(sc.s[sc.i:], closing) {
				return nil, sc.errorf("unterminated table header")
			}
			sc.i += len(closing)
			st.path = path
			table = path
		} else {
			key, err := sc.parseKey()
			if err != nil {
				return nil, err
			}
			sc.skipSpace()
			if sc.peek() != '=' {
				return nil, sc.errorf("expected '=' after key")
			}
			sc.i++
			sc.skipSpace()
			st.kind = tomlStmtKeyValue
			st.valueStart = sc.i
			if err := sc.skipValue(); err != nil {
				return nil, err
			}
			st.valueEnd = sc.i
			st.path = append(append([]string{}, table...), key...)
		}
		if err := sc.endLine(); err != nil {
			return nil, err
		}
		st.lineEnd = sc.i
		stmts = append(stmts, st)
	}
}

type tomlScanner struct {
	s string
	i int
}

func (sc *tomlScanner) done() bool { return sc.i >= len(sc.s) }

func (sc *tomlScanner) peek() byte {
	if sc.done() {
		return 0
	}
	return sc.s[sc.i]
}

func (sc *tomlScanner) errorf(format string, args ...any) error {
	line := 1 + strings.Count(sc.s[:min(sc.i, len(sc.s))], "\n")
	return fmt.Errorf("%w: line %d: %s", errTOMLSyntax, line, fmt.Sprintf(format, args...))
}

// skipSpace skips spaces and tabs.
func (sc *tomlScanner) skipSpace() {
	for !sc.done() && (sc.s[sc.i] == ' ' || sc.s[sc.i] == '\t') {
		sc.i++
	}
}

// skipComment skips a '#' comment up to (not including) its newline.
func (sc *tomlScanner) skipComment() {
	if sc.peek() != '#' {
		return
	}
	for !sc.done() && sc.s[sc.i] != '\n' {
		sc.i++
	}
}

// skipBlank skips whitespace, newlines and comments.
func (sc *tomlScanner) skipBlank() {
	for {
		sc.skipSpace()
		sc.skipComment()
		switch {
		case strings.HasPrefix(sc.s[sc.i:], "\r\n"):
			sc.i += 2
		case sc.peek() == '\n':
			sc.i++
		default:
			return
		}
	}
}

// endLine requires the rest of the line to be blank or a comment and moves
// past its newline.
func (sc *tomlScanner) endLine() error {
	sc.skipSpace()
	sc.skipComment()
	switch {
	case sc.done():
		return nil
	case strings.HasPrefix(sc.s[sc.i:], "\r\n"):
		sc.i += 2
		return nil
	case sc.s[sc.i] == '\n':
		sc.i++
		return nil
	}
	return sc.errorf("unexpected %q after statement", sc.s[sc.i])
}

// parseKey reads a dotted key — simple keys joined by '.', whitespace allowed
// around each dot — and returns its decoded segments.
func (sc *tomlScanner) parseKey() ([]string, error) {
	var path []string
	for {
		sc.skipSpace()
		seg, err := sc.parseSimpleKey()
		if err != nil {
			return nil, err
		}
		path = append(path, seg)
		sc.skipSpace()
		if sc.peek() != '.' {
			return path, nil
		}
		sc.i++
	}
}

func (sc *tomlScanner) parseSimpleKey() (string, error) {
	switch sc.peek() {
	case '"':
		if strings.HasPrefix(sc.s[sc.i:], `"""`) {
			return "", sc.errorf("multi-line string used as a key")
		}
		return sc.parseBasicString()
	case '\'':
		if strings.HasPrefix(sc.s[sc.i:], `'''`) {
			return "", sc.errorf("multi-line string used as a key")
		}
		start := sc.i + 1
		end := strings.IndexAny(sc.s[start:], "'\n")
		if end < 0 || sc.s[start+end] != '\'' {
			return "", sc.errorf("unterminated literal key")
		}
		sc.i = start + end + 1
		return sc.s[start : start+end], nil
	}
	start := sc.i
	for !sc.done() && isBareKeyByte(sc.s[sc.i]) {
		sc.i++
	}
	if sc.i == start {
		return "", sc.errorf("expected a key")
	}
	return sc.s[start:sc.i], nil
}

func isBareKeyByte(c byte) bool {
	return c == '_' || c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// parseBasicString reads a single-line "basic" string and returns it with its
// escapes resolved.
func (sc *tomlScanner) parseBasicString() (string, error) {
	sc.i++ // opening quote
	var b strings.Builder
	for {
		if sc.done() || sc.s[sc.i] == '\n' {
			return "", sc.errorf("unterminated string")
		}
		c := sc.s[sc.i]
		switch c {
		case '"':
			sc.i++
			return b.String(), nil
		case '\\':
			if err := sc.decodeEscape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			sc.i++
		}
	}
}

// decodeEscape decodes the escape sequence at sc.i (a backslash) into b.
func (sc *tomlScanner) decodeEscape(b *strings.Builder) error {
	if sc.i+1 >= len(sc.s) {
		return sc.errorf("unterminated escape")
	}
	c := sc.s[sc.i+1]
	sc.i += 2
	simple := map[byte]byte{'b': '\b', 't': '\t', 'n': '\n', 'f': '\f', 'r': '\r', '"': '"', '\\': '\\', 'e': 0x1b}
	if r, ok := simple[c]; ok {
		b.WriteByte(r)
		return nil
	}
	digits := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
	if digits == 0 || sc.i+digits > len(sc.s) {
		return sc.errorf("invalid escape \\%c", c)
	}
	code, err := strconv.ParseUint(sc.s[sc.i:sc.i+digits], 16, 32)
	if err != nil || !utf8.ValidRune(rune(code)) {
		return sc.errorf("invalid escape \\%c%s", c, sc.s[sc.i:sc.i+digits])
	}
	sc.i += digits
	b.WriteRune(rune(code))
	return nil
}

// skipValue moves past one value. Strings, arrays and inline tables are
// walked to their real end; any other value (number, bool, date-time) runs to
// the next delimiter, with trailing whitespace left for endLine.
func (sc *tomlScanner) skipValue() error {
	rest := sc.s[sc.i:]
	switch {
	case strings.HasPrefix(rest, `"""`):
		return sc.skipMultiline(`"""`, true)
	case strings.HasPrefix(rest, `'''`):
		return sc.skipMultiline(`'''`, false)
	case strings.HasPrefix(rest, `"`):
		_, err := sc.parseBasicString()
		return err
	case strings.HasPrefix(rest, `'`):
		end := strings.IndexAny(rest[1:], "'\n")
		if end < 0 || rest[1+end] != '\'' {
			return sc.errorf("unterminated literal string")
		}
		sc.i += end + 2
		return nil
	case strings.HasPrefix(rest, "["):
		return sc.skipArray()
	case strings.HasPrefix(rest, "{"):
		return sc.skipInlineTable()
	}
	start := sc.i
	for !sc.done() && !strings.ContainsRune("\r\n#,]}", rune(sc.s[sc.i])) {
		sc.i++
	}
	for sc.i > start && (sc.s[sc.i-1] == ' ' || sc.s[sc.i-1] == '\t') {
		sc.i--
	}
	if sc.i == start {
		return sc.errorf("expected a value")
	}
	return nil
}

func (sc *tomlScanner) skipMultiline(delim string, escapes bool) error {
	sc.i += len(delim)
	for !sc.done() {
		if escapes && sc.s[sc.i] == '\\' {
			sc.i += 2
			continue
		}
		if strings.HasPrefix(sc.s[sc.i:], delim) {
			sc.i += len(delim)
			// Up to two more quotes may close the string's content.
			for k := 0; k < 2 && sc.peek() == delim[0]; k++ {
				sc.i++
			}
			return nil
		}
		sc.i++
	}
	return sc.errorf("unterminated multi-line string")
}

func (sc *tomlScanner) skipArray() error {
	sc.i++ // '['
	for {
		sc.skipBlank()
		if sc.peek() == ']' {
			sc.i++
			return nil
		}
		if sc.done() {
			return sc.errorf("unterminated array")
		}
		if err := sc.skipValue(); err != nil {
			return err
		}
		sc.skipBlank()
		switch sc.peek() {
		case ',':
			sc.i++
		case ']':
			sc.i++
			return nil
		default:
			return sc.errorf("expected ',' or ']' in array")
		}
	}
}

// skipInlineTable moves past an inline table. Newlines and comments inside
// it are accepted (TOML 1.1) so a config Codex reads is not refused here.
func (sc *tomlScanner) skipInlineTable() error {
	sc.i++ // '{'
	for {
		sc.skipBlank()
		if sc.peek() == '}' {
			sc.i++
			return nil
		}
		if sc.done() {
			return sc.errorf("unterminated inline table")
		}
		if _, err := sc.parseKey(); err != nil {
			return err
		}
		sc.skipSpace()
		if sc.peek() != '=' {
			return sc.errorf("expected '=' in inline table")
		}
		sc.i++
		sc.skipSpace()
		if err := sc.skipValue(); err != nil {
			return err
		}
		sc.skipBlank()
		switch sc.peek() {
		case ',':
			sc.i++
		case '}':
			sc.i++
			return nil
		default:
			return sc.errorf("expected ',' or '}' in inline table")
		}
	}
}

// tomlPathHasPrefix reports whether path starts with prefix.
func tomlPathHasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

func tomlPathEqual(a, b []string) bool {
	return len(a) == len(b) && tomlPathHasPrefix(a, b)
}
