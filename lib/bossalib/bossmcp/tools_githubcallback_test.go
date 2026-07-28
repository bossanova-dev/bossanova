package bossmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// secretMessage is the callback prompt body. It is a secret and must never
// appear in any tool's output on any surface.
const secretMessage = "SECRET-PROMPT-do-not-leak-42"

// TestRegisterGithubCallbackHappyPath proves the register tool forwards a
// normalized request to the backend and returns the created callback without
// ever echoing the secret message body.
func TestRegisterGithubCallbackHappyPath(t *testing.T) {
	var got *pb.CreateGithubCallbackRequest
	backend := &fakeBackend{createGithubCallback: func(_ context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
		got = req
		return &pb.GithubCallback{
			Id:           "cb1",
			TargetChatId: req.GetTargetChatId(),
			RepoOwner:    req.GetRepoOwner(),
			RepoName:     req.GetRepoName(),
			PrNumber:     req.GetPrNumber(),
			Trigger:      req.GetTrigger(),
			State:        "active",
			Message:      req.GetMessage(), // daemon echoes it; the tool must scrub it
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "register_github_callback",
		Arguments: map[string]any{
			"pr":             "123",
			"repo":           "Owner/Repo",
			"trigger":        "merged",
			"target_chat_id": "chat-1",
			"message":        secretMessage,
		},
	})
	if err != nil {
		t.Fatalf("call register_github_callback: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.CreateGithubCallback was never called")
	}
	// Repo slug must be lowercased and the bare PR number anchored to it.
	if got.GetRepoOwner() != "owner" || got.GetRepoName() != "repo" || got.GetPrNumber() != 123 {
		t.Errorf("normalized ref wrong: owner=%q repo=%q pr=%d", got.GetRepoOwner(), got.GetRepoName(), got.GetPrNumber())
	}
	if got.GetTrigger() != "merged" {
		t.Errorf("trigger = %q, want merged", got.GetTrigger())
	}
	if got.GetMessage() != secretMessage {
		t.Errorf("message must be forwarded verbatim to the backend; got %q", got.GetMessage())
	}
	if got.GetExpiresAt() == nil {
		t.Error("expires_at must be defaulted (24h) when expires_in is omitted")
	}
	out := textOf(t, res)
	if strings.Contains(out, secretMessage) {
		t.Errorf("register_github_callback leaked the secret message in its result: %s", out)
	}
	if !strings.Contains(out, "cb1") {
		t.Errorf("result should include the callback id; got: %s", out)
	}
}

// TestRegisterGithubCallbackFullURL proves a canonical github.com PR URL is
// accepted without a repo slug and the owner/repo are parsed from it.
func TestRegisterGithubCallbackFullURL(t *testing.T) {
	var got *pb.CreateGithubCallbackRequest
	backend := &fakeBackend{createGithubCallback: func(_ context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
		got = req
		return &pb.GithubCallback{Id: "cb2"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "register_github_callback",
		Arguments: map[string]any{
			"pr":             "https://github.com/acme/widget/pull/77",
			"trigger":        "checks_passed",
			"target_chat_id": "chat-2",
			"message":        "ping me",
			"expires_in":     "7d",
			"group":          "grp-a",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got.GetRepoOwner() != "acme" || got.GetRepoName() != "widget" || got.GetPrNumber() != 77 {
		t.Errorf("URL parse wrong: owner=%q repo=%q pr=%d", got.GetRepoOwner(), got.GetRepoName(), got.GetPrNumber())
	}
	if got.GetGroupId() != "grp-a" {
		t.Errorf("group_id = %q, want grp-a", got.GetGroupId())
	}
}

// TestRegisterGithubCallbackValidationErrors proves every bad-input path returns
// an agent-correctable error result and never reaches the backend.
func TestRegisterGithubCallbackValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing message", map[string]any{"pr": "1", "repo": "o/r", "trigger": "merged", "target_chat_id": "c"}},
		{"blank message", map[string]any{"pr": "1", "repo": "o/r", "trigger": "merged", "target_chat_id": "c", "message": "   "}},
		{"missing target chat", map[string]any{"pr": "1", "repo": "o/r", "trigger": "merged", "message": "m"}},
		{"bad trigger", map[string]any{"pr": "1", "repo": "o/r", "trigger": "exploded", "target_chat_id": "c", "message": "m"}},
		{"bare pr without repo", map[string]any{"pr": "1", "trigger": "merged", "target_chat_id": "c", "message": "m"}},
		{"non-github url", map[string]any{"pr": "https://gitlab.com/o/r/pull/1", "trigger": "merged", "target_chat_id": "c", "message": "m"}},
		{"expiry over max", map[string]any{"pr": "1", "repo": "o/r", "trigger": "merged", "target_chat_id": "c", "message": "m", "expires_in": "60d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			backend := &fakeBackend{createGithubCallback: func(_ context.Context, _ *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
				called = true
				return &pb.GithubCallback{Id: "x"}, nil
			}}
			cs := newConnectedClient(t, backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "register_github_callback", Arguments: tc.args})
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected error result for %q; got: %s", tc.name, textOf(t, res))
			}
			if called {
				t.Fatalf("backend must not be called on invalid input (%q)", tc.name)
			}
		})
	}
}

// TestListGithubCallbacksRedactsMessage proves the list tool forwards its filters
// and never echoes any callback's secret message body.
func TestListGithubCallbacksRedactsMessage(t *testing.T) {
	var got *pb.ListGithubCallbacksRequest
	backend := &fakeBackend{listGithubCallbacks: func(_ context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
		got = req
		return []*pb.GithubCallback{
			{Id: "cb1", TargetChatId: "chat-1", Trigger: "merged", State: "active", Message: secretMessage},
			{Id: "cb2", TargetChatId: "chat-1", Trigger: "closed", State: "expired", Message: "another-secret"},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_github_callbacks",
		Arguments: map[string]any{
			"target_chat_id": "chat-1",
			"trigger":        "merged",
			"pr_number":      42,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.ListGithubCallbacks was never called")
	}
	if got.GetTargetChatId() != "chat-1" {
		t.Errorf("target_chat_id filter = %q, want chat-1", got.GetTargetChatId())
	}
	if got.GetTrigger() != "merged" {
		t.Errorf("trigger filter = %q, want merged", got.GetTrigger())
	}
	if got.GetPrNumber() != 42 {
		t.Errorf("pr_number filter = %d, want 42", got.GetPrNumber())
	}
	if got.RepoOwner != nil {
		t.Errorf("omitted repo_owner must remain unset; got %q", got.GetRepoOwner())
	}
	if got.RepoName != nil {
		t.Errorf("omitted repo_name must remain unset; got %q", got.GetRepoName())
	}
	if got.State != nil {
		t.Errorf("omitted state must remain unset; got %q", got.GetState())
	}
	out := textOf(t, res)
	if strings.Contains(out, secretMessage) || strings.Contains(out, "another-secret") {
		t.Errorf("list_github_callbacks leaked a secret message: %s", out)
	}
	if !strings.Contains(out, "cb1") || !strings.Contains(out, "cb2") {
		t.Errorf("result should include both callback ids; got: %s", out)
	}
}

// TestListGithubCallbacksRejectsBadTriggerFilter proves an invalid trigger filter
// is rejected before the backend is touched.
func TestListGithubCallbacksRejectsBadTriggerFilter(t *testing.T) {
	called := false
	backend := &fakeBackend{listGithubCallbacks: func(_ context.Context, _ *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
		called = true
		return nil, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_github_callbacks",
		Arguments: map[string]any{"trigger": "not-a-trigger"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result for bad trigger filter; got: %s", textOf(t, res))
	}
	if called {
		t.Fatal("backend must not be called on an invalid trigger filter")
	}
}

// TestDeleteGithubCallbackRequiresConfirm proves the destructive delete tool is
// confirm-gated and forwards the owning chat id for hosted routing.
func TestDeleteGithubCallbackRequiresConfirm(t *testing.T) {
	var gotChat, gotID string
	called := false
	backend := &fakeBackend{deleteGithubCallback: func(_ context.Context, targetChatID, id string) error {
		called = true
		gotChat, gotID = targetChatID, id
		return nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	// Without confirm: refuses and never touches the backend.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "delete_github_callback",
		Arguments: map[string]any{"id": "cb1", "target_chat_id": "chat-1"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result when confirm omitted; got: %s", textOf(t, res))
	}
	if called {
		t.Fatal("backend.DeleteGithubCallback must not run without confirm:true")
	}

	// With confirm: runs and forwards both routing key and id.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "delete_github_callback",
		Arguments: map[string]any{"id": "cb1", "target_chat_id": "chat-1", "confirm": true},
	})
	if err != nil || res.IsError {
		t.Fatalf("confirmed call failed: err=%v res=%s", err, textOf(t, res))
	}
	if !called {
		t.Fatal("backend.DeleteGithubCallback should run with confirm:true")
	}
	if gotChat != "chat-1" || gotID != "cb1" {
		t.Errorf("delete forwarded chat=%q id=%q, want chat-1/cb1", gotChat, gotID)
	}
}
