package evidence

import (
	"encoding/json"
	"strings"
)

// ProblemsMarker opens the machine line of verify's proof-missing note,
// mirroring PROOF_PROBLEMS_MARKER in skills-toolbox/verify/verify-proof-missing.mjs.
const ProblemsMarker = "<!-- boss-proof-problems:"

// PageProblem is why verify could not publish a head's proof page: the
// writer's reason code (such as upload-failed) and its bounded detail. The
// zero value means no problem was recorded.
type PageProblem struct {
	Reason string
	Detail string
}

type problemsWire struct {
	Head     string `json:"head"`
	Problems []struct {
		Part   string `json:"part"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"problems"`
}

// parsePageProblem returns the head and page problem of the last well-formed
// problems line in body; ok is false when there is none or it names no page
// problem.
func parsePageProblem(body string) (head string, problem PageProblem, ok bool) {
	var found problemsWire
	if !lastValid(body, ProblemsMarker, parseProblemsPayload, &found) {
		return "", PageProblem{}, false
	}
	for _, p := range found.Problems {
		if p.Part == "page" {
			return found.Head, PageProblem{Reason: p.Reason, Detail: p.Detail}, true
		}
	}
	return "", PageProblem{}, false
}

func parseProblemsPayload(payload string) (problemsWire, bool) {
	var w problemsWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &w); err != nil || !proofHead.MatchString(w.Head) {
		return problemsWire{}, false
	}
	for _, p := range w.Problems {
		if p.Part == "" || strings.TrimSpace(p.Reason) == "" || utf16Len(p.Detail) > maxSummaryUnits {
			return problemsWire{}, false
		}
	}
	return w, true
}

// notePageProblem records body's page problem under its head unless a newer
// comment already noted one for that head.
func notePageProblem(problems map[string]PageProblem, body string) {
	head, problem, ok := parsePageProblem(body)
	if !ok {
		return
	}
	if _, seen := problems[strings.ToLower(head)]; !seen {
		problems[strings.ToLower(head)] = problem
	}
}
