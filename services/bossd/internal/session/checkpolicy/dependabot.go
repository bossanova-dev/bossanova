package checkpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// PathScopedCheck requires Name when any Paths glob matches, or unconditionally
// when Paths is omitted. Patterns are compiled once when the config changes.
type PathScopedCheck struct {
	Name     string   `json:"name"`
	Paths    []string `json:"paths,omitempty"`
	patterns []*regexp.Regexp
}

// DependabotPolicy opts merge-time checks into current-base ancestry validation.
// Malformed policies are enabled and fail closed.
type DependabotPolicy struct {
	Enabled   bool
	Malformed error
	Checks    []PathScopedCheck
}

// ParseDependabot extracts and validates the optional ci.dependabot block.
func ParseDependabot(data []byte) DependabotPolicy {
	var doc struct {
		CI json.RawMessage `json:"ci"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		if json.Valid(data) {
			return DependabotPolicy{}
		}
		return DependabotPolicy{Enabled: true, Malformed: err}
	}
	var ci map[string]json.RawMessage
	if len(doc.CI) != 0 {
		if err := json.Unmarshal(doc.CI, &ci); err != nil {
			return DependabotPolicy{}
		}
	}
	raw, ok := ci["dependabot"]
	if !ok {
		return DependabotPolicy{}
	}
	p := DependabotPolicy{Enabled: true}
	var block struct {
		Required json.RawMessage `json:"required"`
	}
	if err := decodeDependabot(raw, &block); err != nil {
		p.Malformed = err
		return p
	}
	if len(block.Required) == 0 {
		return p
	}
	if bytes.Equal(bytes.TrimSpace(block.Required), []byte("null")) {
		p.Malformed = fmt.Errorf("ci.dependabot.required must be an array")
		return p
	}
	if err := decodeDependabot(block.Required, &p.Checks); err != nil {
		p.Malformed = err
		return p
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(block.Required, &entries); err != nil {
		p.Malformed = err
		return p
	}
	for i := range p.Checks {
		if err := p.Checks[i].compile(entries[i]); err != nil {
			p.Malformed = err
			return p
		}
	}

	return p
}

func (c *PathScopedCheck) compile(entry map[string]json.RawMessage) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("ci.dependabot names must be non-empty")
	}
	if raw, ok := entry["paths"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("ci.dependabot paths must be an array")
	}
	for _, pattern := range c.Paths {
		if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "!") {
			return fmt.Errorf("invalid ci.dependabot path %q", pattern)
		}
		c.patterns = append(c.patterns, compileDependabotGlob(pattern))
	}
	return nil
}

func decodeDependabot(data []byte, dest any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("ci.dependabot must not be null")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(dest)
}

func compileDependabotGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?s)^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	b.WriteByte('$')
	return regexp.MustCompile(b.String())
}

// RequiredFor resolves the names applicable to the PR's changed paths.
func (p DependabotPolicy) RequiredFor(files []string) []string {
	var names []string
	for _, c := range p.Checks {
		matched := c.Paths == nil
	matching:
		for _, re := range c.patterns {
			for _, file := range files {
				if re.MatchString(file) {
					matched = true
					break matching
				}
			}
		}
		if matched {
			names = append(names, c.Name)
		}
	}
	return names
}

// DependabotPolicy returns the cached merge policy for a repository reference.
func (r *Resolver) DependabotPolicy(ctx context.Context, repoRef string) DependabotPolicy {
	if r == nil || repoRef == "" {
		return DependabotPolicy{}
	}
	dir := r.resolvePath(ctx, repoRef)
	if dir == "" {
		return DependabotPolicy{}
	}
	return r.forPath(dir).dependabot
}
