package session

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type statusJSONRow struct {
	AgentSessionID string `json:"agent_session_id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"created_at"`
	Status         string `json:"status"`
	LastOutputAt   string `json:"last_output_at"`
	WaitingReason  string `json:"waiting_reason"`
	Phase          string `json:"phase"`
	// The three liveness discriminators the daemon computes on
	// ChatStatusEntry. Declared here rather than shared with the CLI's own
	// struct: this copy is the wire contract a driver reads, so a rename in the
	// CLI must break this test rather than travel silently through it.
	SpinnerPresent          bool   `json:"spinner_present"`
	LastSubstantiveOutputAt string `json:"last_substantive_output_at"`
	LastOutputSeeded        bool   `json:"last_output_seeded"`
}

type chatsEnvelope struct {
	Chats []statusJSONRow `json:"chats"`
}

// chatsErrorEnvelope is the failure half of the contract.
type chatsErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

const (
	chatsSessionID = "sess-aaa-111"
	idleChatID     = "agent-idle-1"
	workingChatID  = "agent-working-2"
	waitingChatID  = "agent-waiting-3"
	unknownChatID  = "agent-unknown-4"
)

var chatsLastOutput = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// chatsLastSubstantive is deliberately EARLIER than chatsLastOutput: that gap is
// the whole point of the discriminator. A spinner redraw advances last_output_at
// while last_substantive_output_at stands still, so a fixture where the two are
// equal could not tell the fields apart.
var chatsLastSubstantive = time.Date(2026, 3, 4, 4, 0, 0, 0, time.UTC)

// chatsHarness seeds one session with four chats: three the status fixture
// covers (IDLE, WORKING, WAITING) and one it deliberately does not, so the
// no-cached-status row is exercised by every case rather than only its own.
func chatsHarness(t *testing.T) *statusCommandHarness {
	t.Helper()
	h := &statusCommandHarness{t: t, client: &statusCommandClient{}}
	for i, id := range []string{idleChatID, workingChatID, waitingChatID, unknownChatID} {
		h.client.chats = append(h.client.chats, &pb.AgentChat{Id: id, SessionId: chatsSessionID, AgentSessionId: id, Title: []string{"Implement the parser", "Fix the flake", "Wait on CI", "No heartbeat yet"}[i], CreatedAt: timestamppb.New(chatsLastOutput)})
	}
	h.client.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId:          idleChatID,
		Status:                  pb.ChatStatus_CHAT_STATUS_IDLE,
		LastOutputAt:            timestamppb.New(chatsLastOutput),
		LastSubstantiveOutputAt: timestamppb.New(chatsLastSubstantive),
	})
	// The spinning-but-idle pane: a live spinner keeps last_output_at fresh
	// while nothing substantive has EVER been observed, so the seed flag is
	// still set. This is the shape a settled-green driver must not read as work.
	h.client.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId:   workingChatID,
		Status:           pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:     timestamppb.New(chatsLastOutput),
		SpinnerPresent:   true,
		LastOutputSeeded: true,
		Phase:            "reviewing",
	})
	h.client.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: waitingChatID,
		Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
		LastOutputAt:   timestamppb.New(chatsLastOutput),
		WaitingReason:  "awaiting checks_passed_ready on acme/app#42",
	})
	return h
}
