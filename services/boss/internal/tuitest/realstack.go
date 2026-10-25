package tuitest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/realstack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// New creates a TUI driven by a real isolated bossd with the stub agent runner.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()
	if bossBinaryErr != nil {
		t.Fatal(bossBinaryErr)
	}
	if bossBinaryPath == "" {
		t.Fatal("BuildBoss must run in TestMain")
	}
	var cfg harnessConfig
	for _, option := range opts {
		option(&cfg)
	}

	stack := realstack.Start(t, realstack.Options{WorktreeBaseDir: cfg.worktreeBaseDir, FirstRunOnboarding: cfg.firstRunOnboarding, LinearEndpoint: trackerEndpoint(t, cfg)})
	repoIDs, sessionIDs := seedRealStack(t, stack, cfg)
	h := launch(t, cfg, stack)
	h.RepoIDs, h.SessionIDs = repoIDs, sessionIDs
	return h
}

// SeedStack populates an existing isolated daemon using the shared TUI fixture options.
// It returns fixture-key mappings for API-generated repository and session IDs.
func SeedStack(t *testing.T, stack *realstack.Stack, opts ...Option) (map[string]string, map[string]string) {
	var cfg harnessConfig
	for _, option := range opts {
		option(&cfg)
	}
	return seedRealStack(t, stack, cfg)
}

func seedRealStack(t *testing.T, stack *realstack.Stack, cfg harnessConfig) (map[string]string, map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ids, repoPaths := seedFixtureRepos(t, ctx, stack, cfg)
	sessionIDs := seedFixtureSessions(t, ctx, stack, cfg, ids, repoPaths)
	seedFixtureRelatedEntities(t, ctx, stack.Client(), cfg, ids, sessionIDs)
	return ids, sessionIDs
}

func seedFixtureRepos(t *testing.T, ctx context.Context, stack *realstack.Stack, cfg harnessConfig) (map[string]string, map[string]string) {
	t.Helper()
	client := stack.Client()
	ids := make(map[string]string)
	repoPaths := make(map[string]string)
	repos := append([]*pb.Repo(nil), cfg.repos...)
	// Legacy fixture sessions span more repositories than WithRepos' first row.
	// Materialise each referenced checkout instead of retaining orphan mock rows.
	for _, session := range cfg.sessions {
		found := false
		for _, repo := range repos {
			if repo.Id == session.RepoId {
				found = true
				break
			}
		}
		if !found {
			repos = append(repos, &pb.Repo{Id: session.RepoId, DisplayName: session.RepoDisplayName, DefaultBaseBranch: "main", MergeStrategy: "merge"})
		}
	}
	ghResponses := make([]realstack.GHResponse, 0, len(repos))
	for i, repo := range repos {
		name := repo.DisplayName
		if name == "" {
			name = fmt.Sprintf("repo-%d", i)
		}
		path := realstack.InitRepo(t, stack.Home(), name)
		response, err := client.RegisterRepo(ctx, realstackrpc.Request(&pb.RegisterRepoRequest{LocalPath: path, DisplayName: name, DefaultBaseBranch: "main", WorktreeBaseDir: filepath.Join(stack.Home(), "worktrees"), IsTrusted: proto.Bool(true)}))
		if err != nil {
			t.Fatalf("register fixture repo: %v\n%s", err, stack.LogTail())
		}
		id := response.Msg.Repo.Id
		if len(cfg.trackerIssues[repo.Id]) > 0 {
			key := "lin_fixture_only"
			_, err := client.UpdateRepo(ctx, realstackrpc.Request(&pb.UpdateRepoRequest{Id: id, LinearApiKey: &key}))
			if err != nil {
				t.Fatalf("configure fixture tracker: %v", err)
			}
		}
		ids[repo.Id] = id
		repoPaths[repo.Id] = path
		var prs []map[string]any
		for _, pr := range cfg.prs[repo.Id] {
			prs = append(prs, map[string]any{"number": pr.Number, "title": pr.Title, "headRefName": pr.HeadBranch, "state": "OPEN", "author": map[string]string{"login": pr.Author}})
		}
		if prs == nil {
			prs = []map[string]any{}
		}
		prJSON, err := json.Marshal(prs)
		if err != nil {
			t.Fatal(err)
		}
		ghResponses = append(ghResponses, realstack.GHResponse{Args: []string{"pr", "list", "--repo", response.Msg.Repo.OriginUrl, "--state", "open", "--json", "number,title,headRefName,state,author", "--limit", "300"}, Stdout: string(prJSON)})
		for _, issue := range cfg.trackerIssues[repo.Id] {
			ghResponses = append(ghResponses, realstack.GHResponse{Args: []string{"pr", "list", "--repo", response.Msg.Repo.OriginUrl, "--state", "all", "--search", issue.ExternalId + " in:title", "--json", "number,title,headRefName,state,author", "--limit", "100"}, Stdout: "[]"})
		}
		strategy := repo.MergeStrategy
		if strategy == "" {
			strategy = "merge"
		}
		_, err = client.UpdateRepo(ctx, realstackrpc.Request(&pb.UpdateRepoRequest{Id: id, MergeStrategy: &strategy, CanAutoMerge: &repo.CanAutoMerge, CanAutoMergeDependabot: &repo.CanAutoMergeDependabot, ShouldArchiveSessionsAfterMerge: &repo.ShouldArchiveSessionsAfterMerge, CanAutoDeleteBranches: &repo.CanAutoDeleteBranches}))
		if err != nil {
			t.Fatal(err)
		}
	}
	stack.SetGHResponses(t, ghResponses)
	return ids, repoPaths
}

func seedFixtureSessions(t *testing.T, ctx context.Context, stack *realstack.Stack, cfg harnessConfig, ids, repoPaths map[string]string) map[string]string {
	t.Helper()
	client := stack.Client()
	sessionIDs := make(map[string]string)
	// State-specific rows cannot be created through the API. Give them genuine
	// worktrees and let bossd's fixture stores persist their lifecycle state.
	// Create oldest first so newest-first ListSessions preserves fixture order.
	var persisted []map[string]any
	for i := len(cfg.sessions) - 1; i >= 0; i-- {
		session := cfg.sessions[i]
		if session.State == pb.SessionState_SESSION_STATE_UNSPECIFIED && session.ArchivedAt == nil && session.DisplayLabel == "" && len(cfg.checkSnapshots[session.Id]) == 0 {
			stream, err := client.CreateSession(ctx, realstackrpc.Request(&pb.CreateSessionRequest{RepoId: ids[session.RepoId], Title: session.Title, Plan: session.Plan, AgentName: proto.String("stub"), IsQuickChat: true}))
			if err != nil {
				t.Fatalf("create fixture session: %v\n%s", err, stack.LogTail())
			}
			for stream.Receive() {
				if created := stream.Msg().GetSessionCreated(); created != nil {
					sessionIDs[session.Id] = created.GetSession().GetId()
				}
			}
			streamErr := stream.Err()
			_ = stream.Close()
			if streamErr != nil {
				t.Fatalf("create fixture session stream: %v", streamErr)
			}
			if sessionIDs[session.Id] == "" {
				t.Fatalf("session create returned no session: %s", session.Title)
			}
			continue
		}
		branch := fmt.Sprintf("boss/fixture-%d", i)
		worktree := filepath.Join(stack.Home(), "worktrees", fmt.Sprintf("fixture-%d", i))
		realstack.Git(t, repoPaths[session.RepoId], "worktree", "add", "-b", branch, worktree, "main")
		state := session.State
		if state == pb.SessionState_SESSION_STATE_UNSPECIFIED {
			state = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
		}
		row := map[string]any{
			"key": session.Id, "repo_id": ids[session.RepoId], "title": session.Title,
			"plan": session.Plan, "worktree_path": worktree, "branch_name": branch, "base_branch": "main",
			"state": int(state), "is_quick_chat": false,
			"display_label": session.DisplayLabel, "display_intent": int32(session.DisplayIntent),
			"has_display_spinner": session.DisplaySpinner, "blocked_reason": session.GetBlockedReason(),
			"setup_error": session.SetupError, "is_automation_enabled": session.IsAutomationEnabled,
		}
		row["check_snapshots"] = fixtureCheckSnapshots(cfg.checkSnapshots[session.Id])
		if session.ArchivedAt != nil {
			row["archived_at"] = session.ArchivedAt.AsTime()
		}
		persisted = append(persisted, row)
	}
	if len(persisted) > 0 {
		spec, err := json.Marshal(map[string]any{"version": 1, "sessions": persisted})
		if err != nil {
			t.Fatal(err)
		}
		for key, id := range stack.SeedDB(t, spec) {
			sessionIDs[key] = id
		}
	}

	return sessionIDs
}

// fixtureCheckSnapshots converts check observations into seeddb rows.
func fixtureCheckSnapshots(snapshots []*pb.CheckSnapshot) []map[string]any {
	rows := make([]map[string]any, 0, len(snapshots))
	for _, snapshot := range snapshots {
		rows = append(rows, map[string]any{"polled_at": snapshot.PolledAt.AsTime(), "head_sha": snapshot.HeadSha, "raw_json": snapshot.RawJson, "computed_status": int(snapshot.ComputedStatus)})
	}
	return rows
}

func seedFixtureRelatedEntities(t *testing.T, ctx context.Context, client realstackrpc.Client, cfg harnessConfig, ids, sessionIDs map[string]string) {
	t.Helper()
	chatIDs := make(map[string]string)

	for _, note := range cfg.notes {
		_, err := client.CreateNote(ctx, realstackrpc.Request(&pb.CreateNoteRequest{RepoId: ids[note.RepoId], Body: note.Body, Tags: note.Tags}))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, chat := range cfg.chats {
		response, err := client.RecordChat(ctx, realstackrpc.Request(&pb.RecordChatRequest{SessionId: sessionIDs[chat.SessionId], AgentSessionId: chat.AgentSessionId, Title: chat.Title, AgentName: proto.String("stub"), Role: chat.Role}))
		if err != nil {
			t.Fatalf("record fixture chat: %v", err)
		}
		chatIDs[chat.Id] = response.Msg.GetChat().GetId()
	}
	for _, job := range cfg.cronJobs {
		_, err := client.CreateCronJob(ctx, realstackrpc.Request(&pb.CreateCronJobRequest{RepoId: ids[job.RepoId], Name: job.Name, Prompt: job.Prompt, Schedule: job.Schedule, Timezone: job.Timezone, IsEnabled: job.IsEnabled, AgentName: "stub", Model: job.Model, GateCommand: job.GateCommand, ShouldRunSetupCommand: &job.ShouldRunSetupCommand, IsZeroOutput: &job.IsZeroOutput, ConcurrencyPolicy: &job.ConcurrencyPolicy, ManagedBy: job.ManagedBy}))
		if err != nil {
			t.Fatalf("create fixture cron job: %v", err)
		}
	}
	for _, callback := range cfg.githubCallbacks {
		_, err := client.CreateGitHubCallback(ctx, realstackrpc.Request(&pb.CreateGitHubCallbackRequest{GroupId: &callback.GroupId, TargetChatId: chatIDs[callback.TargetChatId], RepoOwner: callback.RepoOwner, RepoName: callback.RepoName, PrNumber: callback.PrNumber, Trigger: callback.Trigger, Message: callback.Message, ExpiresAt: callback.ExpiresAt, ShouldRequireTransition: &callback.ShouldRequireTransition}))
		if err != nil {
			t.Fatalf("create fixture callback: %v", err)
		}
	}
}

// trackerEndpoint exposes only the issue query consumed by the real Linear plugin.
func trackerEndpoint(t *testing.T, cfg harnessConfig) string {
	t.Helper()
	if len(cfg.trackerIssues) == 0 {
		return ""
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var query struct {
			Variables struct {
				Filter fixtureIssueFilter `json:"filter"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var nodes []map[string]any
		for _, issues := range cfg.trackerIssues {
			for _, issue := range issues {
				if !query.Variables.Filter.matches(issue) {
					continue
				}
				state := issue.State
				if state == "" {
					state = "Todo"
				}
				nodes = append(nodes, map[string]any{"identifier": issue.ExternalId, "title": issue.Title, "description": issue.Description, "branchName": issue.BranchName, "url": issue.Url, "state": map[string]string{"name": state}})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": nodes}}}); err != nil {
			t.Errorf("encode tracker fixture: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// fixtureIssueClause mirrors the title and number predicates used by Linear.
type fixtureIssueClause struct {
	Title struct {
		Contains string `json:"containsIgnoreCase"`
	} `json:"title"`
	Number struct {
		Equal int `json:"eq"`
	} `json:"number"`
}

type fixtureIssueFilter struct {
	Title struct {
		Contains string `json:"containsIgnoreCase"`
	} `json:"title"`
	Or []fixtureIssueClause `json:"or"`
}

func (f fixtureIssueFilter) matches(issue *pb.TrackerIssue) bool {
	if f.Title.Contains != "" && !strings.Contains(strings.ToLower(issue.Title), strings.ToLower(f.Title.Contains)) {
		return false
	}
	if len(f.Or) == 0 {
		return true
	}
	for _, clause := range f.Or {
		if clause.matches(issue) {
			return true
		}
	}
	return false
}

func (c fixtureIssueClause) matches(issue *pb.TrackerIssue) bool {
	return c.Title.Contains != "" && strings.Contains(strings.ToLower(issue.Title), strings.ToLower(c.Title.Contains)) ||
		c.Number.Equal != 0 && strings.HasSuffix(issue.ExternalId, "-"+strconv.Itoa(c.Number.Equal))
}
