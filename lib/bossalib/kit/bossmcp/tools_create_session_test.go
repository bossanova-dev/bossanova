package bossmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// createSessionResultJSON decodes the create_session success payload so a test
// asserts fields, not substrings.
func createSessionResultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("create_session returned an error result: %s", textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("decode create_session result: %v\n%s", err, textOf(t, res))
	}
	return out
}

func callCreateSession(t *testing.T, backend Backend, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_session", Arguments: args})
	if err != nil {
		t.Fatalf("call create_session: %v", err)
	}
	return res
}

func TestCreateSessionVerifyOnlyReachesRPC(t *testing.T) {
	var got *pb.CreateSessionRequest
	backend := &fakeBackend{createSession: func(_ context.Context, req *pb.CreateSessionRequest) (*CreateSessionResult, error) {
		got = req
		return &CreateSessionResult{Session: &pb.Session{Id: "sess-vo", IsVerifyOnly: req.GetIsVerifyOnly()}}, nil
	}}

	out := createSessionResultJSON(t, callCreateSession(t, backend, map[string]any{
		"repo_id": "r1", "prompt": "/boss-verify 42", "pr_number": 42, "verify_only": true,
	}))
	if !got.GetIsVerifyOnly() || got.GetPrNumber() != 42 {
		t.Fatalf("verify_only/pr_number not forwarded: is_verify_only=%v pr_number=%d", got.GetIsVerifyOnly(), got.GetPrNumber())
	}
	if out["is_verify_only"] != true {
		t.Fatalf("result is_verify_only = %v, want true: %v", out["is_verify_only"], out)
	}

	out = createSessionResultJSON(t, callCreateSession(t, backend, map[string]any{
		"repo_id": "r1", "prompt": "fix it", "pr_number": 42,
	}))
	if got.GetIsVerifyOnly() {
		t.Fatal("omitted verify_only must leave is_verify_only false on the RPC")
	}
	if v, ok := out["is_verify_only"]; !ok || v != false {
		t.Fatalf("result must report is_verify_only=false explicitly, got %v (present=%v)", v, ok)
	}
}

func TestCreateSessionVerifyOnlyRequiresPRNumber(t *testing.T) {
	called := false
	backend := &fakeBackend{createSession: func(context.Context, *pb.CreateSessionRequest) (*CreateSessionResult, error) {
		called = true
		return &CreateSessionResult{Session: &pb.Session{Id: "sess-x"}}, nil
	}}

	res := callCreateSession(t, backend, map[string]any{"repo_id": "r1", "prompt": "verify", "verify_only": true})
	if !res.IsError {
		t.Fatalf("verify_only without pr_number must be a tool error, got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "pr_number") {
		t.Errorf("error must name the missing pr_number argument: %s", textOf(t, res))
	}
	if called {
		t.Error("verify_only without pr_number must be refused before the CreateSession RPC")
	}
}

// A path that drops is_verify_only (an older daemon or hosted orchestrator)
// creates an ordinary session that repair can push to; success would hide it.
func TestCreateSessionVerifyOnlyDroppedIsAnError(t *testing.T) {
	backend := &fakeBackend{createSession: func(context.Context, *pb.CreateSessionRequest) (*CreateSessionResult, error) {
		return &CreateSessionResult{Session: &pb.Session{Id: "sess-dropped"}}, nil
	}}

	res := callCreateSession(t, backend, map[string]any{
		"repo_id": "r1", "prompt": "verify", "pr_number": 7, "verify_only": true,
	})
	if !res.IsError {
		t.Fatalf("a created session that is not verify-only must be a tool error, got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "sess-dropped") {
		t.Errorf("error must name the created session so the caller can stop it: %s", textOf(t, res))
	}
}

func TestCreateSessionVerifyOnlyAttachReportsExistingFlag(t *testing.T) {
	agentID := "agent-existing"
	attach := func(existingVerifyOnly bool) *fakeBackend {
		return &fakeBackend{createSession: func(context.Context, *pb.CreateSessionRequest) (*CreateSessionResult, error) {
			return &CreateSessionResult{
				Session:          &pb.Session{Id: "sess-owner", AgentSessionId: &agentID, IsVerifyOnly: existingVerifyOnly},
				AttachedExisting: true,
			}, nil
		}}
	}
	args := map[string]any{"repo_id": "r1", "prompt": "/boss-verify 9", "pr_number": 9, "verify_only": true}

	out := createSessionResultJSON(t, callCreateSession(t, attach(false), args))
	if out["attached_existing"] != true || out["is_verify_only"] != false {
		t.Fatalf("attach to a repairable session: attached_existing=%v is_verify_only=%v, want true/false", out["attached_existing"], out["is_verify_only"])
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "is_verify_only") {
		t.Errorf("note must warn the existing session is not verify-only: %q", note)
	}

	out = createSessionResultJSON(t, callCreateSession(t, attach(true), args))
	if out["attached_existing"] != true || out["is_verify_only"] != true {
		t.Fatalf("attach to a verify-only session: attached_existing=%v is_verify_only=%v, want true/true", out["attached_existing"], out["is_verify_only"])
	}
	if note, _ := out["note"].(string); strings.Contains(note, "is_verify_only") {
		t.Errorf("a verify-only existing session needs no repair warning: %q", note)
	}
}

func TestCreateSessionSchemaAdvertisesVerifyOnly(t *testing.T) {
	cs := newConnectedClient(t, &fakeBackend{}, Options{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "create_session" {
			continue
		}
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		prop, _ := props["verify_only"].(map[string]any)
		if prop["type"] != "boolean" {
			t.Fatalf("create_session verify_only property = %v, want a boolean", props["verify_only"])
		}
		return
	}
	t.Fatal("create_session is not registered")
}
