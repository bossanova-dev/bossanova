package mcpcontract

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/testharness"
)

// captureBackend records the protobuf request each request-shaped mutating tool
// builds, so the contract test can replay that exact request against a real
// bossd daemon. It embeds a nil bossmcp.Backend: any method the contract test
// does not exercise panics if called, keeping the test scope honest.
type captureBackend struct {
	bossmcp.Backend
	registerRepo *pb.RegisterRepoRequest
	cloneRepo    *pb.CloneAndRegisterRepoRequest
	createSess   *pb.CreateSessionRequest
	createCron   *pb.CreateCronJobRequest
}

func (b *captureBackend) RegisterRepo(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	b.registerRepo = req
	return &pb.Repo{Id: "captured"}, nil
}

func (b *captureBackend) CloneAndRegisterRepo(_ context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	b.cloneRepo = req
	return &pb.Repo{Id: "captured"}, nil
}

func (b *captureBackend) CreateSession(_ context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	b.createSess = req
	return &pb.Session{Id: "captured"}, nil
}

func (b *captureBackend) CreateCronJob(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	b.createCron = req
	return &pb.CronJob{Id: "captured"}, nil
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
	h := testharness.New(t)
	ctx := context.Background()

	t.Run("register_repo", func(t *testing.T) {
		backend := &captureBackend{}
		callTool(t, backend, "register_repo", map[string]any{
			"local_path":          testharness.TempRepoDir(t),
			"name":                "contract-repo",
			"default_base_branch": "main",
		})
		_, err := h.Client.RegisterRepo(ctx, connect.NewRequest(backend.registerRepo))
		assertNotMissingRequired(t, "register_repo", err)
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
}
