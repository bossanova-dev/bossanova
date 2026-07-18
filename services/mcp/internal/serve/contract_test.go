package serve

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// expectedTools is the complete set of 55 bossanova MCP tool names:
// 19 read-only + 26 mutating + 10 destructive.
var expectedTools = []string{
	// read-only (19)
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
	"list_accounts",
	"get_chat_transcript",
	"get_settings",
	// mutating (26)
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
	"start_chat",
	"record_chat",
	"update_chat_title",
	"wake_chat",
	"report_chat_status",
	"create_cron_job",
	"update_cron_job",
	"run_cron_job_now",
	"add_account",
	"refresh_account",
	"update_account",
	"test_account",
	"send_chat_message",
	"switch_account",
	"update_settings",
	"start_repair_workflow",
	// destructive (10)
	"remove_repo",
	"close_session",
	"merge_session",
	"archive_session",
	"resurrect_session",
	"remove_session",
	"delete_chat",
	"empty_trash",
	"delete_cron_job",
	"remove_account",
}

// contractBackend is a minimal bossmcp.Backend sufficient for the contract test:
// it handles list_sessions (read), remove_repo (destructive), and the
// chat-control trio (create_session, send_chat_message, get_chat_transcript).
// Any other method panics to keep the test scope honest.
type contractBackend struct {
	bossmcp.Backend // nil embed: any uncovered method panics if called
	sessions        []*pb.Session
	removedRepoID   string

	// destructive-tier confirm-gate tracking (BOS-404)
	closedSessionID  string
	removedSessionID string
	resurrectedID    string
	deletedAgentID   string
	emptyTrashCalled bool

	// chat-control fields for TestContractChatControl
	createdSession     *pb.Session
	sendResponse       *pb.SendChatMessageResponse
	transcriptResponse *pb.GetChatTranscriptResponse
	sentAgentSessionID string
	sentMessage        string
	sentSubmit         bool
}

func (b *contractBackend) ListSessions(_ context.Context, _ *pb.ListSessionsRequest) ([]*pb.Session, error) {
	return b.sessions, nil
}

func (b *contractBackend) RemoveRepo(_ context.Context, id string) error {
	b.removedRepoID = id
	return nil
}

func (b *contractBackend) CloseSession(_ context.Context, id string) (*pb.Session, error) {
	b.closedSessionID = id
	return &pb.Session{Id: id}, nil
}

func (b *contractBackend) RemoveSession(_ context.Context, id string) error {
	b.removedSessionID = id
	return nil
}

func (b *contractBackend) ResurrectSession(_ context.Context, id string) (*pb.Session, error) {
	b.resurrectedID = id
	return &pb.Session{Id: id}, nil
}

func (b *contractBackend) DeleteChat(_ context.Context, agentSessionID string) error {
	b.deletedAgentID = agentSessionID
	return nil
}

func (b *contractBackend) EmptyTrash(_ context.Context, _ *pb.EmptyTrashRequest) (int32, error) {
	b.emptyTrashCalled = true
	return 0, nil
}

func (b *contractBackend) CreateSession(_ context.Context, _ *pb.CreateSessionRequest) (*bossmcp.CreateSessionResult, error) {
	return &bossmcp.CreateSessionResult{Session: b.createdSession}, nil
}

func (b *contractBackend) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	b.sentAgentSessionID = req.AgentSessionId
	b.sentMessage = req.Message
	b.sentSubmit = req.GetSubmit()
	return b.sendResponse, nil
}

func (b *contractBackend) GetChatTranscript(_ context.Context, _ *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	return b.transcriptResponse, nil
}

// newContractClient wires an in-memory server using newServer (identical to
// the Stdio path) and returns a connected MCP client session.
func newContractClient(t *testing.T, backend bossmcp.Backend) *mcp.ClientSession {
	t.Helper()
	server := newServer(backend, bossmcp.Options{}, nil)
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

// TestContractFullToolSet asserts that tools/list returns exactly the 55
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

// TestContractParityFieldsAdvertised guards the public tool surface: it asserts
// that the MCP/TUI-parity fields wired through the arg structs actually appear in
// each tool's advertised input schema over the wire (tools/list). A field dropped
// from an arg struct would silently stop being settable; this fails if that
// happens.
func TestContractParityFieldsAdvertised(t *testing.T) {
	t.Parallel()

	backend := &contractBackend{}
	cs := newContractClient(t, backend)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	byName := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	want := map[string][]string{
		"update_repo":       {"linear_api_key", "sentry_api_key", "sentry_org"},
		"create_cron_job":   {"model", "gate_command", "run_setup_command"},
		"update_cron_job":   {"model", "gate_command", "run_setup_command"},
		"create_session":    {"base_branch", "branch_name", "force_branch", "force", "quick_chat", "detach", "tmux_unattended", "model", "pr_number", "tracker_id", "tracker_url", "tracker_source"},
		"send_chat_message": {"submit"},
	}

	for toolName, fields := range want {
		tool, ok := byName[toolName]
		if !ok {
			t.Errorf("tool %q missing from tools/list", toolName)
			continue
		}
		props := schemaProperties(t, tool)
		for _, field := range fields {
			if _, ok := props[field]; !ok {
				t.Errorf("tool %q input schema is missing advertised field %q (have %v)", toolName, field, sortedKeys(props))
			}
		}
	}

	// tracker_issue is intentionally excluded from create_session (web-only).
	if props := schemaProperties(t, byName["create_session"]); props != nil {
		if _, ok := props["tracker_issue"]; ok {
			t.Error("create_session must not advertise the web-only tracker_issue field")
		}
	}
}

// schemaProperties marshals a tool's InputSchema (typed `any` over the wire) and
// returns its top-level JSON-Schema "properties" object.
func schemaProperties(t *testing.T, tool *mcp.Tool) map[string]json.RawMessage {
	t.Helper()
	if tool == nil {
		return nil
	}
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema for %q: %v", tool.Name, err)
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal input schema for %q: %v", tool.Name, err)
	}
	return parsed.Properties
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

	// BOS-404: the destructive tools promoted to the hosted gateway must still
	// pass through the local confirm:true gate before the backend is called.
	// The gate fires in bossmcp before proxybackend, so it holds identically for
	// the local socket and hosted gateway backends.
	destructive := []struct {
		tool   string
		args   map[string]any
		called func() bool
	}{
		{"close_session", map[string]any{"id": "s-1"}, func() bool { return backend.closedSessionID != "" }},
		{"remove_session", map[string]any{"id": "s-1"}, func() bool { return backend.removedSessionID != "" }},
		{"resurrect_session", map[string]any{"id": "s-1"}, func() bool { return backend.resurrectedID != "" }},
		{"delete_chat", map[string]any{"agent_session_id": "a-1"}, func() bool { return backend.deletedAgentID != "" }},
		{"empty_trash", map[string]any{}, func() bool { return backend.emptyTrashCalled }},
	}
	for _, tc := range destructive {
		t.Run(tc.tool, func(t *testing.T) {
			// Without confirm — error result, backend untouched.
			resNo, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("%s (no confirm): unexpected transport error: %v", tc.tool, err)
			}
			if !resNo.IsError {
				t.Fatalf("%s without confirm should return IsError=true", tc.tool)
			}
			if !strings.Contains(textOf(t, resNo), "confirm") {
				t.Fatalf("%s error message should mention 'confirm'; got: %s", tc.tool, textOf(t, resNo))
			}
			if tc.called() {
				t.Fatalf("%s backend must not be called without confirm", tc.tool)
			}

			// With confirm:true — success, backend called.
			args := map[string]any{"confirm": true}
			for k, v := range tc.args {
				args[k] = v
			}
			resYes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: args})
			if err != nil {
				t.Fatalf("%s (confirm:true): unexpected transport error: %v", tc.tool, err)
			}
			if resYes.IsError {
				t.Fatalf("%s with confirm:true returned error: %s", tc.tool, textOf(t, resYes))
			}
			if !tc.called() {
				t.Fatalf("%s backend not called with confirm:true", tc.tool)
			}
		})
	}
}

// TestContractChatControl exercises the full trigger→follow→result loop over the
// in-memory transport: create_session → send_chat_message → get_chat_transcript.
func TestContractChatControl(t *testing.T) {
	t.Parallel()

	const agentSessionID = "agt-contract-42"
	const seededOpinion = "looks good to me — no blocking issues"

	agentSessionIDStr := agentSessionID
	backend := &contractBackend{
		createdSession: &pb.Session{
			Id:             "s-contract-cc-1",
			Title:          "second opinion session",
			AgentSessionId: &agentSessionIDStr,
		},
		sendResponse: &pb.SendChatMessageResponse{
			Delivered:       true,
			TmuxSessionName: "bossanova:agt-contract-42",
		},
		transcriptResponse: &pb.GetChatTranscriptResponse{
			FinalAssistantText: seededOpinion,
			Exists:             true,
			Messages:           nil,
		},
	}
	cs := newContractClient(t, backend)
	ctx := context.Background()

	// Step 1: create_session — trigger the second-opinion session.
	resCreate, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_session",
		Arguments: map[string]any{
			"repo_id": "repo-contract-1",
			"prompt":  "Please review the proposed change and give a second opinion.",
			"agent":   "codex",
		},
	})
	if err != nil {
		t.Fatalf("CallTool create_session: unexpected transport error: %v", err)
	}
	if resCreate.IsError {
		t.Fatalf("create_session returned error result: %s", textOf(t, resCreate))
	}
	createText := textOf(t, resCreate)
	if !strings.Contains(createText, "s-contract-cc-1") {
		t.Fatalf("create_session result missing session id; got: %s", createText)
	}

	// Step 2: send_chat_message — follow up with a user message.
	resMsg, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "send_chat_message",
		Arguments: map[string]any{
			"agent_session_id": agentSessionID,
			"message":          "What do you think about the edge-case handling?",
			"wake_if_asleep":   true,
			"submit":           true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool send_chat_message: unexpected transport error: %v", err)
	}
	if resMsg.IsError {
		t.Fatalf("send_chat_message returned error result: %s", textOf(t, resMsg))
	}
	msgText := textOf(t, resMsg)
	if !strings.Contains(msgText, "true") {
		t.Fatalf("send_chat_message result missing delivered:true; got: %s", msgText)
	}
	if !strings.Contains(msgText, "bossanova:agt-contract-42") {
		t.Fatalf("send_chat_message result missing tmux_session_name; got: %s", msgText)
	}

	// Assert the backend received the correct agent_session_id and message.
	if backend.sentAgentSessionID != agentSessionID {
		t.Fatalf("backend.SendChatMessage got agent_session_id=%q, want %q", backend.sentAgentSessionID, agentSessionID)
	}
	if !strings.Contains(backend.sentMessage, "edge-case handling") {
		t.Fatalf("backend.SendChatMessage got message=%q, want it to contain 'edge-case handling'", backend.sentMessage)
	}
	if !backend.sentSubmit {
		t.Fatalf("backend.SendChatMessage got submit=false, want true (submit arg must thread through)")
	}

	// Step 3: get_chat_transcript — read the final assistant result.
	resTx, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_chat_transcript",
		Arguments: map[string]any{
			"agent_session_id": agentSessionID,
		},
	})
	if err != nil {
		t.Fatalf("CallTool get_chat_transcript: unexpected transport error: %v", err)
	}
	if resTx.IsError {
		t.Fatalf("get_chat_transcript returned error result: %s", textOf(t, resTx))
	}
	txText := textOf(t, resTx)
	if !strings.Contains(txText, seededOpinion) {
		t.Fatalf("get_chat_transcript result missing seeded final_assistant_text; got: %s", txText)
	}
}
