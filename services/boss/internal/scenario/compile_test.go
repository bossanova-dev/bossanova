package scenario

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/realstack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

func TestRemapCompiledOpenPRLists(t *testing.T) {
	doc := Document{Version: 1, Name: "remap", Repos: []Repo{{Key: "repo", DisplayName: "Repo"}, {Key: "owner/other", DisplayName: "Other"}}}
	plan, problems := Compile(doc)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	origins := map[string]string{"repo": "scenario/registered", "owner/other": "scenario/other"}
	matched := 0
	for _, response := range plan.GH {
		before := slices.Clone(response.Args)
		stdout := response.Stdout
		remapOpenPRList(&response, origins)
		if slices.Equal(before, response.Args) {
			continue
		}
		matched++
		if response.Stdout != stdout {
			t.Fatal("remapping changed PR data")
		}
		valid := false
		for _, origin := range origins {
			valid = valid || slices.Equal(response.Args, realstack.OpenPRListResponse("https://github.com/"+origin+".git", "[]").Args)
		}
		if !valid {
			t.Fatalf("fixture does not route to a registered origin: %v", response.Args)
		}
	}
	if matched != len(origins) {
		t.Fatalf("remapped %d lists, want %d", matched, len(origins))
	}
}

func TestRemapGitHubAPIRepoPaths(t *testing.T) {
	for _, args := range [][]string{
		{"api", "repos/repo/pulls/42", "-i"},
		{"api", "-X", "PUT", "repos/repo/pulls/42/merge", "-f", "merge_method=merge"},
		{"api", "-X", "DELETE", "repos/repo/git/refs/heads/feature"},
	} {
		response := realstack.GHResponse{Args: slices.Clone(args)}
		RemapGHResponse(&response, map[string]string{"repo": "scenario/registered"})
		for i, arg := range args {
			want := strings.Replace(arg, "repos/repo/", "repos/scenario/registered/", 1)
			if response.Args[i] != want {
				t.Fatalf("remapped argv %v: argument %d = %q, want %q", response.Args, i, response.Args[i], want)
			}
		}
	}
}

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
		if strings.Contains(strings.Join(response.Args, " "), "reviewDecision") {
			value := fixturePRNode(t, response.Stdout)
			if value["isDraft"] != true || value["mergeable"] != "MERGEABLE" {
				t.Fatal(response)
			}
		} else if strings.Contains(strings.Join(response.Args, " "), "statusCheckRollup") && !strings.Contains(response.Stdout, `"conclusion":"FAILURE"`) {
			t.Fatal(response)
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
		if strings.Contains(strings.Join(response.Args, " "), "reviewDecision") {
			raw := fixturePRNode(t, response.Stdout)
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

func TestPRSizes_Sessions_AC213501(t *testing.T) {
	input := `{"version":1,"name":"sizes","repos":[{"key":"r","displayName":"Repo"}],"sessions":[{"key":"s","repo":"r","title":"Sizes","state":"READY_FOR_REVIEW","pr":{"number":1,"state":"OPEN","checks":"PASSED","mergeable":true,"additions":12,"deletions":3,"changedFiles":2}}]}`
	doc, ps := Decode([]byte(input))
	if len(ps) != 0 {
		t.Fatal(ps)
	}
	plan, ps := Compile(doc)
	if len(ps) != 0 {
		t.Fatal(ps)
	}
	found := false
	for _, r := range plan.GH {
		if !strings.Contains(strings.Join(r.Args, " "), "reviewDecision") {
			continue
		}
		found = true
		fields := strings.Fields(r.Args[3])
		raw := fixturePRNode(t, r.Stdout)
		for field, want := range map[string]float64{"additions": 12, "deletions": 3, "changedFiles": 2} {
			has := false
			for _, f := range fields {
				if f == field {
					has = true
				}
			}
			if !has || raw[field] != want {
				t.Fatalf("field %s absent or wrong: %v %s", field, r.Args, r.Stdout)
			}
		}
	}
	if !found {
		t.Fatal("missing pr view")
	}
	for _, field := range []string{"additions", "deletions", "changedFiles"} {
		bad := strings.Replace(input, `"`+field+`":`+map[string]string{"additions": "12", "deletions": "3", "changedFiles": "2"}[field], `"`+field+`":-1`, 1)
		_, ps := Decode([]byte(bad))
		matched := false
		for _, p := range ps {
			if p.Path == "sessions[0].pr."+field {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("negative %s not rejected at its path: %+v", field, ps)
		}
	}
}

func TestPublicRepositoryIdentityFixture(t *testing.T) {
	doc := Document{Version: 1, Name: "public", Repos: []Repo{{Key: "r", DisplayName: "Public", ProviderRepoID: 42, Visibility: "public"}}}
	plan, _ := Compile(doc)
	found := false
	for _, response := range plan.GH {
		if slices.Equal(response.Args, []string{"api", "repos/r", "-i"}) {
			var repo struct {
				ID         int    `json:"id"`
				Visibility string `json:"visibility"`
			}
			if err := json.Unmarshal([]byte(response.Stdout), &repo); err != nil {
				t.Fatal(err)
			}
			if repo.ID != 42 || repo.Visibility != "public" {
				t.Fatalf("fixture: %s", response.Stdout)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("repository identity fixture absent")
	}
}

func TestCompilePerPRGraphQLFixtures(t *testing.T) {
	doc := Document{Version: 1, Name: "per-pr", Repos: []Repo{{Key: "r", DisplayName: "R"}, {Key: "a", DisplayName: "A"}}, Sessions: []Session{
		{Key: "one", Repo: "r", Title: "One", State: "READY_FOR_REVIEW", PR: &PullRequest{Number: 7, State: "OPEN", Checks: "PASSED", Mergeable: true}},
		{Key: "two", Repo: "a", Title: "Two", State: "READY_FOR_REVIEW", PR: &PullRequest{Number: 7, State: "OPEN", Checks: "FAILED", Mergeable: true}},
	}}
	plan, problems := Compile(doc)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	origins := map[string]string{"r": "scenario/one", "a": "scenario/two"}
	seen := map[string]bool{}
	matched := 0
	for _, response := range plan.GH {
		RemapPRQueries(&response, doc, origins)
		key := strings.Join(response.Args, "\x00")
		if seen[key] {
			t.Fatalf("duplicate argv: %v", response.Args)
		}
		seen[key] = true
		for _, session := range doc.Sessions {
			origin := "https://github.com/" + origins[session.Repo] + ".git"
			status, _ := vcs.GitHubPRStatusArgs(origin, 7, "")
			checks, _ := vcs.GitHubPRChecksArgs(origin, 7, "")
			if slices.Equal(response.Args, status) {
				matched++
				node := fixturePRNode(t, response.Stdout)
				if node["state"] != "OPEN" {
					t.Fatal(node)
				}
			}
			if slices.Equal(response.Args, checks) {
				matched++
				want := "SUCCESS"
				if session.PR.Checks == "FAILED" {
					want = "FAILURE"
				}
				if !strings.Contains(response.Stdout, `"conclusion":"`+want+`"`) {
					t.Fatal(response.Stdout)
				}
			}
		}
	}
	if matched != 4 {
		t.Fatalf("got %d remapped per-PR queries, want 4", matched)
	}
}

func fixturePRNode(t *testing.T, body string) map[string]any {
	t.Helper()
	var page struct {
		Data struct {
			Repository struct {
				PullRequest map[string]any `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	return page.Data.Repository.PullRequest
}
