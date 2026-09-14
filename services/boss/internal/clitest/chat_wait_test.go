package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// chatWaitEnvelope mirrors the stable `boss chat wait --json` schema. Declared
// here rather than shared with the CLI's own struct on purpose: this copy is
// the wire contract a driver branches on, so a rename in the CLI must break
// this test rather than travel silently through it.
type chatWaitEnvelope struct {
	ChatID   string `json:"chat_id"`
	Timeout  string `json:"timeout"`
	TimedOut bool   `json:"timed_out"`
	Result   string `json:"result"`
	Liveness struct {
		Known                   bool   `json:"known"`
		Status                  string `json:"status"`
		SpinnerPresent          bool   `json:"spinner_present"`
		LastOutputAt            string `json:"last_output_at"`
		LastSubstantiveOutputAt string `json:"last_substantive_output_at"`
		LastOutputSeeded        bool   `json:"last_output_seeded"`
	} `json:"liveness"`
}

const (
	waitSessionID = "sess-wait-1"
	waitChatID    = "agent-wait-1"
)

// chatWaitHarness seeds one session whose primary chat never finishes. status
// governs whether the pane looks spinning-but-idle or genuinely productive.
func chatWaitHarness(t *testing.T, status *pb.ChatStatusEntry) *clitest.Harness {
	t.Helper()
	h := clitest.New(t,
		clitest.WithSessions(&pb.Session{
			Id:             waitSessionID,
			RepoId:         "repo-1",
			Title:          "Never settles",
			AgentSessionId: proto.String(waitChatID),
			State:          pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		}),
		clitest.WithChats(&pb.ClaudeChat{
			Id: "chat-w", SessionId: waitSessionID, AgentSessionId: waitChatID, Title: "Never settles",
		}),
	)
	h.Daemon.AddChatStatus(status)
	// Exists with an empty final text: the chat is readable but has produced no
	// answer, so wait keeps polling until --timeout rather than returning early.
	h.Daemon.SetChatTranscript(&pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: ""})
	return h
}

var waitLastOutput = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

// TestCLI_ChatWait_JSONTimeoutNamesSpinnerState is the discrimination the
// envelope exists for: a pane with a live spinner whose substantive timestamp
// has never advanced is NOT the same thing as one still producing output, and
// before this a 30-minute wait ended with a sentence that said only that it had
// ended.
func TestCLI_ChatWait_JSONTimeoutNamesSpinnerState(t *testing.T) {
	h := chatWaitHarness(t, &pb.ChatStatusEntry{
		AgentSessionId:   waitChatID,
		Status:           pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:     timestamppb.New(waitLastOutput),
		SpinnerPresent:   true,
		LastOutputSeeded: true,
	})
	res := h.Run("chat", "wait", waitSessionID, "--timeout", "1s", "--json")

	if res.ExitCode == 0 {
		t.Fatalf("a timeout must still exit non-zero; stdout=%q", res.Stdout)
	}
	var env chatWaitEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not a single JSON envelope: %v (stdout=%q)", err, res.Stdout)
	}
	if !env.TimedOut {
		t.Error("timed_out = false on a timeout")
	}
	// The RESOLVED chat id, never the argument the caller typed: `wait` accepts
	// <session-id|chat-id>, and this invocation passed a SESSION id. A driver
	// joins this field against `boss chats --json`, so echoing the argument back
	// would publish a session id under a key named chat_id.
	if env.ChatID != waitChatID {
		t.Errorf("chat_id = %q, want the resolved chat id %q", env.ChatID, waitChatID)
	}
	if env.Timeout != "1s" {
		t.Errorf("timeout = %q, want 1s", env.Timeout)
	}
	if !env.Liveness.Known {
		t.Fatal("liveness.known = false; the status read should have covered this chat")
	}
	if !env.Liveness.SpinnerPresent {
		t.Error("liveness.spinner_present = false, want true")
	}
	if !env.Liveness.LastOutputSeeded {
		t.Error("liveness.last_output_seeded = false, want true — nothing substantive was ever observed")
	}
	if env.Liveness.LastSubstantiveOutputAt != "" {
		t.Errorf("last_substantive_output_at = %q, want empty", env.Liveness.LastSubstantiveOutputAt)
	}
	if env.Liveness.LastOutputAt == "" {
		t.Error("last_output_at should be carried — the spinner is what advanced it")
	}
	if env.Liveness.Status != "WORKING" {
		t.Errorf("liveness.status = %q, want WORKING", env.Liveness.Status)
	}
	// The human channel stays human: the prose explains the wait, and stdout
	// stays exactly one JSON object.
	if !strings.Contains(res.Stderr, "spinner") {
		t.Errorf("stderr should explain the wait in terms of the spinner, got %q", res.Stderr)
	}
}

// TestCLI_ChatWait_JSONTimeoutDistinguishesProductiveChat is the contrast case.
// Same timeout, same exit status, different envelope — which is the whole point
// of carrying the discriminators rather than only the fact of the timeout.
func TestCLI_ChatWait_JSONTimeoutDistinguishesProductiveChat(t *testing.T) {
	substantive := waitLastOutput.Add(-2 * time.Minute)
	h := chatWaitHarness(t, &pb.ChatStatusEntry{
		AgentSessionId:          waitChatID,
		Status:                  pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:            timestamppb.New(waitLastOutput),
		LastSubstantiveOutputAt: timestamppb.New(substantive),
		SpinnerPresent:          true,
		LastOutputSeeded:        false,
	})
	res := h.Run("chat", "wait", waitSessionID, "--timeout", "1s", "--json")

	if res.ExitCode == 0 {
		t.Fatalf("a timeout must still exit non-zero; stdout=%q", res.Stdout)
	}
	var env chatWaitEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not a single JSON envelope: %v (stdout=%q)", err, res.Stdout)
	}
	if env.Liveness.LastOutputSeeded {
		t.Error("last_output_seeded = true, want false once a substantive observation landed")
	}
	if want := substantive.Format(time.RFC3339); env.Liveness.LastSubstantiveOutputAt != want {
		t.Errorf("last_substantive_output_at = %q, want %q", env.Liveness.LastSubstantiveOutputAt, want)
	}
	if !strings.Contains(res.Stderr, "last substantive output was at") {
		t.Errorf("stderr should date the last substantive output, got %q", res.Stderr)
	}
}

// TestCLI_ChatWait_TimeoutExplainsItselfWithoutJSON proves the human path gained
// the same explanation, so a person reading a timeout is no longer told only
// that it happened.
func TestCLI_ChatWait_TimeoutExplainsItselfWithoutJSON(t *testing.T) {
	h := chatWaitHarness(t, &pb.ChatStatusEntry{
		AgentSessionId:   waitChatID,
		Status:           pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:     timestamppb.New(waitLastOutput),
		SpinnerPresent:   true,
		LastOutputSeeded: true,
	})
	res := h.Run("chat", "wait", waitSessionID, "--timeout", "1s")

	if res.ExitCode == 0 {
		t.Fatalf("a timeout must exit non-zero; stdout=%q", res.Stdout)
	}
	for _, want := range []string{"timed out waiting for chat", "a live spinner is present", "still the daemon's seed"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr missing %q; got %q", want, res.Stderr)
		}
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("stdout must stay empty without --json, got %q", res.Stdout)
	}
}

// TestCLI_ChatWait_JSONSuccessEnvelope covers the settled outcome: the same
// schema, timed_out false, and the result carried in the envelope rather than
// printed bare.
func TestCLI_ChatWait_JSONSuccessEnvelope(t *testing.T) {
	h := chatWaitHarness(t, &pb.ChatStatusEntry{
		AgentSessionId:          waitChatID,
		Status:                  pb.ChatStatus_CHAT_STATUS_IDLE,
		LastOutputAt:            timestamppb.New(waitLastOutput),
		LastSubstantiveOutputAt: timestamppb.New(waitLastOutput),
	})
	h.Daemon.SetChatTranscript(&pb.GetChatTranscriptResponse{
		Exists: true, FinalAssistantText: "the parser now handles trailing commas",
	})
	res := h.Run("chat", "wait", waitSessionID, "--timeout", "20s", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var env chatWaitEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not a single JSON envelope: %v (stdout=%q)", err, res.Stdout)
	}
	if env.TimedOut {
		t.Error("timed_out = true on a settled chat")
	}
	if env.Result != "the parser now handles trailing commas" {
		t.Errorf("result = %q", env.Result)
	}
	if env.Liveness.Status != "IDLE" {
		t.Errorf("liveness.status = %q, want IDLE", env.Liveness.Status)
	}
}
