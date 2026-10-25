package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// callbackJSON mirrors the stable schema emitted by `boss callback add|list --json`.
// The message body is a secret and is deliberately absent from this schema — the
// test asserts it never appears in any output.
type callbackJSON struct {
	ID           string `json:"id"`
	GroupID      string `json:"group_id"`
	TargetChatID string `json:"target_chat_id"`
	RepoOwner    string `json:"repo_owner"`
	RepoName     string `json:"repo_name"`
	PRNumber     int32  `json:"pr_number"`
	Trigger      string `json:"trigger"`
	State        string `json:"state"`
	AttemptCount int32  `json:"attempt_count"`
	LastEvent    string `json:"last_event"`
	LastError    string `json:"last_error"`
	TriggeredAt  string `json:"triggered_at"`
	DeliveredAt  string `json:"delivered_at"`
	ExpiresAt    string `json:"expires_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func TestJourney_Callbacks_AC1_Callback_List_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "list")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No GitHub callbacks") {
		t.Errorf("expected empty-state message, got %q", res.Stdout)
	}
}

// A callback waiting for its chat's input box reads "waiting" in the table,
// with the worker's sentence as the reason. A triggered callback retrying
// after a failure keeps its state, and its raw error stays out of the table.

// TestJourney_Callbacks_AC3_Callback_Add_URL registers a callback via a full GitHub PR URL, so no
// repository context is needed. It asserts the human line, the daemon request
// shape, and — critically — that the secret message body never appears in output.
func TestJourney_Callbacks_AC3_Callback_Add_URL(t *testing.T) {
	h := clitest.New(t)
	const secret = "super-secret-delivery-prompt-DO-NOT-LEAK"
	res := h.Run("callback", "add",
		"https://github.com/Acme/Widgets/pull/42", "merged",
		"--chat", "chat-9",
		"--message", secret,
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// Owner/repo are lowercased canonically.
	for _, want := range []string{"Registered callback", "chat-9", "acme/widgets#42", "is merged"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
		t.Fatalf("SECRET LEAK: message body appeared in output\nstdout=%q\nstderr=%q", res.Stdout, res.Stderr)
	}

	calls := storedCallbacks(t, h)
	if len(calls) != 1 {
		t.Fatalf("expected 1 stored callback, got %d", len(calls))
	}
	req := calls[0]
	if req.GetRepoOwner() != "acme" || req.GetRepoName() != "widgets" || req.GetPrNumber() != 42 {
		t.Errorf("unexpected repo/pr in request: owner=%q repo=%q pr=%d", req.GetRepoOwner(), req.GetRepoName(), req.GetPrNumber())
	}
	if req.GetTargetChatId() != "chat-9" || req.GetTrigger() != "merged" {
		t.Errorf("unexpected chat/trigger: chat=%q trigger=%q", req.GetTargetChatId(), req.GetTrigger())
	}

	jsonResult := h.Run("callback", "add", "https://github.com/acme/widgets/pull/5", "checks_passed", "--chat", "chat-1", "--message", secret, "--json")
	var cb callbackJSON
	decodeJSON(t, jsonResult, &cb)
	if cb.RepoOwner != "acme" || cb.RepoName != "widgets" || cb.PRNumber != 5 || cb.Trigger != "checks_passed" || cb.TargetChatID != "chat-1" {
		t.Fatalf("unexpected callback: %+v", cb)
	}
	for _, args := range [][]string{{"callback", "list"}, {"callback", "list", "--json"}} {
		result := h.Run(args...)
		if !result.Success() || strings.Contains(result.Stdout+result.Stderr, secret) {
			t.Fatalf("listing failed or leaked body: %+v", result)
		}
	}
	if strings.Contains(jsonResult.Stdout+jsonResult.Stderr, secret) {
		t.Fatal("JSON registration leaked the prompt")
	}
	if req.GetExpiresAt() == nil {
		t.Errorf("expected a default expiry to be set")
	}
}

func TestJourney_Callbacks_AC2_Callback_Add_MissingMessage(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/1", "merged",
		"--chat", "chat-1",
	)

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with no --message; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "--message") {
		t.Errorf("stderr should require --message, got %q", res.Stderr)
	}
}

func storedCallbacks(t *testing.T, h *clitest.Harness) []*pb.GitHubCallback {
	t.Helper()
	res, err := h.Stack.Client().ListGitHubCallbacks(t.Context(), realstackrpc.Request(&pb.ListGitHubCallbacksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GithubCallbacks
}
