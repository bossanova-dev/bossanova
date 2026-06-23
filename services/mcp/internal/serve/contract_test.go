package serve

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// expectedTools is the complete set of 42 bossanova MCP tool names:
// 16 read-only + 17 mutating + 9 destructive.
var expectedTools = []string{
	// read-only (16)
	"list_sessions",
	"resolve_context",
	"validate_repo_path",
	"list_repos",
	"list_repo_prs",
	"list_tracker_issues",
	"get_session",
	"list_chats",
	"get_chat_statuses",
	"get_session_statuses",
	"list_check_snapshots",
	"repair_doctor",
	"list_agents",
	"list_plugins",
	"list_cron_jobs",
	"get_cron_job",
	// mutating (17)
	"register_repo",
	"clone_and_register_repo",
	"update_repo",
	"create_session",
	"stop_session",
	"pause_session",
	"resume_session",
	"retry_session",
	"update_session",
	"link_session_pr",
	"record_chat",
	"update_chat_title",
	"wake_chat",
	"report_chat_status",
	"create_cron_job",
	"update_cron_job",
	"run_cron_job_now",
	// destructive (9)
	"remove_repo",
	"close_session",
	"merge_session",
	"archive_session",
	"resurrect_session",
	"remove_session",
	"delete_chat",
	"empty_trash",
	"delete_cron_job",
}

// contractBackend is a minimal bossmcp.Backend sufficient for the contract test:
// it handles list_sessions (read) and remove_repo (destructive).
// Any other method panics to keep the test scope honest.
type contractBackend struct {
	bossmcp.Backend // nil embed: any uncovered method panics if called
	sessions        []*pb.Session
	removedRepoID   string
}

func (b *contractBackend) ListSessions(_ context.Context, _ *pb.ListSessionsRequest) ([]*pb.Session, error) {
	return b.sessions, nil
}

func (b *contractBackend) RemoveRepo(_ context.Context, id string) error {
	b.removedRepoID = id
	return nil
}

// newContractClient wires an in-memory server using newServer (identical to
// the Stdio path) and returns a connected MCP client session.
func newContractClient(t *testing.T, backend bossmcp.Backend) *mcp.ClientSession {
	t.Helper()
	server := newServer(backend, bossmcp.Options{})
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "0.0.0"}, nil)

	st, ct := mcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestContractFullToolSet asserts that tools/list returns exactly the 42
// expected bossanova tools over the in-memory transport.
func TestContractFullToolSet(t *testing.T) {
	t.Parallel()

	backend := &contractBackend{}
	cs := newContractClient(t, backend)

	ctx := context.Background()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	want := make([]string, len(expectedTools))
	copy(want, expectedTools)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("tools/list returned %d tools, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestContractReadTool exercises the list_sessions tool end-to-end over the
// in-memory transport.
func TestContractReadTool(t *testing.T) {
	t.Parallel()

	backend := &contractBackend{
		sessions: []*pb.Session{{Id: "s-contract-1", Title: "contract session"}},
	}
	cs := newContractClient(t, backend)

	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_sessions",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool list_sessions: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_sessions returned error result: %s", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "s-contract-1") {
		t.Fatalf("list_sessions result missing expected session id; got: %s", text)
	}
}

// TestContractDestructiveToolConfirmGate asserts that remove_repo returns an
// error result (not a protocol error) when confirm is omitted, and succeeds
// when confirm:true is passed.
func TestContractDestructiveToolConfirmGate(t *testing.T) {
	t.Parallel()

	backend := &contractBackend{}
	cs := newContractClient(t, backend)

	ctx := context.Background()

	// Without confirm — must return an error result, not a transport error.
	resNoConfirm, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "remove_repo",
		Arguments: map[string]any{"id": "repo-123"},
	})
	if err != nil {
		t.Fatalf("CallTool remove_repo (no confirm): unexpected transport error: %v", err)
	}
	if !resNoConfirm.IsError {
		t.Fatal("remove_repo without confirm should return IsError=true; got IsError=false")
	}
	errText := textOf(t, resNoConfirm)
	if !strings.Contains(errText, "confirm") {
		t.Fatalf("error message should mention 'confirm'; got: %s", errText)
	}
	if backend.removedRepoID != "" {
		t.Fatal("backend.RemoveRepo must not be called without confirm")
	}

	// With confirm:true — must succeed and call the backend.
	resConfirmed, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "remove_repo",
		Arguments: map[string]any{"id": "repo-123", "confirm": true},
	})
	if err != nil {
		t.Fatalf("CallTool remove_repo (confirm:true): unexpected transport error: %v", err)
	}
	if resConfirmed.IsError {
		t.Fatalf("remove_repo with confirm:true returned error: %s", textOf(t, resConfirmed))
	}
	if backend.removedRepoID != "repo-123" {
		t.Fatalf("backend.RemoveRepo not called with correct id; got %q", backend.removedRepoID)
	}
}
