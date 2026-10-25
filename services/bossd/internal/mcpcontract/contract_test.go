package mcpcontract

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/bossmcp"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossd/testharness"
)

// captureBackend records the protobuf request each request-shaped mutating tool
// builds, so the contract test can replay that exact request against a real
// bossd daemon. It embeds a nil bossmcp.Backend: any method the contract test
// does not exercise panics if called, keeping the test scope honest.
type captureBackend struct {
	bossmcp.Backend
	registerRepo  *pb.RegisterRepoRequest
	cloneRepo     *pb.CloneAndRegisterRepoRequest
	createSess    *pb.CreateSessionRequest
	createCron    *pb.CreateCronJobRequest
	sendChat      *pb.SendChatMessageRequest
	addAccount    *pb.AddAgentAccountRequest
	updateAccount *pb.UpdateAgentAccountRequest
	createNote    *pb.CreateNoteRequest
	updateNote    *pb.UpdateNoteRequest
}

func (b *captureBackend) RegisterRepo(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	b.registerRepo = req
	return &pb.Repo{Id: "captured"}, nil
}

func (b *captureBackend) CloneAndRegisterRepo(_ context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	b.cloneRepo = req
	return &pb.Repo{Id: "captured"}, nil
}

func (b *captureBackend) CreateSession(_ context.Context, req *pb.CreateSessionRequest) (*bossmcp.CreateSessionResult, error) {
	b.createSess = req
	return &bossmcp.CreateSessionResult{Session: &pb.Session{Id: "captured"}}, nil
}

func (b *captureBackend) CreateCronJob(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	b.createCron = req
	return &pb.CronJob{Id: "captured"}, nil
}

func (b *captureBackend) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	b.sendChat = req
	return &pb.SendChatMessageResponse{}, nil
}

func (b *captureBackend) AddAgentAccount(_ context.Context, req *pb.AddAgentAccountRequest) (*pb.AgentAccount, error) {
	b.addAccount = req
	return &pb.AgentAccount{Id: "captured"}, nil
}

func (b *captureBackend) UpdateAgentAccount(_ context.Context, req *pb.UpdateAgentAccountRequest) (*pb.AgentAccount, error) {
	b.updateAccount = req
	return &pb.AgentAccount{Id: "captured"}, nil
}

func (b *captureBackend) CreateNote(_ context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	b.createNote = req
	return &pb.Note{Id: "captured"}, nil
}

// UpdateNote drops repoID deliberately: it is a hosted-gateway routing key that
// never reaches the daemon, and this test replays the DAEMON request.
func (b *captureBackend) UpdateNote(_ context.Context, _ string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	b.updateNote = req
	return &pb.Note{Id: "captured"}, nil
}

// callTool drives a single MCP tool over an in-memory transport against the
// given backend, returning the backend (post-capture) and failing the test on
// any transport- or tool-level error.
func callTool(t *testing.T, backend bossmcp.Backend, tool string, args map[string]any) {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "bossanova", Version: "test"}, nil)
	bossmcp.RegisterTools(server, backend, bossmcp.Options{})

	client := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "0.0.0"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer cs.Close() //nolint:errcheck // test cleanup

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: transport error: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("CallTool %s: tool error: %s", tool, toolText(res))
	}
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// assertNotMissingRequired fails when bossd rejected the replayed request for a
// missing required field. Errors past validation (NotFound for a bogus id, a
// clone failure, etc.) are expected and ignored — the contract only asserts
// the tool supplied every field bossd requires.
func assertNotMissingRequired(t *testing.T, tool string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if connect.CodeOf(err) == connect.CodeInvalidArgument && strings.Contains(err.Error(), "required") {
		t.Fatalf("%s: tool built a request bossd rejected as incomplete: %v", tool, err)
	}
}

// TestMCPCreateToolsSatisfyDaemonValidation drives every request-shaped mutating
// tool (the ones that map a struct of args into a multi-field protobuf request,
// where a dropped required field hides silently) and replays the captured
// request against a real bossd daemon, asserting bossd never rejects it for a
// missing required field.
func TestMCPCreateToolsSatisfyDaemonValidation(t *testing.T) {
	// The in-process daemon resolves an omitted repo worktree base from the
	// global settings, so seed a known value and keep this test off the
	// developer's real settings file.
	seededBase := filepath.Join(t.TempDir(), "global-worktrees")
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	seed := config.DefaultSettings()
	seed.WorktreeBaseDir = seededBase
	if err := config.SaveTo(settingsPath, seed); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	h := testharness.New(t)
	ctx := context.Background()

	t.Run("register_repo", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "register_repo", map[string]any{
			"local_path":          testharness.TempRepoDir(t),
			"name":                "contract-repo",
			"default_base_branch": "main",
		})
		if got := backend.registerRepo.GetWorktreeBaseDir(); got != "" {
			t.Fatalf("register_repo sent worktree_base_dir %q, want none (the daemon default is under test)", got)
		}
		resp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(backend.registerRepo))
		assertNotMissingRequired(t, "register_repo", err)
		if err != nil {
			t.Fatalf("register_repo: daemon rejected the MCP-built request: %v", err)
		}
		// BOS-1344: an MCP registration with no base must still yield a repo that
		// can create sessions, i.e. one carrying the global default.
		if got := resp.Msg.GetRepo().GetWorktreeBaseDir(); got != seededBase {
			t.Fatalf("registered repo worktree_base_dir = %q, want seeded global default %q", got, seededBase)
		}
	})

	t.Run("clone_and_register_repo", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "clone_and_register_repo", map[string]any{
			"clone_url":  "file:///nonexistent/bossanova-contract.git",
			"local_path": filepath.Join(t.TempDir(), "dest"),
			"name":       "contract-clone",
		})
		_, err := h.Client.CloneAndRegisterRepo(ctx, connect.NewRequest(backend.cloneRepo))
		assertNotMissingRequired(t, "clone_and_register_repo", err)
	})

	t.Run("create_session", func(t *testing.T) {
		backend := &captureBackend{}
		// Title omitted on purpose: the tool must derive one from the prompt,
		// or bossd rejects the request with "title is required".
		callTool(t, backend, "create_session", map[string]any{
			"repo_id": "nonexistent-repo",
			"prompt":  "Add avatar upload to the profile page",
		})
		stream, err := h.Client.CreateSession(ctx, connect.NewRequest(backend.createSess))
		if err == nil {
			for stream.Receive() {
				_ = stream.Msg()
			}
			err = stream.Err()
			_ = stream.Close()
		}
		assertNotMissingRequired(t, "create_session", err)
	})

	t.Run("create_cron_job", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "create_cron_job", map[string]any{
			"repo_id":  "nonexistent-repo",
			"name":     "nightly",
			"prompt":   "run the nightly job",
			"schedule": "0 0 * * *",
			"enabled":  false,
		})
		_, err := h.Client.CreateCronJob(ctx, connect.NewRequest(backend.createCron))
		assertNotMissingRequired(t, "create_cron_job", err)
	})

	t.Run("send_chat_message", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "send_chat_message", map[string]any{
			"agent_session_id": "nonexistent-chat",
			"message":          "continue please",
		})
		_, err := h.Client.SendChatMessage(ctx, connect.NewRequest(backend.sendChat))
		assertNotMissingRequired(t, "send_chat_message", err)
	})

	t.Run("add_account", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "add_account", map[string]any{
			"provider":   "claude",
			"label":      "contract-primary",
			"credential": "contract-token",
		})
		_, err := h.Client.AddAgentAccount(ctx, connect.NewRequest(backend.addAccount))
		assertNotMissingRequired(t, "add_account", err)
	})

	t.Run("update_account", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "update_account", map[string]any{
			"id":    "nonexistent-account",
			"label": "renamed",
		})
		_, err := h.Client.UpdateAgentAccount(ctx, connect.NewRequest(backend.updateAccount))
		assertNotMissingRequired(t, "update_account", err)
	})

	t.Run("create_note", func(t *testing.T) {
		backend := &captureBackend{}
		// Provenance omitted on purpose: session_id/chat_id are optional, and
		// the tool must leave them UNSET rather than sending blank strings.
		callTool(t, backend, "create_note", map[string]any{
			"repo_id": "nonexistent-repo",
			"body":    "the CI poller retries three times before giving up",
			"tags":    []any{"ci", "flake"},
		})
		// Asserted here, not inferred from the replay: bossd's optionalTrimmed
		// maps a blank string to SQL NULL, so a tool regressing to
		// SessionId: proto.String("") would be ACCEPTED and the replay below
		// would still pass. Only the captured request shows the difference.
		if backend.createNote.SessionId != nil || backend.createNote.ChatId != nil {
			t.Errorf("create_note sent provenance %v/%v, want both unset when omitted", backend.createNote.SessionId, backend.createNote.ChatId)
		}
		_, err := h.Client.CreateNote(ctx, connect.NewRequest(backend.createNote))
		assertNotMissingRequired(t, "create_note", err)
	})

	t.Run("update_note", func(t *testing.T) {
		backend := &captureBackend{}
		// Body supplied without tags: the tri-state tags pointer must stay
		// unset, which is what tells the daemon to leave the stored tags alone.
		callTool(t, backend, "update_note", map[string]any{
			"repo_id": "nonexistent-repo",
			"id":      "nonexistent-note",
			"body":    "revised after re-reading poller.go",
		})
		// Also asserted on the captured request rather than the replay: a
		// set-but-empty NoteTagSet means "clear every tag", and the daemon
		// accepts it happily — so a body-only edit that wrongly sent one would
		// wipe the note's tags without any RPC error to notice.
		if backend.updateNote.Tags != nil {
			t.Errorf("update_note sent tags %v, want nil: an omitted tags argument must leave the stored tags alone", backend.updateNote.GetTags().GetTags())
		}
		_, err := h.Client.UpdateNote(ctx, connect.NewRequest(backend.updateNote))
		assertNotMissingRequired(t, "update_note", err)
	})
}

// projectForwardBackend forwards the handful of local methods the project
// config tools call to a real bossd over the harness client, counts every
// write, and implements bossmcp.ProjectLocalBackend with a fixed identity and
// branch. Like captureBackend it embeds a nil bossmcp.Backend, so any other
// method panics if called.
type projectForwardBackend struct {
	bossmcp.Backend
	client interface {
		ResolveContext(context.Context, *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error)
		ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error)
		ValidateRepoPath(context.Context, *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error)
		RegisterRepo(context.Context, *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error)
		UpdateRepo(context.Context, *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error)
		ListCronJobs(context.Context, *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error)
		CreateCronJob(context.Context, *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error)
		UpdateCronJob(context.Context, *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error)
		DeleteCronJob(context.Context, *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error)
	}
	writes int
}

var _ bossmcp.ProjectLocalBackend = (*projectForwardBackend)(nil)

func (b *projectForwardBackend) ProjectDaemonIdentity(context.Context) (projectconfig.DaemonIdentity, string, error) {
	return projectconfig.DaemonIdentity{ID: "d-contract", DisplayName: "contract-daemon"}, "", nil
}

func (b *projectForwardBackend) CheckoutBranch(context.Context, string) (string, bool) {
	return "main", true
}

func (b *projectForwardBackend) ResolveContext(ctx context.Context, dir string) (*pb.ResolveContextResponse, error) {
	resp, err := b.client.ResolveContext(ctx, connect.NewRequest(&pb.ResolveContextRequest{WorkingDirectory: dir}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *projectForwardBackend) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := b.client.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepos(), nil
}

func (b *projectForwardBackend) ValidateRepoPath(ctx context.Context, p string) (*pb.ValidateRepoPathResponse, error) {
	resp, err := b.client.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{LocalPath: p}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *projectForwardBackend) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	b.writes++
	resp, err := b.client.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *projectForwardBackend) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	b.writes++
	resp, err := b.client.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *projectForwardBackend) ListCronJobs(ctx context.Context) ([]*pb.CronJob, error) {
	resp, err := b.client.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJobs(), nil
}

func (b *projectForwardBackend) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	b.writes++
	resp, err := b.client.CreateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *projectForwardBackend) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	b.writes++
	resp, err := b.client.UpdateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *projectForwardBackend) DeleteCronJob(ctx context.Context, id string) error {
	b.writes++
	_, err := b.client.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id}))
	return err
}

// callProjectTool drives one project config tool and decodes its result.
func callProjectTool(t *testing.T, backend bossmcp.Backend, tool string, args map[string]any) (plan projectconfig.Plan, converged, applied bool, errText string) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "bossanova", Version: "test"}, nil)
	bossmcp.RegisterTools(server, backend, bossmcp.Options{})
	client := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "0.0.0"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer cs.Close() //nolint:errcheck // test cleanup
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: transport error: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("CallTool %s: tool error: %s", tool, toolText(res))
	}
	var out struct {
		Plan      projectconfig.Plan `json:"plan"`
		Converged bool               `json:"converged"`
		Applied   *bool              `json:"applied"`
		Error     string             `json:"error"`
	}
	if err := json.Unmarshal([]byte(toolText(res)), &out); err != nil {
		t.Fatalf("%s result: %v\n%s", tool, err, toolText(res))
	}
	return out.Plan, out.Converged, out.Applied != nil && *out.Applied, out.Error
}

// TestMCPApplyProjectConfigAgainstDaemon applies a project config through
// apply_project_config against a real bossd: every request the engine builds
// must pass the daemon's validation, the echoed cron must carry the project
// marker, and the next diff must be converged and the next apply write nothing.
func TestMCPApplyProjectConfigAgainstDaemon(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	seed := config.DefaultSettings()
	seed.WorktreeBaseDir = filepath.Join(t.TempDir(), "global-worktrees")
	if err := config.SaveTo(settingsPath, seed); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	h := testharness.New(t)
	ctx := context.Background()

	repoDir, err := filepath.EvalSymlinks(testharness.TempRepoDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"version": 1, "project": "contract",
		// The harness's git reports this origin for every checkout.
		"repo":    map[string]any{"originUrl": "https://github.com/test/repo"},
		"daemons": []any{map[string]any{"name": "contract-daemon", "daemonId": "d-contract", "path": repoDir}},
		// Disabled: the harness daemon runs no cron scheduler to load an
		// enabled job into (create_cron_job's contract case does the same).
		"crons": []any{map[string]any{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "enabled": false}},
	}
	backend := &projectForwardBackend{client: h.Client}
	args := map[string]any{"config": cfg, "working_dir": repoDir, "dry_run": false}

	plan, _, applied, errText := callProjectTool(t, backend, "apply_project_config", args)
	if !applied || errText != "" {
		t.Fatalf("apply: applied=%v error=%q\n%s", applied, errText, plan.Text())
	}
	if backend.writes != 2 {
		t.Fatalf("apply issued %d writes, want the registration and the cron create", backend.writes)
	}
	jobs, err := h.Client.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := jobs.Msg.GetCronJobs(); len(got) != 1 || got[0].GetName() != "nightly" || got[0].GetManagedBy() != "boss-project:contract" {
		t.Fatalf("daemon cron jobs %v, want nightly marked boss-project:contract", got)
	}

	if plan, converged, _, _ := callProjectTool(t, backend, "diff_project_config", map[string]any{"config": cfg, "working_dir": repoDir}); !converged {
		t.Fatalf("diff after apply is not converged:\n%s", plan.Text())
	}
	before := backend.writes
	if _, _, applied, errText := callProjectTool(t, backend, "apply_project_config", args); !applied || errText != "" || backend.writes != before {
		t.Fatalf("second apply: applied=%v error=%q writes=%d", applied, errText, backend.writes-before)
	}
}
