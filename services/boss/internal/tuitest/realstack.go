package tuitest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/realstack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// New creates a TUI driven by a real isolated bossd with the stub agent runner.
// Scenarios that still depend on mock-only controls must explicitly use NewMock.
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
	if cfg.moveSessionBehaviour != nil || cfg.archiveDelay != 0 || cfg.archiveError != "" || cfg.chatListDelay != 0 || cfg.chatListError != "" || len(cfg.agents) > 0 || len(cfg.trackerIssues) > 0 {
		t.Fatal("mock-only option passed to realstack; port the behavior or use NewMock")
	}
	stack := realstack.Start(t, realstack.Options{WorktreeBaseDir: cfg.worktreeBaseDir, FirstRunOnboarding: cfg.firstRunOnboarding})
	seedRealStack(t, stack, cfg)
	return launch(t, cfg, nil, stack)
}

func seedRealStack(t *testing.T, stack *realstack.Stack, cfg harnessConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := stack.Client()
	ids := make(map[string]string)
	sessionIDs := make(map[string]string)
	chatIDs := make(map[string]string)
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
		ids[repo.Id] = id
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
	// These scenarios need existing rows rather than an executing agent. Quick
	// chats are created through the API without inventing a long-lived tmux pane
	// for the stub runner. State-specific fixtures use Stack.SeedDB instead.
	// Create oldest first so newest-first ListSessions preserves fixture order.
	for i := len(cfg.sessions) - 1; i >= 0; i-- {
		session := cfg.sessions[i]
		stream, err := client.CreateSession(ctx, realstackrpc.Request(&pb.CreateSessionRequest{RepoId: ids[session.RepoId], Title: session.Title, Plan: session.Plan, AgentName: proto.String("stub"), IsQuickChat: true}))
		if err != nil {
			t.Fatalf("create fixture session: %v\n%s", err, stack.LogTail())
		}
		created := false
		for stream.Receive() {
			if stream.Msg().GetSessionCreated() != nil {
				created = true
				sessionIDs[session.Id] = stream.Msg().GetSessionCreated().GetSession().GetId()
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("create fixture session stream: %v\n%s", err, stack.LogTail())
		}
		_ = stream.Close()
		if !created {
			t.Fatalf("session create returned no session: %s", session.Title)
		}
	}
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
