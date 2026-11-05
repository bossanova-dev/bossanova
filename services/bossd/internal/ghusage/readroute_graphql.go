package ghusage

import (
	"strconv"
	"strings"
	"unicode"
)

type readQueryToken struct {
	text    string
	literal bool
}

// tokenizeReadQuery retains string values but prevents their contents from becoming syntax.
func tokenizeReadQuery(query string) ([]readQueryToken, bool) {
	var tokens []readQueryToken
	for i := 0; i < len(query); {
		c := query[i]
		if c == '#' {
			for i < len(query) && query[i] != '\n' {
				i++
			}
			continue
		}
		if c == ',' || unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		if c == '"' {
			value, next, ok := readQueryString(query, i)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, readQueryToken{text: value, literal: true})
			i = next
			continue
		}
		if readQueryNameByte(c) {
			start := i
			i++
			for i < len(query) && readQueryNameByte(query[i]) {
				i++
			}
			tokens = append(tokens, readQueryToken{text: query[start:i]})
			continue
		}
		if !strings.ContainsRune("{}():!$[]=@.-", rune(c)) {
			return nil, false
		}
		tokens = append(tokens, readQueryToken{text: string(c)})
		i++
	}
	return tokens, true
}

type readQueryScanner struct {
	tokens []readQueryToken
	pos    int
}

func (s *readQueryScanner) peek() string {
	if s.pos >= len(s.tokens) || s.tokens[s.pos].literal {
		return ""
	}
	return s.tokens[s.pos].text
}

func (s *readQueryScanner) take(text string) bool {
	if s.peek() != text {
		return false
	}
	s.pos++
	return true
}

// skipGroup checks nested delimiter balance without interpreting child fields.
func (s *readQueryScanner) skipGroup() bool {
	end := map[string]string{"{": "}", "(": ")", "[": "]"}[s.peek()]
	if end == "" {
		return false
	}
	s.pos++
	for s.pos < len(s.tokens) {
		if s.take(end) {
			return true
		}
		switch s.peek() {
		case "{", "(", "[":
			if !s.skipGroup() {
				return false
			}
		case "}", ")", "]":
			return false
		default:
			s.pos++
		}
	}
	return false
}

func classifyGraphQLRead(fields map[string]string, env ReadEnv) ReadRoute {
	tokens, ok := tokenizeReadQuery(fields["query"])
	if !ok || len(tokens) == 0 {
		return readRefusal("not_single_repo")
	}
	s := readQueryScanner{tokens: tokens}
	if s.peek() == "mutation" || s.peek() == "subscription" {
		return readRefusal("not_read")
	}
	if s.take("query") {
		if s.peek() != "(" && s.peek() != "{" {
			s.pos++
		}
		if s.peek() == "(" && !s.skipGroup() {
			return readRefusal("not_single_repo")
		}
	}
	if !s.take("{") {
		return readRefusal("not_single_repo")
	}
	repo, seen := "", false
	for s.pos < len(s.tokens) && s.peek() != "}" {
		target, ok := s.repository(fields, env)
		if !ok || seen && target != repo {
			return readRefusal("not_single_repo")
		}
		repo, seen = target, true
	}
	if !seen || !s.take("}") || s.pos != len(tokens) {
		return readRefusal("not_single_repo")
	}
	return ReadRoute{Routable: true, Repo: repo, Reason: "installation"}
}

func (s *readQueryScanner) repository(fields map[string]string, env ReadEnv) (string, bool) {
	name := s.peek()
	s.pos++
	if s.take(":") {
		name = s.peek()
		s.pos++
	}
	if name != "repository" || !s.take("(") {
		return "", false
	}
	values := map[string]string{}
	for s.pos < len(s.tokens) && s.peek() != ")" {
		key := s.peek()
		s.pos++
		if (key != "owner" && key != "name") || !s.take(":") {
			return "", false
		}
		if _, exists := values[key]; exists {
			return "", false
		}
		if s.take("$") {
			variable := s.peek()
			s.pos++
			value, exists := fields[variable]
			// Typed fields beginning with @ read local files; do not guess their contents.
			if !exists || strings.HasPrefix(value, "@") {
				return "", false
			}
			values[key] = value
		} else {
			if s.pos >= len(s.tokens) || !s.tokens[s.pos].literal {
				return "", false
			}
			values[key] = s.tokens[s.pos].text
			s.pos++
		}
	}
	if !s.take(")") || s.peek() != "{" || !s.skipGroup() {
		return "", false
	}
	owner, ownerOK := values["owner"]
	name, nameOK := values["name"]
	if !ownerOK || !nameOK {
		return "", false
	}
	repo := owner + "/" + name
	if repo == "{owner}/{repo}" {
		repo = env.GHRepo
	}
	route := routeRepository(repo)
	return route.Repo, route.Routable
}

// readQueryNameByte also accepts numeric tokens in nested argument lists.
func readQueryNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func readQueryString(query string, start int) (string, int, bool) {
	for i := start + 1; i < len(query); i++ {
		if query[i] == '\\' {
			i++
			continue
		}
		if query[i] != '"' {
			continue
		}
		value, err := strconv.Unquote(query[start : i+1])
		return value, i + 1, err == nil
	}
	return "", 0, false
}
