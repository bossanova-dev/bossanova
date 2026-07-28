package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

func testCallbacks() []*pb.GithubCallback {
	return []*pb.GithubCallback{
		{
			Id:           "cb-aaa",
			TargetChatId: "chat-1",
			RepoOwner:    "acme",
			RepoName:     "widgets",
			PrNumber:     123,
			Trigger:      "merged",
			State:        "active",
			ExpiresAt:    timestamppb.New(timestampDaysAgo(-1)),
		},
		{
			Id:           "cb-bbb",
			TargetChatId: "chat-2",
			RepoOwner:    "acme",
			RepoName:     "gadgets",
			PrNumber:     7,
			Trigger:      "checks_failed",
			State:        "leased",
			ExpiresAt:    timestamppb.New(timestampDaysAgo(-2)),
		},
	}
}

func TestCLI_Callback_List(t *testing.T) {
	h := clitest.New(t, clitest.WithGithubCallbacks(testCallbacks()...))
	res := h.Run("callback", "list")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"cb-aaa", "acme/widgets#123", "merged", "cb-bbb", "acme/gadgets#7", "checks_failed"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Callback_List_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "list")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No GitHub callbacks") {
		t.Errorf("expected empty-state message, got %q", res.Stdout)
	}
}

func TestCLI_Callback_List_ChatFilter(t *testing.T) {
	h := clitest.New(t, clitest.WithGithubCallbacks(testCallbacks()...))
	res := h.Run("callback", "list", "--chat", "chat-2")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "cb-bbb") {
		t.Errorf("stdout should contain chat-2 callback, got %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "cb-aaa") {
		t.Errorf("stdout should NOT contain chat-1 callback, got %q", res.Stdout)
	}
	// The daemon-side filter must have been requested with the chat set.
	calls := h.Daemon.ListGithubCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 ListGithubCallbacks call, got %d", len(calls))
	}
	if calls[0].GetTargetChatId() != "chat-2" {
		t.Errorf("expected TargetChatId filter chat-2, got %q", calls[0].GetTargetChatId())
	}
}

func TestCLI_Callback_List_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithGithubCallbacks(testCallbacks()...))
	res := h.Run("callback", "list", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var cbs []callbackJSON
	if err := json.Unmarshal([]byte(res.Stdout), &cbs); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if len(cbs) != 2 {
		t.Fatalf("expected 2 callbacks, got %d", len(cbs))
	}
	var first callbackJSON
	for _, cb := range cbs {
		if cb.ID == "cb-aaa" {
			first = cb
		}
	}
	if first.ID != "cb-aaa" {
		t.Fatalf("cb-aaa not present in JSON")
	}
	if first.RepoOwner != "acme" || first.RepoName != "widgets" || first.PRNumber != 123 {
		t.Errorf("unexpected repo/pr fields: %+v", first)
	}
	if first.Trigger != "merged" || first.State != "active" {
		t.Errorf("unexpected trigger/state: %+v", first)
	}
	if first.ExpiresAt == "" {
		t.Errorf("expected RFC3339 expires_at, got empty: %+v", first)
	}
}

// TestCLI_Callback_Add_URL registers a callback via a full GitHub PR URL, so no
// repository context is needed. It asserts the human line, the daemon request
// shape, and — critically — that the secret message body never appears in output.
func TestCLI_Callback_Add_URL(t *testing.T) {
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

	calls := h.Daemon.CreateGithubCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateGithubCallback call, got %d", len(calls))
	}
	req := calls[0]
	if req.GetRepoOwner() != "acme" || req.GetRepoName() != "widgets" || req.GetPrNumber() != 42 {
		t.Errorf("unexpected repo/pr in request: owner=%q repo=%q pr=%d", req.GetRepoOwner(), req.GetRepoName(), req.GetPrNumber())
	}
	if req.GetTargetChatId() != "chat-9" || req.GetTrigger() != "merged" {
		t.Errorf("unexpected chat/trigger: chat=%q trigger=%q", req.GetTargetChatId(), req.GetTrigger())
	}
	if req.GetMessage() != secret {
		t.Errorf("message must be delivered verbatim to the daemon; got %q", req.GetMessage())
	}
	if req.GetExpiresAt() == nil {
		t.Errorf("expected a default expiry to be set")
	}
}

func TestCLI_Callback_Add_JSON(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/5", "checks_passed",
		"--chat", "chat-1",
		"--message", "ping me",
		"--json",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var cb callbackJSON
	if err := json.Unmarshal([]byte(res.Stdout), &cb); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if cb.RepoOwner != "acme" || cb.RepoName != "widgets" || cb.PRNumber != 5 {
		t.Errorf("unexpected repo/pr: %+v", cb)
	}
	if cb.Trigger != "checks_passed" || cb.TargetChatID != "chat-1" {
		t.Errorf("unexpected trigger/chat: %+v", cb)
	}
	if strings.Contains(res.Stdout, "ping me") {
		t.Fatalf("SECRET LEAK: message body appeared in JSON output\n%s", res.Stdout)
	}
}

// TestCLI_Callback_Add_BareNumber_NeedsContext asserts that a bare PR number with
// no repository context (no --repo, no ambient repo) fails with an actionable
// error rather than guessing.
func TestCLI_Callback_Add_BareNumber_NeedsContext(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add", "123", "merged",
		"--chat", "chat-1",
		"--message", "hi",
	)

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for bare number without repo context; stdout=%q", res.Stdout)
	}
	if !strings.Contains(strings.ToLower(res.Stderr), "repo") {
		t.Errorf("stderr should explain repository context is needed, got %q", res.Stderr)
	}
	if n := len(h.Daemon.CreateGithubCallbackCalls()); n != 0 {
		t.Errorf("no CreateGithubCallback call should be made on a validation failure, got %d", n)
	}
}

// TestCLI_Callback_Add_BareNumber_WithRepoFlag lets --repo supply the missing
// context for a bare PR number.
func TestCLI_Callback_Add_BareNumber_WithRepoFlag(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add", "123", "merged",
		"--chat", "chat-1",
		"--repo", "Acme/Widgets",
		"--message", "hi",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateGithubCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateGithubCallback call, got %d", len(calls))
	}
	if calls[0].GetRepoOwner() != "acme" || calls[0].GetRepoName() != "widgets" || calls[0].GetPrNumber() != 123 {
		t.Errorf("unexpected repo/pr: owner=%q repo=%q pr=%d", calls[0].GetRepoOwner(), calls[0].GetRepoName(), calls[0].GetPrNumber())
	}
}

func TestCLI_Callback_Add_MissingChat(t *testing.T) {
	// No --chat and no BOSS_AGENT_SESSION_ID (the clitest harness strips it) must
	// fail with an actionable error and register nothing.
	h := clitest.New(t)
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/1", "merged",
		"--message", "hi",
	)

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with no target chat; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "--chat") {
		t.Errorf("stderr should tell the user to pass --chat, got %q", res.Stderr)
	}
	if n := len(h.Daemon.CreateGithubCallbackCalls()); n != 0 {
		t.Errorf("no CreateGithubCallback call should be made, got %d", n)
	}
}

func TestCLI_Callback_Add_DefaultChatFromEnv(t *testing.T) {
	h := clitest.New(t, clitest.WithEnv("BOSS_AGENT_SESSION_ID=ambient-chat"))
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/1", "merged",
		"--message", "hi",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateGithubCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateGithubCallback call, got %d", len(calls))
	}
	if calls[0].GetTargetChatId() != "ambient-chat" {
		t.Errorf("expected default chat from env, got %q", calls[0].GetTargetChatId())
	}
}

func TestCLI_Callback_Add_MissingMessage(t *testing.T) {
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

func TestCLI_Callback_Add_InvalidTrigger(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/1", "exploded",
		"--chat", "chat-1",
		"--message", "hi",
	)

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for invalid trigger; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "trigger") {
		t.Errorf("stderr should mention the invalid trigger, got %q", res.Stderr)
	}
}

func TestCLI_Callback_Add_ExpiryTooLong(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("callback", "add",
		"https://github.com/acme/widgets/pull/1", "merged",
		"--chat", "chat-1",
		"--message", "hi",
		"--expires-in", "60d",
	)

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for expiry over 30d; stdout=%q", res.Stdout)
	}
	if n := len(h.Daemon.CreateGithubCallbackCalls()); n != 0 {
		t.Errorf("no CreateGithubCallback call should be made, got %d", n)
	}
}

// TestCLI_Callback_Add_AllTriggers registers a callback for every trigger the
// CLI accepts, including the two draft-aware additions (ready_for_review and
// checks_passed_ready), and asserts both that registration succeeds and that
// the human-readable confirmation line renders the expected natural-language
// clause for each.
func TestCLI_Callback_Add_AllTriggers(t *testing.T) {
	cases := []struct {
		trigger string
		label   string
	}{
		{"merged", "is merged"},
		{"closed", "is closed"},
		{"checks_passed", "passes checks"},
		{"checks_failed", "fails checks"},
		{"ready_for_review", "is ready for review"},
		{"checks_passed_ready", "passes checks while ready for review"},
	}
	for _, tc := range cases {
		t.Run(tc.trigger, func(t *testing.T) {
			h := clitest.New(t)
			res := h.Run("callback", "add",
				"https://github.com/acme/widgets/pull/1", tc.trigger,
				"--chat", "chat-1",
				"--message", "hi",
			)

			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			if !strings.Contains(res.Stdout, tc.label) {
				t.Errorf("stdout missing label %q\n%s", tc.label, res.Stdout)
			}

			calls := h.Daemon.CreateGithubCallbackCalls()
			if len(calls) != 1 {
				t.Fatalf("expected 1 CreateGithubCallback call, got %d", len(calls))
			}
			if calls[0].GetTrigger() != tc.trigger {
				t.Errorf("expected trigger %q sent to daemon, got %q", tc.trigger, calls[0].GetTrigger())
			}
		})
	}
}

func TestCLI_Callback_Remove(t *testing.T) {
	h := clitest.New(t, clitest.WithGithubCallbacks(testCallbacks()...))
	res := h.Run("callback", "remove", "cb-aaa", "--chat", "chat-1")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Removed callback cb-aaa") {
		t.Errorf("unexpected output: %q", res.Stdout)
	}
	calls := h.Daemon.DeleteGithubCallbackCalls()
	if len(calls) != 1 || calls[0] != "cb-aaa" {
		t.Errorf("expected one delete of cb-aaa, got %v", calls)
	}
}
