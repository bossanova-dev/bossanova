package plugin_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/vcs"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
)

// testVCSProvider returns canned responses for the integration test.
type testVCSProvider struct {
	prs    []vcs.PRSummary
	checks map[int][]vcs.CheckResult
	status map[int]*vcs.PRStatus
}

func (p *testVCSProvider) CreateDraftPR(_ context.Context, _ vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, nil
}
func (p *testVCSProvider) GetPRStatus(_ context.Context, _ string, prID int) (*vcs.PRStatus, error) {
	if s, ok := p.status[prID]; ok {
		return s, nil
	}
	return &vcs.PRStatus{}, nil
}
func (p *testVCSProvider) GetCheckResults(_ context.Context, _ string, prID int) ([]vcs.CheckResult, error) {
	return p.checks[prID], nil
}
func (p *testVCSProvider) GetFailedCheckLogs(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}
func (p *testVCSProvider) MarkReadyForReview(_ context.Context, _ string, _ int) error { return nil }
func (p *testVCSProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (p *testVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return p.prs, nil
}
func (p *testVCSProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *testVCSProvider) MergePR(_ context.Context, _ string, _ int, _ string) error { return nil }
func (p *testVCSProvider) UpdatePRTitle(_ context.Context, _ string, _ int, _ string) error {
	return nil
}

// taskSourceWithBroker is a host-side GRPCPlugin that registers the HostService
// on broker ID 1, which the plugin binary expects when it calls broker.Dial(1).
type taskSourceWithBroker struct {
	goplugin.NetRPCUnsupportedPlugin
	hostService *pluginpkg.HostServiceServer
}

func (p *taskSourceWithBroker) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return nil
}

func (p *taskSourceWithBroker) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	// Start a gRPC server on broker ID 1 with HostService registered.
	// The plugin binary will broker.Dial(1) to connect to this.
	serverFunc := func(opts []grpc.ServerOption) *grpc.Server {
		srv := grpc.NewServer(opts...)
		p.hostService.Register(srv)
		return srv
	}
	go broker.AcceptAndServe(1, serverFunc)

	return &taskSourceGRPCClientWrapper{conn: conn}, nil
}

// taskSourceGRPCClientWrapper implements pluginpkg.TaskSource using raw gRPC Invoke,
// matching the production taskSourceGRPCClient in grpc_plugins.go.
type taskSourceGRPCClientWrapper struct {
	conn *grpc.ClientConn
}

func (c *taskSourceGRPCClientWrapper) GetInfo(ctx context.Context) (*bossanovav1.PluginInfo, error) {
	resp := &bossanovav1.TaskSourceServiceGetInfoResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/GetInfo", &bossanovav1.TaskSourceServiceGetInfoRequest{}, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetInfo(), nil
}

func (c *taskSourceGRPCClientWrapper) PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error) {
	req := &bossanovav1.PollTasksRequest{}
	if repoOriginURL != "" {
		req.RepoOriginUrl = &repoOriginURL
	}
	resp := &bossanovav1.PollTasksResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/PollTasks", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetTasks(), nil
}

func (c *taskSourceGRPCClientWrapper) UpdateTaskStatus(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error {
	req := &bossanovav1.UpdateTaskStatusRequest{
		ExternalId: externalID,
		Status:     status,
		Details:    details,
	}
	resp := &bossanovav1.UpdateTaskStatusResponse{}
	return c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/UpdateTaskStatus", req, resp)
}

func (c *taskSourceGRPCClientWrapper) ListAvailableIssues(ctx context.Context, repoOriginURL string, config map[string]string) ([]*bossanovav1.TrackerIssue, error) {
	req := &bossanovav1.ListAvailableIssuesRequest{
		RepoOriginUrl: repoOriginURL,
		Config:        config,
	}
	resp := &bossanovav1.ListAvailableIssuesResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.TaskSourceService/ListAvailableIssues", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetIssues(), nil
}

func buildPluginBinary(t *testing.T) string {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "bossd-plugin-dependabot")
	cmd := exec.Command("go", "build", "-o", binPath, "./plugins/bossd-plugin-dependabot")
	// Build from the workspace root so go.work resolves bossalib.
	cmd.Dir = filepath.Join("..", "..", "..", "..")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build plugin binary: %v", err)
	}
	return binPath
}

// TestIntegration_NewPluginMapBrokerDial verifies that the production
// NewPluginMap correctly registers HostService on broker ID 1 for
// TaskSourceGRPCPlugin. Without this, the dependabot plugin's
// broker.Dial(1) will timeout because no ConnInfo is sent.
//
// This is a regression test for the bug where HostService was
// accidentally removed from TaskSourceGRPCPlugin, breaking the
// plugin's ability to call back into the host for VCS data.
func TestIntegration_NewPluginMapBrokerDial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	binPath := buildPluginBinary(t)

	success := vcs.CheckConclusionSuccess
	mergeable := true

	provider := &testVCSProvider{
		prs: []vcs.PRSummary{
			{
				Number:     1,
				Title:      "Bump foo from 1.0 to 2.0",
				HeadBranch: "dependabot/npm_and_yarn/foo-2.0",
				State:      vcs.PRStateOpen,
				Author:     "app/dependabot",
			},
		},
		checks: map[int][]vcs.CheckResult{
			1: {{ID: "ci", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
		},
		status: map[int]*vcs.PRStatus{
			1: {State: vcs.PRStateOpen, Mergeable: &mergeable},
		},
	}

	hostService := pluginpkg.NewHostServiceServer(provider)

	// Use ONLY the production TaskSourceGRPCPlugin — no WorkflowService.
	// This isolates the test: if TaskSourceGRPCPlugin doesn't call
	// AcceptAndServe(1), there's no other plugin to send ConnInfo and
	// the dependabot plugin's broker.Dial(1) will timeout.
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource: pluginpkg.NewTaskSourceGRPCPlugin(hostService),
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: pluginpkg.Handshake,
		Plugins:         pluginMap,
		Cmd:             exec.Command(binPath),
		AllowedProtocols: []goplugin.Protocol{
			goplugin.ProtocolGRPC,
		},
		Logger: hclog.NewNullLogger(),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}

	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
	if err != nil {
		t.Fatalf("dispense TaskSource: %v", err)
	}
	taskSource, ok := raw.(pluginpkg.TaskSource)
	if !ok {
		t.Fatalf("dispensed type %T does not implement TaskSource", raw)
	}

	// PollTasks with a non-empty URL forces the plugin to call back to
	// the host via broker.Dial(1). If AcceptAndServe(1) was not called,
	// this will fail with "timeout waiting for connection info".
	ctx := context.Background()
	tasks, err := taskSource.PollTasks(ctx, "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("PollTasks via production NewPluginMap failed: %v\n"+
			"This likely means TaskSourceGRPCPlugin is missing AcceptAndServe(1) "+
			"for the HostService broker connection.", err)
	}

	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
		t.Errorf("action = %v, want AUTO_MERGE", tasks[0].GetAction())
	}
}

func TestIntegration_PluginGRPCRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	binPath := buildPluginBinary(t)

	mergeable := true
	success := vcs.CheckConclusionSuccess
	failure := vcs.CheckConclusionFailure

	provider := &testVCSProvider{
		prs: []vcs.PRSummary{
			{
				Number:     10,
				Title:      "Bump lodash from 4.17.20 to 4.17.21",
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
				State:      vcs.PRStateOpen,
				Author:     "app/dependabot",
			},
			{
				Number:     11,
				Title:      "Bump express from 4.0.0 to 5.0.0",
				HeadBranch: "dependabot/npm_and_yarn/express-5.0.0",
				State:      vcs.PRStateOpen,
				Author:     "app/dependabot",
			},
		},
		checks: map[int][]vcs.CheckResult{
			10: {{ID: "ci-1", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			11: {{ID: "ci-2", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
		},
		status: map[int]*vcs.PRStatus{
			10: {State: vcs.PRStateOpen, Mergeable: &mergeable},
		},
	}

	hostService := pluginpkg.NewHostServiceServer(provider)
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource: &taskSourceWithBroker{
			hostService: hostService,
		},
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  sharedplugin.ProtocolVersion,
			MagicCookieKey:   sharedplugin.MagicCookieKey,
			MagicCookieValue: sharedplugin.MagicCookieValue,
		},
		Plugins: pluginMap,
		Cmd:     exec.Command(binPath),
		AllowedProtocols: []goplugin.Protocol{
			goplugin.ProtocolGRPC,
		},
		Logger: hclog.NewNullLogger(),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}

	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
	if err != nil {
		t.Fatalf("dispense TaskSource: %v", err)
	}

	taskSource, ok := raw.(pluginpkg.TaskSource)
	if !ok {
		t.Fatalf("dispensed type %T does not implement TaskSource", raw)
	}

	ctx := context.Background()

	// --- Test GetInfo ---
	t.Run("GetInfo", func(t *testing.T) {
		info, err := taskSource.GetInfo(ctx)
		if err != nil {
			t.Fatalf("GetInfo: %v", err)
		}
		if info.GetName() != "dependabot" {
			t.Errorf("name = %q, want %q", info.GetName(), "dependabot")
		}
		if info.GetVersion() != "0.1.0" {
			t.Errorf("version = %q, want %q", info.GetVersion(), "0.1.0")
		}
	})

	// --- Test PollTasks ---
	t.Run("PollTasks", func(t *testing.T) {
		tasks, err := taskSource.PollTasks(ctx, "https://github.com/org/repo")
		if err != nil {
			t.Fatalf("PollTasks: %v", err)
		}

		// Expect 2 tasks: lodash AUTO_MERGE (checks passed, mergeable) and
		// express CREATE_SESSION (checks failed).
		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d", len(tasks))
		}

		// Task 1: lodash should be AUTO_MERGE.
		if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
			t.Errorf("tasks[0] action = %v, want AUTO_MERGE", tasks[0].GetAction())
		}
		if tasks[0].GetExternalId() == "" {
			t.Error("tasks[0] external_id should not be empty")
		}

		// Task 2: express should be CREATE_SESSION with a plan.
		if tasks[1].GetAction() != bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION {
			t.Errorf("tasks[1] action = %v, want CREATE_SESSION", tasks[1].GetAction())
		}
		if tasks[1].GetPlan() == "" {
			t.Error("tasks[1] plan should not be empty for CREATE_SESSION")
		}
		if tasks[1].GetExistingBranch() != "dependabot/npm_and_yarn/express-5.0.0" {
			t.Errorf("tasks[1] existing_branch = %q, want dependabot branch", tasks[1].GetExistingBranch())
		}
	})

	// --- Test PollTasks with empty URL ---
	t.Run("PollTasks_EmptyURL", func(t *testing.T) {
		tasks, err := taskSource.PollTasks(ctx, "")
		if err != nil {
			t.Fatalf("PollTasks empty URL: %v", err)
		}
		if len(tasks) != 0 {
			t.Errorf("expected 0 tasks for empty URL, got %d", len(tasks))
		}
	})

	// --- Test UpdateTaskStatus ---
	t.Run("UpdateTaskStatus", func(t *testing.T) {
		err := taskSource.UpdateTaskStatus(ctx, "dep:pr:repo:10",
			bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED, "merged successfully")
		if err != nil {
			t.Fatalf("UpdateTaskStatus: %v", err)
		}
	})
}
