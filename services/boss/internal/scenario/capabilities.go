package scenario

import (
	"reflect"
	"sort"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// CapabilitySet describes the complete v1 authoring surface and explicit gaps.
type CapabilitySet struct {
	InputDescriptions map[string]string   `json:"inputDescriptions"`
	Version           int                 `json:"version"`
	EntityFields      map[string][]string `json:"entityFields"`
	Enums             map[string][]string `json:"enums"`
	Operations        []string            `json:"operations"`
	UnsupportedInputs []Problem           `json:"unsupportedInputs"`
}

// Capabilities advertises supported inputs before a scenario is authored.
func Capabilities() CapabilitySet {
	fields := map[string][]string{}
	for _, typ := range []reflect.Type{reflect.TypeFor[Document](), reflect.TypeFor[Repo](), reflect.TypeFor[Session](), reflect.TypeFor[Chat](), reflect.TypeFor[PullRequest](), reflect.TypeFor[Operation]()} {
		names := []string{}
		for i := 0; i < typ.NumField(); i++ {
			names = append(names, fieldName(typ.Field(i)))
		}
		fields[typ.Name()] = names
	}
	return CapabilitySet{InputDescriptions: map[string]string{"PullRequest.body": "PR description returned by gh pr view, including Markdown markers.", "PullRequest.headSha": "Full 40- or 64-digit hexadecimal Git object ID, or branch-head to use the live prepared worktree HEAD. Offline readiness retains the symbolic input without polling."}, Version: 1, EntityFields: fields, Enums: enumValues(), Operations: operationNames(), UnsupportedInputs: []Problem{
		{Path: "trackerIssues", Code: "unsupported-input", Message: "external tracker issues are unsupported", Hint: "Use synthetic repository and session inputs; external tracker integration is unavailable."},
		{Path: "agentOutput", Code: "unsupported-input", Message: "real agent output is unsupported", Hint: "Use declared chats; real agent processes and output are unavailable."},
		{Path: "github", Code: "unsupported-input", Message: "real GitHub is unsupported", Hint: "Use synthetic session pr inputs instead of real GitHub access."},
	}}
}
func operationNames() []string { return []string{"holdMerge", "holdArchive"} }
func enumValues() map[string][]string {
	return map[string][]string{
		"chatRole":     {"build", "verify", "repair", "operator"},
		"sessionState": enumShortNames(pb.SessionState_value, "SESSION_STATE_"),
		"prState":      enumShortNames(pb.PRState_value, "PR_STATE_"),
		"checks":       enumShortNames(pb.ChecksOverall_value, "CHECKS_OVERALL_"),
	}
}
func enumShortNames(values map[string]int32, prefix string) []string {
	names := []string{}
	for full, value := range values {
		if value != 0 {
			names = append(names, strings.TrimPrefix(full, prefix))
		}
	}
	sort.Strings(names)
	return names
}
