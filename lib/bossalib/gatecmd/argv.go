package gatecmd

import (
	"slices"
	"strings"
)

// directExec reports whether Run executes cmd (already trimmed) directly,
// splitting it on whitespace, rather than through `sh -c`. Argv shares this
// predicate with buildCommand so the comparator cannot drift from execution.
func directExec(cmd string) bool {
	return strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../")
}

// Argv is the argument vector Run would execute for cmd, with $HOME, ${HOME}
// and a leading ~ resolved against home. A direct-exec command (one starting
// with /, ./ or ../) is split on whitespace with quotes kept literal and nothing
// expanded, exactly as Run runs it. Any other command is parsed as POSIX shell
// words, modelling only quoting, backslash escapes and the two HOME spellings.
//
// ok is false for anything that model does not cover — operators, globs,
// comments, other expansions, an env-assignment or reserved-word first word, a
// newline, an unterminated quote, an expansion with an empty home, or an empty
// result — so a caller falls back rather than guessing what sh would run.
func Argv(cmd, home string) ([]string, bool) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil, false
	}
	if directExec(cmd) {
		return strings.Fields(cmd), true
	}
	return shellWords(cmd, home)
}

// Equivalent reports whether a and b are the same command as Run would
// execute it, on a host whose home directory is home. It holds when the
// trimmed strings are equal or both parse to the same Argv. When either side
// is beyond the Argv model, it also holds when both are equal after replacing
// $HOME and ${HOME} with home and neither is direct-exec: the comparison
// callers used before Argv existed, kept so no such pair stops comparing equal.
func Equivalent(a, b, home string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	argvA, okA := Argv(a, home)
	argvB, okB := Argv(b, home)
	if okA && okB {
		return slices.Equal(argvA, argvB)
	}
	// A direct-exec command passes $HOME through literally, so the string
	// rule would equate commands Run executes differently.
	if home == "" || directExec(a) || directExec(b) {
		return false
	}
	expand := strings.NewReplacer("${HOME}", home, "$HOME", home)
	return expand.Replace(a) == expand.Replace(b)
}

// shellUnmodelled are the unquoted bytes whose shell meaning Argv does not
// model: operators, grouping, substitution, globs, braces and negation.
const shellUnmodelled = ";&|<>()`*?[{}!"

// shellReserved are first words sh treats as syntax rather than a command.
var shellReserved = []string{"!", "{", "}", "[[", "]]", "if", "then", "else", "elif", "fi", "do", "done", "case", "esac", "while", "until", "for", "in", "function", "select", "time"}

// shellWords lexes cmd as POSIX shell words. See Argv for what it models.
func shellWords(cmd, home string) ([]string, bool) {
	for i := 0; i < len(cmd); i++ {
		if c := cmd[i]; c < 0x20 && c != '\t' {
			return nil, false
		}
	}
	var (
		words  []string
		cur    strings.Builder
		inWord bool
	)
	expandHome := func(rest string) (int, bool) {
		n := homeRef(rest)
		if n == 0 || home == "" {
			return 0, false
		}
		cur.WriteString(home)
		return n, true
	}
	for i := 0; i < len(cmd); {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case c == '\'':
			end := strings.IndexByte(cmd[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case c == '"':
			inWord = true
			i++
			for closed := false; !closed; {
				if i >= len(cmd) {
					return nil, false
				}
				switch d := cmd[i]; d {
				case '"':
					closed = true
					i++
				case '\\':
					if i+1 < len(cmd) && strings.IndexByte("$\"\\`", cmd[i+1]) >= 0 {
						cur.WriteByte(cmd[i+1])
						i += 2
					} else {
						cur.WriteByte('\\')
						i++
					}
				case '`':
					return nil, false
				case '$':
					n, ok := expandHome(cmd[i:])
					if !ok {
						return nil, false
					}
					i += n
				default:
					cur.WriteByte(d)
					i++
				}
			}
		case c == '\\':
			if i+1 >= len(cmd) {
				return nil, false
			}
			cur.WriteByte(cmd[i+1])
			inWord = true
			i += 2
		case c == '$':
			// sh field-splits and globs an unquoted expansion; this model
			// does neither, so a home it would split or glob is unmodelled.
			if strings.ContainsAny(home, " \t\n*?[") {
				return nil, false
			}
			n, ok := expandHome(cmd[i:])
			if !ok {
				return nil, false
			}
			inWord = true
			i += n
		case c == '#' && !inWord:
			return nil, false
		case c == '~' && !inWord:
			if i+1 < len(cmd) && cmd[i+1] != '/' && cmd[i+1] != ' ' && cmd[i+1] != '\t' {
				return nil, false // ~user
			}
			if home == "" {
				return nil, false
			}
			cur.WriteString(home)
			inWord = true
			i++
		case strings.IndexByte(shellUnmodelled, c) >= 0:
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 || strings.Contains(words[0], "=") || slices.Contains(shellReserved, words[0]) {
		return nil, false
	}
	return words, true
}

// homeRef is the length of the $HOME or ${HOME} reference at the start of s,
// or 0 when s starts with any other expansion.
func homeRef(s string) int {
	switch {
	case strings.HasPrefix(s, "${HOME}"):
		return len("${HOME}")
	case strings.HasPrefix(s, "$HOME"):
		if rest := s[len("$HOME"):]; rest != "" && isNameByte(rest[0]) {
			return 0
		}
		return len("$HOME")
	}
	return 0
}

func isNameByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
