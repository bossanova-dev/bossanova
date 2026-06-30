package bossmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestMutatingTools drives every non-destructive mutating tool, asserting it
// forwards args to the configured Backend method and returns its result.
func TestMutatingTools(t *testing.T) {
	cases := []struct {
		tool     string
		args     map[string]any
		backend  *fakeBackend
		sentinel string
	}{
		{
			tool: "register_repo",
			args: map[string]any{"local_path": "/code/x", "name": "X", "setup_script": "make", "default_base_branch": "main"},
			backend: &fakeBackend{registerRepo: func(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
				if req.GetLocalPath() != "/code/x" || req.GetDisplayName() != "X" || req.GetSetupScript() != "make" || req.GetDefaultBaseBranch() != "main" {
					t.Errorf("register_repo args not forwarded: %+v", req)
				}
				return &pb.Repo{Id: "repo-rr"}, nil
			}},
			sentinel: "repo-rr",
		},
		{
			tool: "clone_and_register_repo",
			args: map[string]any{"clone_url": "git@host:x.git", "local_path": "/code/x", "name": "X", "default_base_branch": "main", "setup_script": "make"},
			backend: &fakeBackend{cloneAndRegisterRepo: func(_ context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
				if req.GetCloneUrl() != "git@host:x.git" || req.GetLocalPath() != "/code/x" || req.GetDisplayName() != "X" || req.GetDefaultBaseBranch() != "main" || req.GetSetupScript() != "make" {
					t.Errorf("clone_and_register_repo args not forwarded: %+v", req)
				}
				return &pb.Repo{Id: "repo-crr"}, nil
			}},
			sentinel: "repo-crr",
		},
		{
			tool: "update_repo",
			args: map[string]any{"id": "r1", "name": "newname", "merge_strategy": "squash"},
			backend: &fakeBackend{updateRepo: func(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
				if req.GetId() != "r1" || req.GetDisplayName() != "newname" || req.GetMergeStrategy() != "squash" {
					t.Errorf("update_repo args not forwarded: %+v", req)
				}
				return &pb.Repo{Id: "repo-ur"}, nil
			}},
			sentinel: "repo-ur",
		},
		{
			tool: "create_session",
			args: map[string]any{"repo_id": "r1", "prompt": "do it", "title": "Do the thing", "agent": "claude"},
			backend: &fakeBackend{createSession: func(_ context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
				if req.GetRepoId() != "r1" || req.GetPlan() != "do it" || req.GetTitle() != "Do the thing" || req.GetAgentName() != "claude" {
					t.Errorf("create_session args not forwarded: %+v", req)
				}
				return &pb.Session{Id: "sess-cs"}, nil
			}},
			sentinel: "sess-cs",
		},
		{
			tool: "create_session",
			args: map[string]any{"repo_id": "r1", "prompt": "Add avatar upload\nto the profile page"},
			backend: &fakeBackend{createSession: func(_ context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
				// Title omitted -> derived from the first line of the prompt so
				// bossd's "title is required" guard is satisfied.
				if req.GetTitle() != "Add avatar upload" {
					t.Errorf("create_session did not derive title from prompt: %q", req.GetTitle())
				}
				return &pb.Session{Id: "sess-cs-derived"}, nil
			}},
			sentinel: "sess-cs-derived",
		},
		{
			tool: "stop_session",
			args: map[string]any{"id": "s1"},
			backend: &fakeBackend{stopSession: func(_ context.Context, id string) (*pb.Session, error) {
				return &pb.Session{Id: "sess-stop-" + id}, nil
			}},
			sentinel: "sess-stop-s1",
		},
		{
			tool: "pause_session",
			args: map[string]any{"id": "s1"},
			backend: &fakeBackend{pauseSession: func(_ context.Context, id string) (*pb.Session, error) {
				return &pb.Session{Id: "sess-pause-" + id}, nil
			}},
			sentinel: "sess-pause-s1",
		},
		{
			tool: "resume_session",
			args: map[string]any{"id": "s1"},
			backend: &fakeBackend{resumeSession: func(_ context.Context, id string) (*pb.Session, error) {
				return &pb.Session{Id: "sess-resume-" + id}, nil
			}},
			sentinel: "sess-resume-s1",
		},
		{
			tool: "retry_session",
			args: map[string]any{"id": "s1"},
			backend: &fakeBackend{retrySession: func(_ context.Context, id string) (*pb.Session, error) {
				return &pb.Session{Id: "sess-retry-" + id}, nil
			}},
			sentinel: "sess-retry-s1",
		},
		{
			tool: "update_session",
			args: map[string]any{"id": "s1", "title": "newtitle"},
			backend: &fakeBackend{updateSession: func(_ context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
				if req.GetId() != "s1" || req.GetTitle() != "newtitle" {
					t.Errorf("update_session args not forwarded: %+v", req)
				}
				return &pb.Session{Id: "sess-us"}, nil
			}},
			sentinel: "sess-us",
		},
		{
			tool: "update_session",
			args: map[string]any{"id": "s1", "tracker_url": "https://linear.app/x/issue/BOS-9", "tracker_id": "BOS-9"},
			backend: &fakeBackend{updateSession: func(_ context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
				if req.GetId() != "s1" || req.GetTrackerUrl() != "https://linear.app/x/issue/BOS-9" || req.GetTrackerId() != "BOS-9" {
					t.Errorf("update_session tracker args not forwarded: %+v", req)
				}
				if req.Title != nil {
					t.Errorf("update_session title should be nil when omitted, got %q", req.GetTitle())
				}
				return &pb.Session{Id: "sess-us-tracker"}, nil
			}},
			sentinel: "sess-us-tracker",
		},
		{
			tool: "link_session_pr",
			args: map[string]any{"session_id": "s1", "pr": "42"},
			backend: &fakeBackend{linkSessionPR: func(_ context.Context, id, pr string) (*pb.Session, error) {
				if id != "s1" || pr != "42" {
					t.Errorf("link_session_pr args not forwarded: %q %q", id, pr)
				}
				return &pb.Session{Id: "sess-lsp"}, nil
			}},
			sentinel: "sess-lsp",
		},
		{
			tool: "record_chat",
			args: map[string]any{"session_id": "s1", "agent_session_id": "a1", "title": "T", "agent_name": "claude", "resume": true},
			backend: &fakeBackend{recordChat: func(_ context.Context, sid, aid, title, agent string, resume bool) (*pb.ClaudeChat, error) {
				if sid != "s1" || aid != "a1" || title != "T" || agent != "claude" || !resume {
					t.Errorf("record_chat args not forwarded: %q %q %q %q %v", sid, aid, title, agent, resume)
				}
				return &pb.ClaudeChat{Id: "chat-rc"}, nil
			}},
			sentinel: "chat-rc",
		},
		{
			tool: "update_chat_title",
			args: map[string]any{"agent_session_id": "a1", "title": "newtitle"},
			backend: &fakeBackend{updateChatTitle: func(_ context.Context, aid, title string) error {
				if aid != "a1" || title != "newtitle" {
					t.Errorf("update_chat_title args not forwarded: %q %q", aid, title)
				}
				return nil
			}},
			sentinel: "a1",
		},
		{
			tool: "wake_chat",
			args: map[string]any{"session_id": "s1", "agent_session_id": "a1", "force_fresh": true},
			backend: &fakeBackend{wakeChat: func(_ context.Context, sid, aid string, fresh bool) (*pb.WakeChatResponse, error) {
				if sid != "s1" || aid != "a1" || !fresh {
					t.Errorf("wake_chat args not forwarded: %q %q %v", sid, aid, fresh)
				}
				return &pb.WakeChatResponse{TmuxSessionName: "tmux-wc"}, nil
			}},
			sentinel: "tmux-wc",
		},
		{
			tool: "report_chat_status",
			args: map[string]any{"agent_session_id": "a1", "status": "CHAT_STATUS_WORKING"},
			backend: &fakeBackend{reportChatStatus: func(_ context.Context, statuses []*pb.ChatStatusReport) error {
				if len(statuses) != 1 || statuses[0].GetAgentSessionId() != "a1" || statuses[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_WORKING {
					t.Errorf("report_chat_status args not forwarded: %+v", statuses)
				}
				return nil
			}},
			sentinel: "a1",
		},
		{
			tool: "create_cron_job",
			args: map[string]any{"repo_id": "r1", "name": "nightly", "prompt": "p", "schedule": "0 0 * * *", "enabled": true},
			backend: &fakeBackend{createCronJob: func(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
				if req.GetRepoId() != "r1" || req.GetName() != "nightly" || req.GetSchedule() != "0 0 * * *" || !req.GetEnabled() {
					t.Errorf("create_cron_job args not forwarded: %+v", req)
				}
				return &pb.CronJob{Id: "cron-ccj"}, nil
			}},
			sentinel: "cron-ccj",
		},
		{
			tool: "update_cron_job",
			args: map[string]any{"id": "c1", "name": "renamed", "enabled": false},
			backend: &fakeBackend{updateCronJob: func(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
				if req.GetId() != "c1" || req.GetName() != "renamed" || req.GetEnabled() {
					t.Errorf("update_cron_job args not forwarded: %+v", req)
				}
				return &pb.CronJob{Id: "cron-ucj"}, nil
			}},
			sentinel: "cron-ucj",
		},
		{
			tool: "run_cron_job_now",
			args: map[string]any{"id": "c1"},
			backend: &fakeBackend{runCronJobNow: func(_ context.Context, id string) (*pb.RunCronJobNowResponse, error) {
				if id != "c1" {
					t.Errorf("run_cron_job_now id not forwarded: %q", id)
				}
				return &pb.RunCronJobNowResponse{SkippedReason: "skipped-rcjn"}, nil
			}},
			sentinel: "skipped-rcjn",
		},
		{
			tool: "send_chat_message",
			args: map[string]any{"agent_session_id": "agent-scm", "message": "hello bot", "wake_if_asleep": true},
			backend: &fakeBackend{sendChatMessage: func(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
				if req.GetAgentSessionId() != "agent-scm" || req.GetMessage() != "hello bot" || !req.GetWakeIfAsleep() {
					t.Errorf("send_chat_message args not forwarded: %+v", req)
				}
				return &pb.SendChatMessageResponse{TmuxSessionName: "tmux-scm", Delivered: true}, nil
			}},
			sentinel: "tmux-scm",
		},
		{
			tool: "send_chat_message",
			args: map[string]any{"agent_session_id": "agent-default", "message": "wake me"},
			backend: &fakeBackend{sendChatMessage: func(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
				if req.GetAgentSessionId() != "agent-default" || req.GetMessage() != "wake me" || !req.GetWakeIfAsleep() {
					t.Errorf("send_chat_message default wake_if_asleep not forwarded: %+v", req)
				}
				return &pb.SendChatMessageResponse{TmuxSessionName: "tmux-default", Delivered: true}, nil
			}},
			sentinel: "tmux-default",
		},
		{
			tool: "send_chat_message",
			args: map[string]any{"agent_session_id": "agent-no-wake", "message": "do not wake", "wake_if_asleep": false},
			backend: &fakeBackend{sendChatMessage: func(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
				if req.GetAgentSessionId() != "agent-no-wake" || req.GetMessage() != "do not wake" || req.GetWakeIfAsleep() {
					t.Errorf("send_chat_message explicit false wake_if_asleep not forwarded: %+v", req)
				}
				return &pb.SendChatMessageResponse{TmuxSessionName: "tmux-no-wake", Delivered: true}, nil
			}},
			sentinel: "tmux-no-wake",
		},
		{
			tool: "create_cron_job",
			args: map[string]any{"repo_id": "r1", "name": "nightly", "prompt": "p", "schedule": "0 0 * * *", "model": "opus", "gate_command": "node gate.mjs", "run_setup_command": true},
			backend: &fakeBackend{createCronJob: func(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
				if req.GetModel() != "opus" || req.GetGateCommand() != "node gate.mjs" || !req.GetRunSetupCommand() {
					t.Errorf("create_cron_job new fields not forwarded: %+v", req)
				}
				return &pb.CronJob{Id: "cron-ccj-full"}, nil
			}},
			sentinel: "cron-ccj-full",
		},
		{
			tool: "update_cron_job",
			args: map[string]any{"id": "c1", "model": "sonnet", "gate_command": "g", "run_setup_command": false},
			backend: &fakeBackend{updateCronJob: func(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
				if req.GetModel() != "sonnet" || req.GetGateCommand() != "g" || req.RunSetupCommand == nil || req.GetRunSetupCommand() {
					t.Errorf("update_cron_job new fields not forwarded: %+v", req)
				}
				return &pb.CronJob{Id: "cron-ucj-full"}, nil
			}},
			sentinel: "cron-ucj-full",
		},
		{
			tool: "update_repo",
			args: map[string]any{"id": "r1", "linear_api_key": "lin_secret", "sentry_api_key": "sen_secret", "sentry_org": "myorg"},
			backend: &fakeBackend{updateRepo: func(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
				if req.GetLinearApiKey() != "lin_secret" || req.GetSentryApiKey() != "sen_secret" || req.GetSentryOrg() != "myorg" {
					t.Errorf("update_repo secrets not forwarded: %+v", req)
				}
				return &pb.Repo{Id: "repo-ur-secrets"}, nil
			}},
			sentinel: "repo-ur-secrets",
		},
		{
			tool: "create_session",
			args: map[string]any{
				"repo_id": "r1", "prompt": "do it", "title": "T",
				"base_branch": "develop", "branch_name": "feat/x", "force_branch": true,
				"quick_chat": true, "pr_number": 42,
				"tracker_id": "BOS-1", "tracker_url": "https://linear.app/x", "tracker_source": "linear",
			},
			backend: &fakeBackend{createSession: func(_ context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
				if req.GetBaseBranch() != "develop" || req.GetBranchName() != "feat/x" || !req.GetForceBranch() ||
					!req.GetQuickChat() || req.GetPrNumber() != 42 || req.GetTrackerId() != "BOS-1" ||
					req.GetTrackerUrl() != "https://linear.app/x" || req.GetTrackerSource() != "linear" {
					t.Errorf("create_session full field set not forwarded: %+v", req)
				}
				return &pb.Session{Id: "sess-cs-full"}, nil
			}},
			sentinel: "sess-cs-full",
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			cs := newConnectedClient(t, tc.backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("call %s: %v", tc.tool, err)
			}
			if res.IsError {
				t.Fatalf("%s returned error result: %s", tc.tool, textOf(t, res))
			}
			if got := textOf(t, res); !strings.Contains(got, tc.sentinel) {
				t.Fatalf("%s result missing sentinel %q: %s", tc.tool, tc.sentinel, got)
			}
		})
	}
}

// TestUpdateCronJobOmitRunSetupLeavesUnset proves that omitting run_setup_command
// leaves the proto optional field nil (not an explicit false), so a present-only
// update never clobbers the daemon's stored value.
func TestUpdateCronJobOmitRunSetupLeavesUnset(t *testing.T) {
	var captured *pb.UpdateCronJobRequest
	backend := &fakeBackend{updateCronJob: func(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
		captured = req
		return &pb.CronJob{Id: "c1"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_cron_job", Arguments: map[string]any{"id": "c1", "name": "x"}})
	if err != nil {
		t.Fatalf("call update_cron_job: %v", err)
	}
	if res.IsError {
		t.Fatalf("update_cron_job error: %s", textOf(t, res))
	}
	if captured.RunSetupCommand != nil {
		t.Errorf("omitted run_setup_command should leave proto field nil, got %v", *captured.RunSetupCommand)
	}
}

// TestCreateCronJobOmitRunSetupLeavesUnset is the create-side analogue: omitting
// run_setup_command leaves the optional proto field nil so the server applies its
// own default rather than seeing an explicit false.
func TestCreateCronJobOmitRunSetupLeavesUnset(t *testing.T) {
	var captured *pb.CreateCronJobRequest
	backend := &fakeBackend{createCronJob: func(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
		captured = req
		return &pb.CronJob{Id: "c1"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_cron_job", Arguments: map[string]any{"repo_id": "r1", "name": "n", "prompt": "p", "schedule": "0 0 * * *"}})
	if err != nil {
		t.Fatalf("call create_cron_job: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_cron_job error: %s", textOf(t, res))
	}
	if captured.RunSetupCommand != nil {
		t.Errorf("omitted run_setup_command should leave proto field nil, got %v", *captured.RunSetupCommand)
	}
}

// TestUpdateRepoRedactsSecretsInResult proves update_repo never echoes a repo's
// raw API keys back in its result, while still returning non-secret fields.
func TestUpdateRepoRedactsSecretsInResult(t *testing.T) {
	backend := &fakeBackend{updateRepo: func(_ context.Context, _ *pb.UpdateRepoRequest) (*pb.Repo, error) {
		return &pb.Repo{Id: "r1", LinearApiKey: "lin_topsecret", SentryApiKey: "sen_topsecret", SentryOrg: "acme"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_repo", Arguments: map[string]any{"id": "r1"}})
	if err != nil {
		t.Fatalf("call update_repo: %v", err)
	}
	out := textOf(t, res)
	if strings.Contains(out, "lin_topsecret") || strings.Contains(out, "sen_topsecret") {
		t.Errorf("update_repo leaked a raw API key in its result: %s", out)
	}
	if !strings.Contains(out, "acme") {
		t.Errorf("update_repo should still return non-secret sentry_org; got: %s", out)
	}
}

// TestListReposRedactsSecretsInResult proves the list_repos read tool never echoes
// a repo's raw API keys.
func TestListReposRedactsSecretsInResult(t *testing.T) {
	backend := &fakeBackend{listRepos: func(_ context.Context) ([]*pb.Repo, error) {
		return []*pb.Repo{{Id: "r1", LinearApiKey: "lin_topsecret", SentryApiKey: "sen_topsecret", SentryOrg: "acme"}}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_repos", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call list_repos: %v", err)
	}
	out := textOf(t, res)
	if strings.Contains(out, "lin_topsecret") || strings.Contains(out, "sen_topsecret") {
		t.Errorf("list_repos leaked a raw API key in its result: %s", out)
	}
}

// TestResolveContextRedactsSecretsInResult proves the resolve_context read tool,
// which returns a response embedding a *pb.Repo, never echoes the repo's raw API
// keys.
func TestResolveContextRedactsSecretsInResult(t *testing.T) {
	backend := &fakeBackend{resolveContext: func(_ context.Context, _ string) (*pb.ResolveContextResponse, error) {
		return &pb.ResolveContextResponse{
			Repo: &pb.Repo{Id: "r1", LinearApiKey: "lin_topsecret", SentryApiKey: "sen_topsecret", SentryOrg: "acme"},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "resolve_context", Arguments: map[string]any{"working_dir": "/code/x"}})
	if err != nil {
		t.Fatalf("call resolve_context: %v", err)
	}
	out := textOf(t, res)
	if strings.Contains(out, "lin_topsecret") || strings.Contains(out, "sen_topsecret") {
		t.Errorf("resolve_context leaked a raw API key in its result: %s", out)
	}
	if !strings.Contains(out, "acme") {
		t.Errorf("resolve_context should still return non-secret sentry_org; got: %s", out)
	}
}

// TestRedactRepoDoesNotMutateSource proves redactRepo clears only the secret
// fields, preserves the rest, and returns a copy without mutating its input.
func TestRedactRepoDoesNotMutateSource(t *testing.T) {
	src := &pb.Repo{Id: "r1", LinearApiKey: "lin", SentryApiKey: "sen", SentryOrg: "org"}
	got := redactRepo(src)
	if src.GetLinearApiKey() != "lin" || src.GetSentryApiKey() != "sen" {
		t.Errorf("redactRepo mutated its source: %+v", src)
	}
	if got.GetLinearApiKey() != "" || got.GetSentryApiKey() != "" {
		t.Errorf("redactRepo did not clear secrets: %+v", got)
	}
	if got.GetSentryOrg() != "org" || got.GetId() != "r1" {
		t.Errorf("redactRepo dropped non-secret fields: %+v", got)
	}
	if redactRepo(nil) != nil {
		t.Error("redactRepo(nil) should return nil")
	}
}

func TestDeriveSessionTitle(t *testing.T) {
	long := strings.Repeat("a", 100)
	cases := []struct {
		name   string
		title  string
		prompt string
		want   string
	}{
		{"explicit title wins", "My Title", "ignored prompt", "My Title"},
		{"explicit title trimmed", "  spaced  ", "x", "spaced"},
		{"derive first line", "", "First line\nsecond line", "First line"},
		{"derive trims whitespace", "", "  hello world  \nmore", "hello world"},
		{"derive skips leading blank lines", "", "\n\n  real title\nbody", "real title"},
		{"derive truncates long line", "", long, strings.Repeat("a", maxDerivedTitleLen)},
		{"empty prompt yields empty", "", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSessionTitle(tc.title, tc.prompt); got != tc.want {
				t.Errorf("deriveSessionTitle(%q, %q) = %q, want %q", tc.title, tc.prompt, got, tc.want)
			}
		})
	}
}
