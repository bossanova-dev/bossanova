package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCompileInputsOnly(t *testing.T) {
	d := Document{Version: 1, Name: "inputs", Repos: []Repo{{Key: "r", DisplayName: "Repo"}}, Sessions: []Session{{Key: "s", Repo: "r", Title: "Blocked", State: "BLOCKED", BlockedReason: "operator", PR: &PullRequest{Number: 1, State: "OPEN", Checks: "PASSED", Mergeable: true}, Chats: []Chat{{Key: "c", Title: "Chat"}}}}, Operations: []Operation{{Op: "holdMerge", Session: "s"}}}
	p, ps := Compile(d)
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	b, err := json.Marshal(p.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"display_label", "display_intent", "has_display_spinner"} {
		if strings.Contains(string(b), field) {
			t.Fatalf("output field %s present: %s", field, b)
		}
	}
	if p.Spec.Sessions[0].BlockedReason != "operator" || len(p.GH) == 0 || len(p.Operations) != 1 {
		t.Fatalf("incomplete plan: %+v", p)
	}
}

func TestCompileEveryStateAndPRInputs(t *testing.T) {
	d := Document{Version: 1, Name: "all", Repos: []Repo{{Key: "r", DisplayName: "Repo"}}}
	for _, state := range enumValues()["sessionState"] {
		d.Sessions = append(d.Sessions, Session{Key: state, Repo: "r", Title: state, State: state, BlockedReason: "blocked", RunBlockedReason: "run", SetupError: "setup", Archived: true, Chats: []Chat{{Key: "c", Title: "Chat"}}})
	}
	for i, state := range enumValues()["prState"] {
		d.Sessions = append(d.Sessions, Session{Key: "pr" + state, Repo: "r", Title: state, State: "IMPLEMENTING_PLAN", PR: &PullRequest{Number: i + 1, State: state, Checks: "FAILED", Mergeable: true, Draft: true}})
	}
	p, ps := Compile(d)
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	if len(p.Spec.Sessions) != len(d.Sessions) {
		t.Fatal("missing rows")
	}
	for i, row := range p.Spec.Sessions {
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err = json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"display_label", "display_intent", "has_display_spinner"} {
			if _, ok := raw[field]; ok {
				t.Fatalf("%s has output %s", row.Key, field)
			}
		}
		input := d.Sessions[i]
		if row.Title != input.Title || row.State != pb.SessionState_value["SESSION_STATE_"+input.State] {
			t.Fatalf("input lost: %+v", row)
		}
		if input.PR == nil && (row.SetupError != "setup" || row.RunBlockedReason != "run" || row.ArchivedAt == nil || len(row.Chats) != 1) {
			t.Fatalf("seed input lost: %+v", row)
		}
	}
	for _, response := range p.GH {
		if response.Args[0] != "pr" {
			continue
		}
		switch response.Args[1] {
		case "view":
			var value struct {
				Mergeable string `json:"mergeable"`
				Draft     bool   `json:"isDraft"`
			}
			if err := json.Unmarshal([]byte(response.Stdout), &value); err != nil {
				t.Fatal(err)
			}
			if !value.Draft || value.Mergeable != "MERGEABLE" {
				t.Fatal(response)
			}
		case "checks":
			if !strings.Contains(response.Stdout, `"state":"FAILURE"`) {
				t.Fatal(response)
			}
		case "list":
			if response.Args[3] != "r" || !strings.Contains(response.Stdout, `"number":`) {
				t.Fatal(response)
			}
		default:
			t.Fatalf("unexpected argv %v", response.Args)
		}
	}
}

func TestCompileOpenListExcludesTerminalPRs(t *testing.T) {
	doc := Document{Version: 1, Name: "prs", Repos: []Repo{{Key: "repo", DisplayName: "Repo"}}}
	for i, state := range []string{"OPEN", "OPEN", "CLOSED", "MERGED"} {
		doc.Sessions = append(doc.Sessions, Session{Key: fmt.Sprintf("pr%d", i), Repo: "repo", Title: state, State: "IMPLEMENTING_PLAN", PR: &PullRequest{Number: i + 1, State: state, Checks: "PASSED", Mergeable: true, Draft: i == 1}})
	}
	plan, problems := Compile(doc)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	for _, response := range plan.GH {
		if response.Args[1] != "list" {
			continue
		}
		var prs []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
		}
		if err := json.Unmarshal([]byte(response.Stdout), &prs); err != nil {
			t.Fatal(err)
		}
		if len(prs) != 2 {
			t.Fatalf("open list includes terminal PRs: %s", response.Stdout)
		}
		for _, pr := range prs {
			if pr.State != "OPEN" || pr.Number > 2 {
				t.Fatalf("invalid open PR: %+v", pr)
			}
		}
	}
}

func TestCompileSnapshotDisplayStatus(t *testing.T) {
	tests := []struct {
		name, state, checks string
		draft, mergeable    bool
		want                pb.DisplayStatus
	}{
		{"passing", "OPEN", "PASSED", false, true, pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{"passing alias", "OPEN", "PASSING", false, true, pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{"pending", "OPEN", "PENDING", false, true, pb.DisplayStatus_DISPLAY_STATUS_CHECKING},
		{"failed", "OPEN", "FAILED", false, true, pb.DisplayStatus_DISPLAY_STATUS_FAILING},
		{"draft flag", "OPEN", "FAILED", true, false, pb.DisplayStatus_DISPLAY_STATUS_DRAFT},
		{"conflict", "OPEN", "PASSED", false, false, pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
		{"closed", "CLOSED", "FAILED", true, false, pb.DisplayStatus_DISPLAY_STATUS_CLOSED},
		{"merged", "MERGED", "PENDING", false, false, pb.DisplayStatus_DISPLAY_STATUS_MERGED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := Document{Version: 1, Name: "snapshot", Repos: []Repo{{Key: "repo", DisplayName: "Repo"}}, Sessions: []Session{{Key: "session", Repo: "repo", Title: "Session", State: "IMPLEMENTING_PLAN", PR: &PullRequest{Number: 1, State: tt.state, Checks: tt.checks, Draft: tt.draft, Mergeable: tt.mergeable}}}}
			plan, problems := Compile(doc)
			if len(problems) > 0 {
				t.Fatal(problems)
			}
			snapshots := plan.Spec.Sessions[0].CheckSnapshots
			if len(snapshots) != 1 {
				t.Fatalf("got %d snapshots, want 1", len(snapshots))
			}
			if got := pb.DisplayStatus(snapshots[0].ComputedStatus); got != tt.want {
				t.Fatalf("initial snapshot status = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompilePRBodyAndHead(t *testing.T) {
	doc, ps := Decode([]byte(`{"version":1,"name":"partial","repos":[{"key":"r","displayName":"Repo"}],"sessions":[{"key":"s","repo":"r","title":"Partial","state":"READY_FOR_REVIEW","pr":{"number":1,"state":"OPEN","checks":"PASSED","mergeable":true,"body":"## Partial\ndo not merge — partial: 1/2","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}]}`))
	if len(ps) != 0 {
		t.Fatal(ps)
	}
	plan, ps := Compile(doc)
	if len(ps) != 0 {
		t.Fatal(ps)
	}
	for _, response := range plan.GH {
		if response.Args[1] == "view" {
			var raw map[string]any
			if err := json.Unmarshal([]byte(response.Stdout), &raw); err != nil {
				t.Fatal(err)
			}
			if raw["body"] != doc.Sessions[0].PR.Body || raw["headRefOid"] != doc.Sessions[0].PR.HeadSHA {
				t.Fatalf("PR inputs lost: %s", response.Stdout)
			}
			return
		}
	}
	t.Fatal("missing pr view")
}

func TestPRHeadInputValidation(t *testing.T) {
	for _, head := range []string{"branch-head", strings.Repeat("a", 40), strings.Repeat("b", 64), "short", "../outside", strings.Repeat("z", 40)} {
		doc := Document{Version: 1, Name: "heads", Repos: []Repo{{Key: "r", DisplayName: "Repo"}}, Sessions: []Session{{Key: "s", Repo: "r", Title: "Session", State: "READY_FOR_REVIEW", PR: &PullRequest{Number: 1, State: "OPEN", Checks: "PASSED", Mergeable: true, HeadSHA: head}}}}
		_, problems := Compile(doc)
		valid := head == "branch-head" || head == strings.Repeat("a", 40) || head == strings.Repeat("b", 64)
		if (len(problems) == 0) != valid {
			t.Fatalf("head %q problems: %+v", head, problems)
		}
	}
}
