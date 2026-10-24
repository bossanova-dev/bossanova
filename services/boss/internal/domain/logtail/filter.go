package logtail

import "strings"

// Filter selects log records by their outer or unwrapped plugin fields.
type Filter struct{ Repo, Plugin, Level string }

// Empty reports whether no selection criteria were supplied.
func (f Filter) Empty() bool { return f == Filter{} }

// Match reports whether a record passes. Raw records always pass so filtering
// cannot hide a panic or other non-JSON diagnostic output.
func (f Filter) Match(rec Record) bool {
	if !rec.Parsed {
		return true
	}
	if f.Repo != "" && !fieldEquals(rec, f.Repo, "repo", "repo_id") {
		return false
	}
	if f.Plugin != "" && !fieldEquals(rec, f.Plugin, "plugin") {
		return false
	}
	return f.Level == "" || fieldEquals(rec, f.Level, "level")
}

func fieldEquals(rec Record, want string, keys ...string) bool {
	for _, fields := range []map[string]any{rec.Plugin, rec.Fields} {
		for _, key := range keys {
			if value, ok := fields[key].(string); ok && value != "" && strings.EqualFold(value, want) {
				return true
			}
		}
	}
	return false
}
