package bossmcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newConnectedClient wires an in-memory MCP client<->server session against a
// freshly-registered tool set, returning the client session. The caller must
// Close it.
func newConnectedClient(t *testing.T, backend Backend, opts Options) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "bossanova", Version: "test"}, nil)
	RegisterTools(server, backend, opts)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// textOf extracts the first text content block from a tool result.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no text content in result")
	return ""
}

func TestListSessionsTool(t *testing.T) {
	backend := &fakeBackend{
		listSessions: func(_ context.Context, _ *pb.ListSessionsRequest) ([]*pb.Session, error) {
			return []*pb.Session{{Id: "sess-1"}}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error result: %s", textOf(t, res))
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0]["id"] != "sess-1" {
		t.Fatalf("unexpected sessions payload: %s", textOf(t, res))
	}
}

// TestListSessionsTool_RepoIDFilter pins the repo_id propagation on tools.go:56
// (`if args.RepoID != ""`). A non-empty repo_id must reach the backend as a set
// RepoId pointer, and an absent repo_id must leave RepoId nil. This kills the
// CONDITIONALS_NEGATION mutant (`!= ""` → `== ""`), which would otherwise both
// drop a real filter and inject an empty-string filter when none was requested.
func TestListSessionsTool_RepoIDFilter(t *testing.T) {
	t.Run("non-empty repo_id reaches backend", func(t *testing.T) {
		var captured *pb.ListSessionsRequest
		backend := &fakeBackend{
			listSessions: func(_ context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
				captured = req
				return nil, nil
			},
		}
		cs := newConnectedClient(t, backend, Options{})

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "list_sessions",
			Arguments: map[string]any{"repo_id": "repo-42"},
		})
		if err != nil {
			t.Fatalf("call tool: %v", err)
		}
		if res.IsError {
			t.Fatalf("tool returned error result: %s", textOf(t, res))
		}
		if captured == nil {
			t.Fatal("backend did not receive a request")
		}
		if captured.RepoId == nil {
			t.Fatal("RepoId was nil; non-empty repo_id must set the filter")
		}
		if got := captured.GetRepoId(); got != "repo-42" {
			t.Errorf("RepoId = %q, want %q", got, "repo-42")
		}
	})

	t.Run("absent repo_id leaves filter unset", func(t *testing.T) {
		var captured *pb.ListSessionsRequest
		backend := &fakeBackend{
			listSessions: func(_ context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
				captured = req
				return nil, nil
			},
		}
		cs := newConnectedClient(t, backend, Options{})

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "list_sessions",
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatalf("call tool: %v", err)
		}
		if res.IsError {
			t.Fatalf("tool returned error result: %s", textOf(t, res))
		}
		if captured == nil {
			t.Fatal("backend did not receive a request")
		}
		if captured.RepoId != nil {
			t.Errorf("RepoId = %v, want nil when repo_id absent", captured.GetRepoId())
		}
	})
}
