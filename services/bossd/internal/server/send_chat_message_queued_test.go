package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/chatdelivery"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// The two panes below are structural models of the REAL captures that live with
// the verifier that reads them (internal/tmux/testdata/panes/claude_*_real.txt,
// claude 2.1.220 — see that directory's capture.sh). They are copied rather than
// read across the package boundary, because reaching for another package's
// testdata costs a hand-written bazel data dep that gazelle then fights; the
// assertions ABOUT the captured bytes belong beside the captures and live there
// (tmux_submit_verify_queued_test.go). What this file asserts is the layer
// above: that each pane shape resolves to the right RPC response.
//
// Two details are reproduced deliberately, because they are what makes these
// models of a capture rather than of a plan's prose:
//
//   - The live composer separates its glyph from its text with a NON-BREAKING
//     space (U+00A0). A hand-authored fixture uses a plain space and never
//     notices.
//   - A queued send CLEARS the composer. The payload is echoed ABOVE the input
//     box with its own glyph, and the box is left showing the placeholder hint.
//     The pre-capture revision of this file asserted the opposite — payload
//     still in the composer — which is the swallowed-Enter shape, not a queued
//     one.
const (
	queuedPayload = "also update the changelog"

	// queuedPane: mid-turn, Enter ACCEPTED. Composer released the payload into
	// the agent's queue.
	queuedPane = "⏺ Done — I updated the three call sites and re-ran the suite.\n" +
		"\n" +
		"  ❯ also update the changelog\n" +
		"\n" +
		"❯\u00a0Press up to edit queued messages\n" +
		"  ? for shortcuts\n"

	// swallowedEnterPane: mid-turn, Enter SWALLOWED. Composer still holds the
	// payload and there is no queue anywhere — the message is NOT delivered.
	swallowedEnterPane = "⏺ Done — I updated the three call sites and re-ran the suite.\n" +
		"\n" +
		"✳ Blanching… (1m 37s · ↓ 244 tokens)\n" +
		"\n" +
		"❯\u00a0also update the changelog\n" +
		"  ? for shortcuts\n"
)

// newQueuedTestServer wires a Server whose tmux surface is the real client
// driven by the given pane. registerRunner controls whether an agent runner is
// registered at all, which is how the tests pin that the queued verdict no
// longer depends on the plugin registry.
func newQueuedTestServer(t *testing.T, pane string, registerRunner bool) *Server {
	t.Helper()
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux: tmux.NewClient(tmux.WithCommandFactory(verifierPaneFactory(func() *exec.Cmd {
			return exec.CommandContext(context.Background(), "printf", "%s", pane)
		}))),
	}
	if registerRunner {
		s.agentClients = map[string]agent.AgentRunnerClient{"claude": &agentClientStub{}}
	}
	return s
}

// agentClientStub registers a runner that answers exactly one call: the
// readiness gate's modal check. The embedded nil interface is still there and
// still panics on every OTHER method, which is the assertion BOS-599 wrote it
// for — a future change that reintroduces some further agent round-trip on the
// delivery path fails loudly rather than silently paying for it.
//
// HasQuestionPrompt is the one exception because BOS-600 added it deliberately:
// the gate asks the agent "is this pane a menu?" once per capture before
// anything is typed, so a selection UI is never delivered into. That call is
// documented at modalDetectorFor. Answering blocks_input=false is what these
// panes ARE — a live composer, mid-turn — so the queued path proceeds exactly as
// it did before the gate existed, and this file keeps asserting the pane→RPC
// mapping rather than the modal behaviour (send_chat_message_modal_test.go owns
// that).
type agentClientStub struct{ agent.AgentRunnerClient }

func (*agentClientStub) HasQuestionPrompt(context.Context, *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	return &pb.HasQuestionPromptResponse{}, nil
}

func sendQueuedPayload(t *testing.T, s *Server) (*connect.Response[pb.SendChatMessageResponse], error) {
	t.Helper()
	return s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        queuedPayload,
		Submit:         true,
	}))
}

// TestSendChatMessage_QueuedMidTurn_ReportsDelivered is BOS-599's headline
// behaviour at the RPC layer: a message the agent accepts into its queue while
// a turn runs is DELIVERED, and the caller is told not to resend it.
func TestSendChatMessage_QueuedMidTurn_ReportsDelivered(t *testing.T) {
	resp, err := sendQueuedPayload(t, newQueuedTestServer(t, queuedPane, true))
	if err != nil {
		t.Fatalf("expected a response, got error: %v (code %v)", err, connect.CodeOf(err))
	}
	if !resp.Msg.GetDelivered() {
		t.Error("delivered = false, want true: the agent has the message")
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_QUEUED", got)
	}

	notice := resp.Msg.GetNoticeText()
	// notice_text is the only field the hand-rolled converters forward
	// (services/boss/internal/client/remote.go,
	// services/mcp-gateway/internal/proxybackend/proxybackend.go both drop
	// delivery_state), so a proxied caller learns "queued, do not resend" from
	// here or nowhere.
	if !strings.Contains(notice, chatdelivery.QueuedGuidance) {
		t.Errorf("notice_text = %q, want it to carry the queued guidance", notice)
	}
	// And it must NOT carry the unconfirmed guidance, which says the opposite.
	// Both surfaces recover a dropped delivery_state by matching these
	// sentences, so a notice carrying both would render as "resend at your own
	// risk" for a message the agent already holds.
	if strings.Contains(notice, chatdelivery.ResendGuidance) {
		t.Errorf("notice_text = %q, must not carry the resend guidance for a queued delivery", notice)
	}
	if !strings.Contains(notice, resp.Msg.GetTmuxSessionName()) {
		t.Errorf("notice_text = %q, want it to name tmux session %q", notice, resp.Msg.GetTmuxSessionName())
	}
}

// TestSendChatMessage_SwallowedEnterMidTurn_StaysNotSubmitted is the safety
// half, and it is the regression test for the Critical this ticket closed. This
// pane is mid-turn — a spinner is on screen and the agent is genuinely busy on
// an EARLIER request — but the payload never left the composer. An earlier
// revision read exactly this pane as QUEUED, answered delivered=true with "do
// not resend", skipped the clear-and-re-deliver recovery, and dropped the
// message silently. It must stay a loud, retryable failure.
func TestSendChatMessage_SwallowedEnterMidTurn_StaysNotSubmitted(t *testing.T) {
	_, err := sendQueuedPayload(t, newQueuedTestServer(t, swallowedEnterPane, true))
	if err == nil {
		t.Fatal("expected an error for a payload still sitting in the composer, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), tmux.OutcomeNotSubmitted.String()) {
		t.Errorf("error %q does not name the delivery state %q", err.Error(), tmux.OutcomeNotSubmitted)
	}
	if strings.Contains(err.Error(), tmux.OutcomeQueued.String()) {
		t.Errorf("error %q reports a queued delivery for a payload the composer still holds", err.Error())
	}
}

// TestSendChatMessage_QueuedWithoutAgentRunner pins that the queued verdict is
// derived from the pane alone. The previous revision routed it through the
// chat's agent-runner plugin, which made delivery reporting depend on the
// plugin registry: a chat whose runner was not loaded could not be told its
// message had been queued. Nothing here registers a runner, and the verdict is
// unchanged.
func TestSendChatMessage_QueuedWithoutAgentRunner(t *testing.T) {
	resp, err := sendQueuedPayload(t, newQueuedTestServer(t, queuedPane, false))
	if err != nil {
		t.Fatalf("expected a response, got error: %v (code %v)", err, connect.CodeOf(err))
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_QUEUED with no agent runner loaded", got)
	}
	if !resp.Msg.GetDelivered() {
		t.Error("delivered = false, want true")
	}
}
