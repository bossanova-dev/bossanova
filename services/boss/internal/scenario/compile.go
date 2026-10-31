package scenario

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/boss/internal/realstack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// SeedSpec is the input-only subset of the offline store seeder contract.
type SeedSpec struct {
	Version  int           `json:"version"`
	Repos    []SeedRepo    `json:"repos,omitempty"`
	Sessions []SeedSession `json:"sessions"`
}

// SeedRepo describes a repository created only for a fresh offline run.
type SeedRepo struct {
	Key           string `json:"key"`
	DisplayName   string `json:"display_name"`
	LocalPath     string `json:"local_path"`
	OriginURL     string `json:"origin_url"`
	MergeStrategy string `json:"merge_strategy,omitempty"`
}

// SeedChat describes a conversation input. Archive keepers own an offline client chat.
type SeedChat struct {
	AgentSessionID string `json:"agent_session_id"`
	Title          string `json:"title"`
	Role           string `json:"role"`
}

// SeedCheck is a persisted synthetic PR observation.
type SeedCheck struct {
	PolledAt       time.Time `json:"polled_at"`
	RawJSON        string    `json:"raw_json"`
	ComputedStatus int32     `json:"computed_status"`
	HeadSHA        string    `json:"head_sha"`
}

// SeedSession deliberately has no computed display fields.
type SeedSession struct {
	PRNumber       *int        `json:"pr_number,omitempty"`
	ArchivedAt     *time.Time  `json:"archived_at,omitempty"`
	Chats          []SeedChat  `json:"chats,omitempty"`
	CheckSnapshots []SeedCheck `json:"check_snapshots,omitempty"`

	Key              string `json:"key"`
	RepoID           string `json:"repo_id"`
	Title            string `json:"title"`
	State            int32  `json:"state"`
	WorktreePath     string `json:"worktree_path"`
	BranchName       string `json:"branch_name"`
	BaseBranch       string `json:"base_branch"`
	BlockedReason    string `json:"blocked_reason"`
	RunBlockedReason string `json:"run_blocked_reason"`
	SetupError       string `json:"setup_error"`
}

// Plan contains store inputs, synthetic provider responses and ordered transient operations.
type Plan struct {
	Spec       SeedSpec               `json:"spec"`
	GH         []realstack.GHResponse `json:"responses"`
	Operations []Operation            `json:"operations"`
}

// Compile validates the entire document before producing any mutations.
func Compile(doc Document) (Plan, []Problem) {
	ps := Validate(doc)
	if len(ps) > 0 {
		return Plan{}, ps
	}
	p := Plan{Spec: SeedSpec{Version: 1, Sessions: []SeedSession{}}, GH: []realstack.GHResponse{}, Operations: append([]Operation{}, doc.Operations...)}
	lists := map[string][]map[string]any{}
	for _, r := range doc.Repos {
		lists[r.Key] = []map[string]any{}
	}
	for _, s := range doc.Sessions {
		p.Spec.Sessions = append(p.Spec.Sessions, SeedSession{Key: s.Key, RepoID: s.Repo, Title: s.Title, State: pb.SessionState_value["SESSION_STATE_"+s.State], BaseBranch: "main", BlockedReason: s.BlockedReason, RunBlockedReason: s.RunBlockedReason, SetupError: s.SetupError})
		row := &p.Spec.Sessions[len(p.Spec.Sessions)-1]
		if s.Archived {
			stamp := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			row.ArchivedAt = &stamp
		}
		for _, chat := range s.Chats {
			row.Chats = append(row.Chats, compileChat(s.Key, chat))
		}
		if s.State != "BLOCKED" && row.RunBlockedReason == "" {
			row.RunBlockedReason = row.BlockedReason
			row.BlockedReason = ""
		}
		if s.PR == nil {
			continue
		}
		pr := s.PR
		row.PRNumber = &pr.Number
		n := strconv.Itoa(pr.Number)
		state := pr.State
		if state == "DRAFT" {
			state = "OPEN"
		}
		if state == "OPEN" {
			lists[s.Repo] = append(lists[s.Repo], map[string]any{"number": pr.Number, "title": s.Title, "headRefName": s.Key, "state": state, "author": map[string]string{"login": "scenario"}})
		}
		mergeable := "CONFLICTING"
		if pr.Mergeable {
			mergeable = "MERGEABLE"
		}
		view, _ := json.Marshal(map[string]any{"body": pr.Body, "headRefOid": pr.HeadSHA, "state": state, "mergeable": mergeable, "mergeStateStatus": "CLEAN", "isDraft": pr.State == "DRAFT" || pr.Draft, "title": s.Title, "headRefName": s.Key, "baseRefName": "main"})
		checks := "PENDING"
		switch pr.Checks {
		case "PASSED", "PASSING":
			checks = "SUCCESS"
		case "FAILED":
			checks = "FAILURE"
		}
		checkRows := []map[string]string{{"name": "scenario", "state": checks, "workflow": "scenario", "description": "Synthetic scenario check"}}
		snapshotHead := ""
		for _, op := range doc.Operations {
			if op.Op == "holdMerge" && op.Session == s.Key {
				snapshotHead, _ = vcs.NormalizeHeadSHA(pr.HeadSHA)
				checkRows = append(checkRows, map[string]string{"name": "boss/verify", "state": "SUCCESS", "description": "Scenario merge authorization"})
				break
			}
		}
		checkJSON, _ := json.Marshal(checkRows)
		row.CheckSnapshots = append(row.CheckSnapshots, SeedCheck{PolledAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), RawJSON: string(checkJSON), HeadSHA: snapshotHead, ComputedStatus: int32(snapshotDisplayStatus(pr, checks))})
		restMergeability, _ := json.Marshal(map[string]any{"mergeable": pr.Mergeable, "mergeable_state": "clean", "rebaseable": true})
		p.GH = append(p.GH, realstack.GHResponse{Args: []string{"api", "repos/" + s.Repo + "/pulls/" + n}, Stdout: string(restMergeability)})
		p.GH = append(p.GH, realstack.GHResponse{Args: []string{"api", "repos/" + s.Repo + "/pulls/" + n + "/reviews?per_page=100", "--paginate"}, Stdout: "[]"})
		p.GH = append(p.GH, realstack.GHResponse{Args: []string{"pr", "view", n, "--repo", s.Repo, "--json", "state,mergeable,mergeStateStatus,isDraft,title,body,headRefName,baseRefName,headRefOid,isCrossRepository,reviewDecision,reviews"}, Stdout: string(view)}, realstack.GHResponse{Args: []string{"pr", "checks", n, "--repo", s.Repo, "--json", "name,state,workflow,description"}, Stdout: string(checkJSON)})
	}
	p.GH = append(p.GH, heldMergeResponses(doc)...)
	for _, r := range doc.Repos {
		p.GH = append(p.GH, realstack.GHResponse{Args: []string{"api", "repos/" + r.Key, "--jq", "{m:.allow_merge_commit,s:.allow_squash_merge,r:.allow_rebase_merge}"}, Stdout: `{"m":true,"s":true,"r":true}`})
		data, _ := json.Marshal(lists[r.Key])
		p.GH = append(p.GH, realstack.GHResponse{Args: []string{"pr", "list", "--repo", r.Key, "--state", "open", "--json", "number,title,headRefName,state,author", "--limit", "300"}, Stdout: string(data)})
	}
	return p, ps
}

func replaceBranch(value string, branches map[string]string) string {
	for key, branch := range branches {
		encoded, _ := json.Marshal(key)
		replacement, _ := json.Marshal(branch)
		value = strings.ReplaceAll(value, `"headRefName":`+string(encoded), `"headRefName":`+string(replacement))
	}
	return value
}

func replacePRHead(value string, branches, heads map[string]string) string {
	for key, head := range heads {
		if strings.Contains(value, `"headRefName":"`+branches[key]+`"`) {
			return strings.ReplaceAll(value, `"headRefOid":"branch-head"`, `"headRefOid":"`+head+`"`)
		}
	}
	return value
}

func snapshotDisplayStatus(pr *PullRequest, checkState string) vcs.DisplayStatus {
	state := vcs.PRStateOpen
	switch pr.State {
	case "CLOSED":
		state = vcs.PRStateClosed
	case "MERGED":
		state = vcs.PRStateMerged
	}
	status, conclusion, recognized := vcs.ParseGitHubCheckState(checkState)
	return vcs.ComputeDisplayStatus(&vcs.PRStatus{
		State: state, Draft: pr.Draft || pr.State == "DRAFT", Mergeable: &pr.Mergeable,
	}, []vcs.CheckResult{{Status: status, Conclusion: conclusion, Unclassified: !recognized}}, nil).Status
}

func heldMergeResponses(doc Document) []realstack.GHResponse {
	var responses []realstack.GHResponse
	for _, op := range doc.Operations {
		if op.Op != "holdMerge" {
			continue
		}
		for _, session := range doc.Sessions {
			if session.Key != op.Session {
				continue
			}
			strategy := "merge"
			for _, repo := range doc.Repos {
				if repo.Key == session.Repo && repo.MergeStrategy != "" {
					strategy = repo.MergeStrategy
				}
			}
			args := []string{"pr", "merge", strconv.Itoa(session.PR.Number), "--repo", session.Repo, "--" + strategy, "--delete-branch"}
			if session.PR.HeadSHA != "" {
				args = append(args, "--match-head-commit", session.PR.HeadSHA)
			}
			responses = append(responses, realstack.GHResponse{Args: args, Hold: operationName(op)})
		}
	}

	return responses
}

func compileChat(sessionKey string, chat Chat) SeedChat {
	role := chat.Role
	if role == "" {
		role = "operator"
	}
	return SeedChat{AgentSessionID: sessionKey + "-" + chat.Key, Title: chat.Title, Role: role}
}
