package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// broadcastSecret is the distinctive token the leak assertions grep for. It is
// unlike any field name or state string, so a match can only mean the body
// itself reached a surface it must never reach.
const broadcastSecret = "TOP-SECRET-CLI-BROADCAST-BODY-4b7e2d"

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

// TestJourney_Broadcasts_AC3_Broadcast_SubscriptionLifecycle drives subscribe → subscriptions →
// unsubscribe, and pins --session defaulting to the ambient session.
func TestJourney_Broadcasts_AC3_Broadcast_SubscriptionLifecycle(t *testing.T) {
	h := clitest.New(t, append(broadcastFixtures(), clitest.WithEnv("BOSS_SESSION_ID=broadcast-session"))...)

	sub := h.Run("broadcast", "subscribe", "--on", "completed",
		"--to", "chat:chat-a", "--message", broadcastSecret, "--json")
	if !sub.Success() {
		t.Fatalf("subscribe failed (%d): %s", sub.ExitCode, sub.Stderr)
	}
	var created broadcastSubscriptionJSON
	if err := json.Unmarshal([]byte(sub.Stdout), &created); err != nil {
		t.Fatalf("unmarshal subscribe output: %v\n%s", err, sub.Stdout)
	}
	if created.OwnerSessionID != h.SessionIDs["broadcast-session"] {
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
	if len(remaining) != 1 || remaining[0].ID != created.ID || remaining[0].State != "canceled" {
		t.Fatalf("unsubscribe should retain the canceled rule as history, got %s", after.Stdout)
	}
}

// TestJourney_Broadcasts_AC2_Broadcast_NeverLeaksMessageBody is the acceptance criterion the plan
// calls the single most important test: the body must appear in NO human
// output, NO --json output and NO error text, across all four surfaces that
// accept one.
func TestJourney_Broadcasts_AC2_Broadcast_NeverLeaksMessageBody(t *testing.T) {
	h := clitest.New(t, append(broadcastFixtures(), clitest.WithEnv("BOSS_SESSION_ID=broadcast-session", "BOSS_AGENT_SESSION_ID=chat-origin"))...)

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

	stdin := h.RunWithStdin(broadcastSecret+"\nsecond line\n", "broadcast", "send", "--to", "chat:chat-a", "--message", "-", "--json")
	if !stdin.Success() || strings.Contains(stdin.Stdout+stdin.Stderr, broadcastSecret) {
		t.Fatalf("stdin send failed or leaked body: %+v", stdin)
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

	// Confirm registration succeeded on the real daemon. Read APIs intentionally
	// redact message bodies, so the stored rows prove these surfaces were used.
	sends := storedBroadcasts(t, h)
	if len(sends) == 0 {
		t.Fatal("no broadcast persisted; the leak assertions would be vacuous")
	}

	subs := storedBroadcastSubscriptions(t, h)
	if len(subs) == 0 {
		t.Fatal("no subscription was persisted; assertions would be vacuous")
	}
}

func broadcastFixtures() []clitest.Option {
	return []clitest.Option{
		clitest.WithRepos(&pb.Repo{Id: "broadcast-repo", DisplayName: "broadcasts"}),
		clitest.WithSessions(&pb.Session{Id: "broadcast-session", RepoId: "broadcast-repo", Title: "broadcast targets", State: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN}),
		clitest.WithChats(&pb.AgentChat{Id: "chat-a", SessionId: "broadcast-session", AgentSessionId: "chat-a", AgentName: "stub", Title: "target a"}, &pb.AgentChat{Id: "chat-b", SessionId: "broadcast-session", AgentSessionId: "chat-b", AgentName: "stub", Title: "target b"}),
	}
}
func storedBroadcasts(t *testing.T, h *clitest.Harness) []*pb.Broadcast {
	t.Helper()
	res, err := h.Stack.Client().ListBroadcasts(t.Context(), realstackrpc.Request(&pb.ListBroadcastsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Broadcasts
}
func storedBroadcastSubscriptions(t *testing.T, h *clitest.Harness) []*pb.BroadcastSubscription {
	t.Helper()
	res, err := h.Stack.Client().ListBroadcastSubscriptions(t.Context(), realstackrpc.Request(&pb.ListBroadcastSubscriptionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Subscriptions
}
