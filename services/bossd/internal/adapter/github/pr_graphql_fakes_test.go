package github

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

func perPRQuery(args []string) (kind string, pr int, ok bool) {
	if !isGraphQL(args) {
		return "", 0, false
	}
	for _, a := range args {
		if n, found := strings.CutPrefix(a, "number="); found {
			pr, _ = strconv.Atoi(n)
		}
	}
	for k, expected := range map[string][]string{
		"status": vcs.GitHubPRStatusQueryArgs("", "", "", pr, ""),
		"checks": vcs.GitHubPRChecksQueryArgs("", "", "", pr, ""),
		"merge":  vcs.GitHubPRMergeCommitQueryArgs("", "", "", pr),
	} {
		if slices.Contains(args, expected[3]) {
			return k, pr, true
		}
	}
	return "", 0, false
}

func statusGraphQL(flattened string) string {
	var pr map[string]json.RawMessage
	if json.Unmarshal([]byte(flattened), &pr) != nil {
		return flattened
	}
	if pr == nil {
		return prGraphQL("null")
	}
	reviews := pr["reviews"]
	if len(reviews) == 0 || string(reviews) == "null" {
		reviews = json.RawMessage(`[]`)
	}
	pr["reviews"] = json.RawMessage(`{"pageInfo":{"hasNextPage":false},"nodes":` + string(reviews) + `}`)
	out, _ := json.Marshal(pr)
	return prGraphQL(string(out))
}

func prGraphQL(node string) string {
	return `{"data":{"repository":{"pullRequest":` + node + `}}}`
}

func checksGraphQL(rows string) string {
	var raw []checkRow
	if json.Unmarshal([]byte(rows), &raw) != nil {
		return rows
	}
	contexts := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		if row.Workflow == "" {
			contexts = append(contexts, map[string]any{"__typename": "StatusContext", "context": row.Name, "state": row.State, "description": row.Description})
			continue
		}
		status, conclusion := "COMPLETED", row.State
		switch row.State {
		case "QUEUED", "IN_PROGRESS", "WAITING", "PENDING", "REQUESTED":
			status, conclusion = row.State, ""
		}
		contexts = append(contexts, map[string]any{"__typename": "CheckRun", "name": row.Name, "status": status, "conclusion": conclusion, "checkSuite": map[string]any{"workflowRun": map[string]any{"workflow": map[string]any{"name": row.Workflow}}}})
	}
	nodes, _ := json.Marshal(contexts)
	return checksPage(string(nodes), false, "")
}

func checksPage(nodes string, next bool, cursor string) string {
	info, _ := json.Marshal(map[string]any{"hasNextPage": next, "endCursor": cursor})
	return prGraphQL(`{"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":` + nodes + `,"pageInfo":` + string(info) + `}}}}]}}`)
}

// perPRResponse adapts the legacy fixture bodies, leaving unrelated calls alone.
func perPRResponse(args []string, body string) string {
	kind, _, ok := perPRQuery(args)
	if !ok {
		return body
	}
	switch kind {
	case "status":
		return statusGraphQL(body)
	case "checks":
		return checksGraphQL(body)
	default:
		return prGraphQL(body)
	}
}

func isPRChecks(args []string) bool { kind, _, ok := perPRQuery(args); return ok && kind == "checks" }

func perPRFixtureGH(run ghFunc) ghFunc {
	return func(ctx context.Context, args ...string) (string, error) {
		out, err := run(ctx, args...)
		if err != nil {
			return out, err
		}
		return perPRResponse(args, out), nil
	}
}
