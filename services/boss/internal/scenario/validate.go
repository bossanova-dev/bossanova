package scenario

import (
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Validate checks all references, lifecycle values and operation prerequisites.
func Validate(doc Document) []Problem {
	ps := []Problem{}
	if doc.Version != 1 {
		ps = append(ps, Problem{Path: "version", Code: "version", Message: "unsupported scenario version", Hint: "Set version to 1.", Allowed: []string{"1"}})
	}
	required(&ps, "name", doc.Name)
	repos := map[string]bool{}
	for i, r := range doc.Repos {
		path := fmt.Sprintf("repos[%d]", i)
		uniqueKey(&ps, repos, path, r.Key)
		required(&ps, path+".displayName", r.DisplayName)
		if r.MergeStrategy != "" {
			enumProblem(&ps, path+".mergeStrategy", r.MergeStrategy, []string{"merge", "rebase", "squash"})
		}
	}
	sessions := map[string]bool{}
	sessionByKey := map[string]Session{}
	for i, s := range doc.Sessions {
		path := fmt.Sprintf("sessions[%d]", i)
		uniqueKey(&ps, sessions, path, s.Key)
		sessionByKey[s.Key] = s
		reference(&ps, path+".repo", s.Repo, repos)
		required(&ps, path+".title", s.Title)
		validateEnum(&ps, path+".state", s.State, "sessionState")
		chats := map[string]bool{}
		for j, c := range s.Chats {
			cp := fmt.Sprintf("%s.chats[%d]", path, j)
			uniqueKey(&ps, chats, cp, c.Key)
			required(&ps, cp+".title", c.Title)
		}
		if s.PR != nil {
			validatePR(&ps, path+".pr", *s.PR)
		}
	}
	for i, op := range doc.Operations {
		path := fmt.Sprintf("operations[%d]", i)
		if !slices.Contains(operationNames(), op.Op) {
			ps = append(ps, Problem{Path: path + ".op", Code: "unsupported-operation", Message: "unsupported transient operation", Hint: "Choose a supported operation.", Allowed: operationNames()})
		}
		reference(&ps, path+".session", op.Session, sessions)
		if s, ok := sessionByKey[op.Session]; ok && op.Op == "holdMerge" && s.PR == nil {
			ps = append(ps, Problem{Path: path + ".session", Code: "required", Message: "holdMerge requires a pull request", Hint: "Add a pr input to the referenced session."})
		}
	}
	return ps
}

func validatePR(ps *[]Problem, path string, pr PullRequest) {
	if pr.HeadSHA != "" && pr.HeadSHA != "branch-head" {
		_, err := hex.DecodeString(pr.HeadSHA)
		if err != nil || (len(pr.HeadSHA) != 40 && len(pr.HeadSHA) != 64) {
			*ps = append(*ps, Problem{Path: path + ".headSha", Code: "invalid-head", Message: "headSha must be a full Git object ID or branch-head", Hint: "Use branch-head for the scenario worktree HEAD."})
		}
	}
	if pr.Number <= 0 || pr.Number > math.MaxInt32 {
		*ps = append(*ps, Problem{Path: path + ".number", Code: "out-of-range", Message: "PR number is outside 1..2147483647", Hint: "Use a positive int32 PR number."})
	}
	validateEnum(ps, path+".state", pr.State, "prState")
	checks := pr.Checks
	if checks == "PASSING" {
		checks = "PASSED"
	}
	validateEnum(ps, path+".checks", checks, "checks")
}

func required(ps *[]Problem, path, value string) {
	if strings.TrimSpace(value) == "" {
		*ps = append(*ps, Problem{Path: path, Code: "required", Message: "value is required", Hint: "Provide a non-empty value."})
	}
}
func uniqueKey(ps *[]Problem, seen map[string]bool, path, key string) {
	required(ps, path+".key", key)
	if key == "" {
		return
	}
	if seen[key] {
		*ps = append(*ps, Problem{Path: path + ".key", Code: "duplicate-key", Message: "key is already used in this collection", Hint: "Choose a unique key within this collection."})
	}
	seen[key] = true
}
func reference(ps *[]Problem, path, key string, known map[string]bool) {
	if strings.TrimSpace(key) == "" {
		required(ps, path, key)
		return
	}
	if !known[key] {
		*ps = append(*ps, Problem{Path: path, Code: "unknown-reference", Message: "key does not name an entity in this document", Hint: "Declare the referenced key in the corresponding collection."})
	}
}
func validateEnum(ps *[]Problem, path, value, kind string) {
	if strings.TrimSpace(value) == "" {
		required(ps, path, value)
		return
	}
	enumProblem(ps, path, value, enumValues()[kind])
}
func enumProblem(ps *[]Problem, path, value string, allowed []string) {
	if !slices.Contains(allowed, value) {
		*ps = append(*ps, Problem{Path: path, Code: "unsupported-value", Message: fmt.Sprintf("unsupported value %q", value), Hint: "Choose one of the allowed values.", Allowed: allowed})
	}
}
