package scenario

import (
	"fmt"
	"strings"
	"testing"
)

const validDocument = `{"version":1,"name":"demo","repos":[{"key":"app","displayName":"App","mergeStrategy":"merge"}],"sessions":[{"key":"one","repo":"app","title":"One","state":"BLOCKED","pr":{"number":101,"state":"OPEN","checks":"PASSING","mergeable":true},"chats":[{"key":"main","title":"Main"}]}],"operations":[{"op":"holdMerge","session":"one"}]}`

func TestDecodeValid(t *testing.T) {
	s, problems := Decode([]byte(validDocument))
	if len(problems) != 0 || s.Name != "demo" {
		t.Fatalf("scenario=%+v problems=%+v", s, problems)
	}
}

func TestDecodeProblems(t *testing.T) {
	tests := []struct{ name, document, path, code string }{
		{"required", `{"version":1}`, "name", "required"},
		{"version", `{"version":2,"name":"x"}`, "version", "version"},
		{"unknown", `{"version":1,"name":"x","typo":1}`, "typo", "unknown-field"},
		{"duplicate", `{"version":1,"name":"x","repos":[{"key":"a","displayName":"A"},{"key":"a","displayName":"B"}]}`, "repos[1].key", "duplicate-key"},
		{"reference", `{"version":1,"name":"x","sessions":[{"key":"a","repo":"missing","title":"A","state":"BLOCKED"}]}`, "sessions[0].repo", "unknown-reference"},
		{"state", `{"version":1,"name":"x","sessions":[{"key":"a","repo":"missing","title":"A","state":"BAD"}]}`, "sessions[0].state", "unsupported-value"},
		{"range", strings.Replace(validDocument, `"number":101`, `"number":0`, 1), "sessions[0].pr.number", "out-of-range"},
		{"operation", strings.Replace(validDocument, `"holdMerge"`, `"invented"`, 1), "operations[0].op", "unsupported-operation"},
		{"unsupported", `{"version":1,"name":"x","trackerIssues":[]}`, "trackerIssues", "unsupported-input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { _, ps := Decode([]byte(tt.document)); requireProblem(t, ps, tt.path, tt.code) })
	}
}

func TestOutputFieldsAnywhere(t *testing.T) {
	for _, field := range []string{"displayLabel", "display_label", "displayIntent", "display_intent", "displaySpinner", "display_spinner", "intent", "color", "label"} {
		for _, parent := range []string{"", "sessions[0]", "sessions[0].chats[0]", "unknown.nested"} {
			t.Run(field+parent, func(t *testing.T) {
				member := fmt.Sprintf(`%q:"answer"`, field)
				doc := `{` + member + `}`
				switch parent {
				case "sessions[0]":
					doc = `{"sessions":[{` + member + `}]}`
				case "sessions[0].chats[0]":
					doc = `{"sessions":[{"chats":[{` + member + `}]}]}`
				case "unknown.nested":
					doc = `{"unknown":{"nested":{` + member + `}}}`
				}
				path := field
				if parent != "" {
					path = parent + "." + field
				}
				_, ps := Decode([]byte(doc))
				p := requireProblem(t, ps, path, "output-field")
				if !strings.Contains(p.Hint, "UI") {
					t.Fatalf("hint=%q", p.Hint)
				}
			})
		}
	}
}

func TestAggregateOperations(t *testing.T) {
	doc := `{"version":1,"name":"x","repos":[{"key":"r","displayName":"R"}],"sessions":[{"key":"s","repo":"r","title":"S","state":"BLOCKED"}],"operations":[{"op":"unknown","session":"s"},{"op":"holdMerge","session":"s"}]}`
	_, ps := Decode([]byte(doc))
	requireProblem(t, ps, "operations[0].op", "unsupported-operation")
	requireProblem(t, ps, "operations[1].session", "required")
}

func TestStrictDocument(t *testing.T) {
	for _, doc := range []string{"null", "[]", "{} {}", "{} garbage", `{"version":`, `{"version":1,"name":"x","sessions":"bad"}`} {
		if _, ps := Decode([]byte(doc)); len(ps) == 0 {
			t.Errorf("accepted %s", doc)
		}
	}
}

func requireProblem(t *testing.T, ps []Problem, path, code string) Problem {
	t.Helper()
	for _, p := range ps {
		if p.Path == path && p.Code == code {
			return p
		}
	}
	t.Fatalf("missing %s at %s in %+v", code, path, ps)
	return Problem{}
}

func TestAggregateTypeErrors(t *testing.T) {
	_, ps := Decode([]byte(`{"version":"bad","name":17,"repos":{},"sessions":[{"key":"s","repo":"missing","title":"S","state":"BAD"}]}`))
	requireProblem(t, ps, "version", "unsupported-value")
	requireProblem(t, ps, "name", "unsupported-value")
	requireProblem(t, ps, "repos", "unsupported-value")
	requireProblem(t, ps, "sessions[0].repo", "unknown-reference")
	requireProblem(t, ps, "sessions[0].state", "unsupported-value")
}

func TestAggregateCollectionTypeErrors(t *testing.T) {
	_, ps := Decode([]byte(`{"version":1,"name":"x","repos":[false,12],"sessions":[null]}`))
	requireProblem(t, ps, "repos[0]", "unsupported-value")
	requireProblem(t, ps, "repos[1]", "unsupported-value")
	requireProblem(t, ps, "sessions[0]", "unsupported-value")
}
