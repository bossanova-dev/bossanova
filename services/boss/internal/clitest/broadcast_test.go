package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

// broadcastSecret is the distinctive token the leak assertions grep for. It is
// unlike any field name or state string, so a match can only mean the body
// itself reached a surface it must never reach.
const broadcastSecret = "TOP-SECRET-CLI-BROADCAST-BODY-4b7e2d"

// broadcastJSON mirrors the stable schema emitted by
// `boss broadcast send|ls --json`. The message body is a secret and is
// deliberately absent from this schema — the tests below assert it never
// appears in any output.
type broadcastJSON struct {
	ID           string                  `json:"id"`
	OriginChatID string                  `json:"origin_chat_id"`
	Selector     string                  `json:"selector"`
	State        string                  `json:"state"`
	TargetCount  int32                   `json:"target_count"`
	Deliveries   []broadcastDeliveryJSON `json:"deliveries,omitempty"`
	ExpiresAt    string                  `json:"expires_at"`
	CreatedAt    string                  `json:"created_at"`
}

type broadcastDeliveryJSON struct {
	TargetChatID string `json:"target_chat_id"`
	State        string `json:"state"`
	AttemptCount int32  `json:"attempt_count"`
	LastError    string `json:"last_error"`
	DeliveredAt  string `json:"delivered_at"`
}

type broadcastSubscriptionJSON struct {
	ID               string `json:"id"`
	OwnerSessionID   string `json:"owner_session_id"`
	OriginChatID     string `json:"origin_chat_id"`
	TriggerEvent     string `json:"trigger_event"`
	Selector         string `json:"selector"`
	State            string `json:"state"`
	FiredBroadcastID string `json:"fired_broadcast_id"`
	FiredAt          string `json:"fired_at"`
	ExpiresAt        string `json:"expires_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// TestCLI_Broadcast_SendListRemove drives the full send → ls → rm loop against
// the mock daemon, which is the round trip an operator or agent actually runs.
func TestCLI_Broadcast_SendListRemove(t *testing.T) {
	h := clitest.New(t)

	send := h.Run("broadcast", "send", "--to", "chat:chat-a,chat:chat-b",
		"--message", broadcastSecret, "--json")
	if !send.Success() {
		t.Fatalf("send failed (%d): %s", send.ExitCode, send.Stderr)
	}
	var sent broadcastJSON
	if err := json.Unmarshal([]byte(send.Stdout), &sent); err != nil {
		t.Fatalf("unmarshal send output: %v\n%s", err, send.Stdout)
	}
	if sent.ID == "" {
		t.Fatalf("send emitted no broadcast id: %s", send.Stdout)
	}
	if sent.TargetCount != 2 || len(sent.Deliveries) != 2 {
		t.Fatalf("expected 2 resolved targets, got %d: %s", sent.TargetCount, send.Stdout)
	}

	list := h.Run("broadcast", "ls", "--json")
	if !list.Success() {
		t.Fatalf("ls failed (%d): %s", list.ExitCode, list.Stderr)
	}
	var listed []broadcastJSON
	if err := json.Unmarshal([]byte(list.Stdout), &listed); err != nil {
		t.Fatalf("unmarshal ls output: %v\n%s", err, list.Stdout)
	}
	if len(listed) != 1 || listed[0].ID != sent.ID {
		t.Fatalf("ls should return the sent broadcast, got %s", list.Stdout)
	}

	rm := h.Run("broadcast", "rm", sent.ID)
	if !rm.Success() {
		t.Fatalf("rm failed (%d): %s", rm.ExitCode, rm.Stderr)
	}

	after := h.Run("broadcast", "ls", "--json")
	var remaining []broadcastJSON
	if err := json.Unmarshal([]byte(after.Stdout), &remaining); err != nil {
		t.Fatalf("unmarshal ls output: %v\n%s", err, after.Stdout)
	}
	if len(remaining) != 0 {
		t.Fatalf("rm should have removed the broadcast, got %s", after.Stdout)
	}
}

// TestCLI_Broadcast_RemoveUnknownIDExitsZero pins the documented idempotency:
// removing an id that does not exist succeeds quietly.
func TestCLI_Broadcast_RemoveUnknownIDExitsZero(t *testing.T) {
	h := clitest.New(t)

	rm := h.Run("broadcast", "rm", "bc-does-not-exist")
	if !rm.Success() {
		t.Fatalf("rm of an unknown id must exit zero, got %d: %s", rm.ExitCode, rm.Stderr)
	}

	unsub := h.Run("broadcast", "unsubscribe", "bsub-does-not-exist")
	if !unsub.Success() {
		t.Fatalf("unsubscribe of an unknown id must exit zero, got %d: %s", unsub.ExitCode, unsub.Stderr)
	}
}

// TestCLI_Broadcast_InvalidSelectorFailsBeforeDaemon asserts the parse error
// surfaces verbatim and non-zero. The mock daemon records every SendBroadcast
// it receives, so an empty call log proves no daemon round trip happened.
func TestCLI_Broadcast_InvalidSelectorFailsBeforeDaemon(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("broadcast", "send", "--to", "bogus:x", "--message", broadcastSecret)
	if res.Success() {
		t.Fatalf("an invalid selector must exit non-zero: %s", res.Stdout)
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "unknown key") || !strings.Contains(combined, "valid keys are") {
		t.Fatalf("expected the parser's own message, got: %s", combined)
	}
	if strings.Contains(combined, broadcastSecret) {
		t.Fatalf("the selector error leaked the message body: %s", combined)
	}
	if calls := h.Daemon.SendBroadcastCalls(); len(calls) != 0 {
		t.Fatalf("an invalid selector must not reach the daemon, got %d call(s)", len(calls))
	}
}

// TestCLI_Broadcast_MessageFromStdin covers `--message -`, the way an agent
// sends a multi-line body without shell-quoting hazards.
func TestCLI_Broadcast_MessageFromStdin(t *testing.T) {
	h := clitest.New(t)

	body := broadcastSecret + "\nsecond line\n"
	res := h.RunWithStdin(body, "broadcast", "send", "--to", "chat:chat-a", "--message", "-", "--json")
	if !res.Success() {
		t.Fatalf("send failed (%d): %s", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.SendBroadcastCalls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one SendBroadcast, got %d", len(calls))
	}
	if got := calls[0].GetMessage(); got != body {
		t.Fatalf("stdin body not transmitted verbatim: got %q, want %q", got, body)
	}
	// Transmitted, but never echoed.
	if strings.Contains(res.Stdout+res.Stderr, broadcastSecret) {
		t.Fatalf("send output leaked the stdin body: %s", res.Stdout+res.Stderr)
	}
}

// TestCLI_Broadcast_OriginResolution covers the --from precedence chain end to
// end, including the case the plan calls out explicitly: with neither flag nor
// ambient chat, the send still SUCCEEDS with no origin.
func TestCLI_Broadcast_OriginResolution(t *testing.T) {
	t.Run("explicit --from wins over the ambient chat", func(t *testing.T) {
		h := clitest.New(t, clitest.WithEnv("BOSS_AGENT_SESSION_ID=chat-ambient"))
		res := h.Run("broadcast", "send", "--to", "chat:chat-a",
			"--message", broadcastSecret, "--from", "chat-explicit")
		if !res.Success() {
			t.Fatalf("send failed (%d): %s", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.SendBroadcastCalls()
		if len(calls) != 1 || calls[0].GetOriginChatId() != "chat-explicit" {
			t.Fatalf("origin = %q, want chat-explicit", calls[0].GetOriginChatId())
		}
	})

	t.Run("defaults to BOSS_AGENT_SESSION_ID", func(t *testing.T) {
		h := clitest.New(t, clitest.WithEnv("BOSS_AGENT_SESSION_ID=chat-ambient"))
		res := h.Run("broadcast", "send", "--to", "chat:chat-a", "--message", broadcastSecret)
		if !res.Success() {
			t.Fatalf("send failed (%d): %s", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.SendBroadcastCalls()
		if len(calls) != 1 || calls[0].GetOriginChatId() != "chat-ambient" {
			t.Fatalf("origin = %q, want chat-ambient", calls[0].GetOriginChatId())
		}
	})

	t.Run("with neither, the send succeeds with no origin", func(t *testing.T) {
		h := clitest.New(t)
		res := h.Run("broadcast", "send", "--to", "chat:chat-a", "--message", broadcastSecret)
		if !res.Success() {
			t.Fatalf("a send with no origin must succeed, got %d: %s", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.SendBroadcastCalls()
		if len(calls) != 1 || calls[0].GetOriginChatId() != "" {
			t.Fatalf("origin = %q, want empty", calls[0].GetOriginChatId())
		}
	})
}

// TestCLI_Broadcast_SendPrintsTargetTable covers the safety affordance: human
// output names the broadcast and every resolved target, so a too-broad selector
// is visible before anything else happens.
func TestCLI_Broadcast_SendPrintsTargetTable(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("broadcast", "send", "--to", "chat:chat-a,chat:chat-b", "--message", broadcastSecret)
	if !res.Success() {
		t.Fatalf("send failed (%d): %s", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"bc-1", "TARGET CHAT", "chat-a", "chat-b"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("send output missing %q:\n%s", want, res.Stdout)
		}
	}
}

// TestCLI_Broadcast_SubscriptionLifecycle drives subscribe → subscriptions →
// unsubscribe, and pins --session defaulting to the ambient session.
func TestCLI_Broadcast_SubscriptionLifecycle(t *testing.T) {
	h := clitest.New(t, clitest.WithEnv("BOSS_SESSION_ID=sess-ambient"))

	sub := h.Run("broadcast", "subscribe", "--on", "completed",
		"--to", "chat:chat-a", "--message", broadcastSecret, "--json")
	if !sub.Success() {
		t.Fatalf("subscribe failed (%d): %s", sub.ExitCode, sub.Stderr)
	}
	var created broadcastSubscriptionJSON
	if err := json.Unmarshal([]byte(sub.Stdout), &created); err != nil {
		t.Fatalf("unmarshal subscribe output: %v\n%s", err, sub.Stdout)
	}
	if created.OwnerSessionID != "sess-ambient" {
		t.Fatalf("--session should default to the ambient session, got %q", created.OwnerSessionID)
	}
	if created.TriggerEvent != "completed" {
		t.Fatalf("trigger_event = %q", created.TriggerEvent)
	}

	list := h.Run("broadcast", "subscriptions", "--json")
	if !list.Success() {
		t.Fatalf("subscriptions failed (%d): %s", list.ExitCode, list.Stderr)
	}
	var listed []broadcastSubscriptionJSON
	if err := json.Unmarshal([]byte(list.Stdout), &listed); err != nil {
		t.Fatalf("unmarshal subscriptions output: %v\n%s", err, list.Stdout)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("subscriptions should return the created rule, got %s", list.Stdout)
	}

	if res := h.Run("broadcast", "unsubscribe", created.ID); !res.Success() {
		t.Fatalf("unsubscribe failed (%d): %s", res.ExitCode, res.Stderr)
	}
	after := h.Run("broadcast", "subscriptions", "--json")
	var remaining []broadcastSubscriptionJSON
	if err := json.Unmarshal([]byte(after.Stdout), &remaining); err != nil {
		t.Fatalf("unmarshal subscriptions output: %v\n%s", err, after.Stdout)
	}
	if len(remaining) != 0 {
		t.Fatalf("unsubscribe should have retired the rule, got %s", after.Stdout)
	}
}

// TestCLI_Broadcast_SubscribeRequiresSession pins the one place absence IS an
// error: a subscription with no owning session could never fire.
func TestCLI_Broadcast_SubscribeRequiresSession(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("broadcast", "subscribe", "--on", "completed",
		"--to", "chat:chat-a", "--message", broadcastSecret)
	if res.Success() {
		t.Fatal("subscribe with no session must exit non-zero")
	}
	if !strings.Contains(res.Stdout+res.Stderr, "--session") {
		t.Fatalf("error should name --session: %s", res.Stdout+res.Stderr)
	}
}

// TestCLI_Broadcast_NeverLeaksMessageBody is the acceptance criterion the plan
// calls the single most important test: the body must appear in NO human
// output, NO --json output and NO error text, across all four surfaces that
// accept one.
func TestCLI_Broadcast_NeverLeaksMessageBody(t *testing.T) {
	h := clitest.New(t, clitest.WithEnv("BOSS_SESSION_ID=sess-1", "BOSS_AGENT_SESSION_ID=chat-origin"))

	surfaces := []struct {
		name string
		args []string
	}{
		{"send human", []string{"broadcast", "send", "--to", "chat:chat-a", "--message", broadcastSecret}},
		{"send json", []string{"broadcast", "send", "--to", "chat:chat-a", "--message", broadcastSecret, "--json"}},
		{"subscribe human", []string{"broadcast", "subscribe", "--on", "completed", "--to", "chat:chat-a", "--message", broadcastSecret}},
		{"subscribe json", []string{"broadcast", "subscribe", "--on", "completed", "--to", "chat:chat-a", "--message", broadcastSecret, "--json"}},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			res := h.Run(s.args...)
			if !res.Success() {
				t.Fatalf("%s failed (%d): %s", s.name, res.ExitCode, res.Stderr)
			}
			if strings.Contains(res.Stdout, broadcastSecret) {
				t.Fatalf("%s leaked the body on stdout:\n%s", s.name, res.Stdout)
			}
			if strings.Contains(res.Stderr, broadcastSecret) {
				t.Fatalf("%s leaked the body on stderr:\n%s", s.name, res.Stderr)
			}
		})
	}

	// The read surfaces must not leak it either, now that bodies have been sent.
	for _, args := range [][]string{
		{"broadcast", "ls"},
		{"broadcast", "ls", "--json"},
		{"broadcast", "subscriptions"},
		{"broadcast", "subscriptions", "--json"},
	} {
		res := h.Run(args...)
		if !res.Success() {
			t.Fatalf("%v failed (%d): %s", args, res.ExitCode, res.Stderr)
		}
		if strings.Contains(res.Stdout+res.Stderr, broadcastSecret) {
			t.Fatalf("%v leaked the body:\n%s", args, res.Stdout+res.Stderr)
		}
	}

	// Guard against a vacuous pass: the bodies really were transmitted, so the
	// assertions above mean the CLI dropped them rather than never having had
	// them.
	sends := h.Daemon.SendBroadcastCalls()
	if len(sends) == 0 {
		t.Fatal("no SendBroadcast reached the daemon; the leak assertions would be vacuous")
	}
	if sends[0].GetMessage() != broadcastSecret {
		t.Fatalf("the daemon did not receive the body: %q", sends[0].GetMessage())
	}
	subs := h.Daemon.CreateBroadcastSubscriptionCalls()
	if len(subs) == 0 || subs[0].GetMessage() != broadcastSecret {
		t.Fatal("the daemon did not receive the subscription body; assertions would be vacuous")
	}
}

// TestCLI_Broadcast_Send_CrossDaemonFlag proves `--cross-daemon` actually
// reaches the daemon on SendBroadcastRequest.cross_daemon, and that omitting it
// leaves the field false.
//
// This is the flag's ONLY consumer check. The daemon gates its whole
// cross-daemon egress path on cross_daemon (services/bossd/internal/server/
// send_broadcast.go), so a CLI that accepted the flag and dropped it would
// leave that path permanently unreachable while looking wired — the exact
// defect this test exists to prevent. Naming another daemon in the selector is
// NOT a substitute: local chat rows carry an empty daemon id, so a
// `daemon:<other-id>` term resolves to zero targets.
func TestCLI_Broadcast_Send_CrossDaemonFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"flag set", []string{"--cross-daemon"}, true},
		{"flag omitted", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := clitest.New(t)
			args := append([]string{"broadcast", "send", "--to", "chat:chat-a",
				"--message", broadcastSecret, "--json"}, tc.args...)
			if res := h.Run(args...); !res.Success() {
				t.Fatalf("send failed (%d): %s", res.ExitCode, res.Stderr)
			}
			calls := h.Daemon.SendBroadcastCalls()
			if len(calls) != 1 {
				t.Fatalf("expected exactly 1 SendBroadcast call, got %d", len(calls))
			}
			if got := calls[0].GetCrossDaemon(); got != tc.want {
				t.Errorf("cross_daemon = %v, want %v", got, tc.want)
			}
		})
	}
}
