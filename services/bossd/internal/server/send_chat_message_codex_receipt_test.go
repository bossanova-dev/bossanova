package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

const codexReceiptRolloutID = "rollout-1"

// fakeRollout is a codex runner whose ReadTranscript serves an in-memory
// rollout. Every other AgentRunnerClient method panics via the embedded nil.
// A wedged rollout answers no read, ending each only at the caller's deadline as
// a gRPC client does against a stalled plugin.
type fakeRollout struct {
	agent.AgentRunnerClient
	mu     sync.Mutex
	exists bool
	wedged bool
	// keyedBy, when set, is the one id the rollout is found under; a read keyed
	// by any other id misses, as the codex plugin's filename match does.
	keyedBy string
	msgs    []*pb.ChatMessage
	reqs    []*pb.ReadTranscriptRequest
}

func (f *fakeRollout) ReadTranscript(ctx context.Context, req *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
	if f.wedged {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.keyedBy != "" && req.GetAgentSessionId() != f.keyedBy {
		return &pb.ReadTranscriptResponse{}, nil
	}
	return &pb.ReadTranscriptResponse{Exists: f.exists, Messages: append([]*pb.ChatMessage(nil), f.msgs...)}, nil
}

// HasQuestionPrompt answers the readiness gate's modal probe: no modal.
func (*fakeRollout) HasQuestionPrompt(context.Context, *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	return &pb.HasQuestionPromptResponse{}, nil
}

// ResolveInteractiveSessionID answers the post-send rollout discovery an
// unbound codex chat starts: nothing to bind yet.
func (*fakeRollout) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}

// recordUserTurn appends the user turn Codex writes when it takes a message.
func (f *fakeRollout) recordUserTurn() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, &pb.ChatMessage{Role: "user", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)})
}

// codexPaneFactory drives the real tmux client against a codex pane. The first
// capture is the readiness wait's empty composer; every later capture is
// answered by pane. onEnter runs when the Enter key reaches the pane, and the
// factory records whether the composer was ever cleared with C-u.
type codexPaneFactory struct {
	mu       sync.Mutex
	captures int
	cleared  bool
	pane     func(capture int) string
	onEnter  func()
}

func (f *codexPaneFactory) factory(ctx context.Context, _ string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) > 0 && args[0] == "capture-pane" {
		if slices.Contains(args, "-e") {
			return exec.CommandContext(ctx, "true")
		}
		f.captures++
		out := "› \n"
		if f.captures > 1 {
			out = f.pane(f.captures)
		}
		return exec.CommandContext(ctx, "printf", "%s", out)
	}
	if len(args) > 0 && args[0] == "send-keys" {
		switch args[len(args)-1] {
		case "Enter":
			if f.onEnter != nil {
				f.onEnter()
			}
		case "C-u":
			f.cleared = true
		}
	}
	return exec.CommandContext(ctx, "true")
}

func (f *codexPaneFactory) wasCleared() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleared
}

func boundCodexChat() *models.AgentChat {
	rollout := codexReceiptRolloutID
	return &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex", ProviderSessionID: &rollout}
}

func newCodexReceiptServer(t *testing.T, chat *models.AgentChat, rollout *fakeRollout, factory tmux.CommandFactory) (*Server, *models.Session) {
	t.Helper()
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	return &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		tmux:         tmux.NewClient(tmux.WithCommandFactory(factory)),
		agentClients: map[string]agent.AgentRunnerClient{"codex": rollout},
	}, sess
}

func sendCodex(t *testing.T, s *Server) (*connect.Response[pb.SendChatMessageResponse], error) {
	t.Helper()
	return s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "$boss-verify 3068",
		Submit:         true,
	}))
}

// TestSendChatMessage_CodexRolloutTurnConfirmsSend: the composer reads empty
// after the Enter, which on its own proves nothing for Codex, and the rollout
// records the user turn. The send is delivered on the agent's own record.
func TestSendChatMessage_CodexRolloutTurnConfirmsSend(t *testing.T) {
	rollout := &fakeRollout{exists: true}
	panes := &codexPaneFactory{pane: func(int) string { return "› \n" }, onEnter: rollout.recordUserTurn}
	s, sess := newCodexReceiptServer(t, boundCodexChat(), rollout, panes.factory)

	resp, err := sendCodex(t, s)
	if err != nil {
		t.Fatalf("a send the rollout recorded must not error: %v", err)
	}
	if !resp.Msg.GetDelivered() || resp.Msg.GetDeliveryState() != pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED {
		t.Fatalf("delivered = %v, state = %v; want delivered SUBMITTED", resp.Msg.GetDelivered(), resp.Msg.GetDeliveryState())
	}
	rollout.mu.Lock()
	defer rollout.mu.Unlock()
	for _, req := range rollout.reqs {
		if req.GetAgentSessionId() != codexReceiptRolloutID || req.GetWorkDir() != sess.WorktreePath {
			t.Fatalf("rollout read %q in %q, want %q in %q", req.GetAgentSessionId(), req.GetWorkDir(), codexReceiptRolloutID, sess.WorktreePath)
		}
	}
}

// TestSendChatMessage_CodexNoRolloutTurnIsNotDelivered is the BOS-3068
// incident: Codex holds the typed burst undrawn, so the composer reads empty,
// and the rollout records no turn. Pane-only verification called this
// delivered. It must come back unconfirmed, with delivered=false.
func TestSendChatMessage_CodexNoRolloutTurnIsNotDelivered(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the production verifier budgets; skipped in -short")
	}
	rollout := &fakeRollout{exists: true}
	panes := &codexPaneFactory{pane: func(int) string { return "› \n" }}
	s, _ := newCodexReceiptServer(t, boundCodexChat(), rollout, panes.factory)

	resp, err := sendCodex(t, s)
	if err != nil {
		t.Fatalf("expected an unconfirmed response, got error: %v", err)
	}
	if resp.Msg.GetDelivered() || resp.Msg.GetDeliveryState() != pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED {
		t.Fatalf("delivered = %v, state = %v; want undelivered UNCONFIRMED", resp.Msg.GetDelivered(), resp.Msg.GetDeliveryState())
	}
	if panes.wasCleared() {
		t.Fatal("the composer was cleared although it never showed the payload")
	}
}

// TestSendChatMessage_CodexBurstFlushedIntoComposerRetries: the composer reads
// empty right after the Enter, then the held burst flushes and the payload
// sits in the composer. With no rollout turn the early empty reads do not
// settle the send, so the verifier sees the payload held and runs its
// clear-and-redeliver retry.
func TestSendChatMessage_CodexBurstFlushedIntoComposerRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the production verifier budgets; skipped in -short")
	}
	rollout := &fakeRollout{exists: true}
	panes := &codexPaneFactory{pane: func(capture int) string {
		if capture < 5 {
			return "› \n"
		}
		return "› $boss-verify 3068\n"
	}}
	s, _ := newCodexReceiptServer(t, boundCodexChat(), rollout, panes.factory)

	_, err := sendCodex(t, s)
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(fmt.Sprint(err), tmux.OutcomeNotSubmitted.String()) {
		t.Fatalf("err = %v (code %v); want CodeInternal naming %q", err, connect.CodeOf(err), tmux.OutcomeNotSubmitted)
	}
	if !panes.wasCleared() {
		t.Fatal("the held payload was never cleared: the retry did not run")
	}
}

// agentKeyedCodexChat is a long-lived codex chat whose rollout is keyed by its
// agent_session_id, so no provider session id was ever bound: the shape of the
// PR #3105 verify chat, whose transcript `boss chat show` read by
// agent_session_id while every send logged receipt_required=false.
func agentKeyedCodexChat() *models.AgentChat {
	chat := boundCodexChat()
	chat.ProviderSessionID = nil
	return chat
}

// TestSendChatMessage_CodexAgentKeyedRolloutGatesSend: with no provider id
// bound, the rollout under the agent_session_id still confirms the send, and a
// composer that reads empty with no recorded turn is unconfirmed rather than
// submitted on the pane alone.
func TestSendChatMessage_CodexAgentKeyedRolloutGatesSend(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the production verifier budgets; skipped in -short")
	}
	t.Run("turn recorded", func(t *testing.T) {
		rollout := &fakeRollout{exists: true, keyedBy: "agent-1"}
		panes := &codexPaneFactory{pane: func(int) string { return "› \n" }, onEnter: rollout.recordUserTurn}
		s, _ := newCodexReceiptServer(t, agentKeyedCodexChat(), rollout, panes.factory)
		var buf bytes.Buffer
		s.logger = zerolog.New(&buf)

		resp, err := sendCodex(t, s)
		if err != nil || resp.Msg.GetDeliveryState() != pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED {
			t.Fatalf("resp = %v, err = %v; want SUBMITTED", resp, err)
		}
		line := deliveryLogLine(t, buf.String())
		if line["confirmed_by"] != "receipt" || line["receipt_required"] != true {
			t.Fatalf("delivery log = %v; want confirmed_by=receipt receipt_required=true", line)
		}
	})
	t.Run("no turn recorded", func(t *testing.T) {
		rollout := &fakeRollout{exists: true, keyedBy: "agent-1"}
		panes := &codexPaneFactory{pane: func(int) string { return "› \n" }}
		s, _ := newCodexReceiptServer(t, agentKeyedCodexChat(), rollout, panes.factory)

		resp, err := sendCodex(t, s)
		if err != nil {
			t.Fatalf("expected an unconfirmed response, got error: %v", err)
		}
		if resp.Msg.GetDelivered() || resp.Msg.GetDeliveryState() != pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED {
			t.Fatalf("delivered = %v, state = %v; want undelivered UNCONFIRMED", resp.Msg.GetDelivered(), resp.Msg.GetDeliveryState())
		}
	})
}

// TestSendChatMessage_CodexPaneOnlyFallbackIsLogged: a codex submit with no
// readable rollout under either id falls back to the pane and says so at warn.
func TestSendChatMessage_CodexPaneOnlyFallbackIsLogged(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := newSendMessageTestServer(t, agentKeyedCodexChat(), sess, &fakeTmuxClient{available: true, hasSession: true})
	s.agentClients = map[string]agent.AgentRunnerClient{"codex": &fakeRollout{exists: true, keyedBy: codexReceiptRolloutID}}
	var buf bytes.Buffer
	s.logger = zerolog.New(&buf)

	if _, err := sendCodex(t, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(raw), &entry) == nil && entry["message"] == "codex submit has no rollout receipt; verifying from the pane alone" {
			if entry["level"] != "warn" || entry["chat_id"] != "c1" {
				t.Fatalf("fallback log = %v; want a warn naming chat c1", entry)
			}
			return
		}
	}
	t.Fatalf("no pane-only fallback warning in %q", buf.String())
}

// TestSendChatMessage_CodexReceiptNeedsAReadableRollout: the rollout receipt is
// wired only when there is a rollout to read and a turn to expect. Every other
// codex send keeps the pane verifier as its only signal.
func TestSendChatMessage_CodexReceiptNeedsAReadableRollout(t *testing.T) {
	unbound := boundCodexChat()
	unbound.ProviderSessionID = nil
	cases := []struct {
		name    string
		chat    *models.AgentChat
		exists  bool
		wedged  bool
		keyedBy string
		message string
		submit  bool
		want    bool
	}{
		{"bound readable rollout", boundCodexChat(), true, false, "", "$boss-verify 3068", true, true},
		{"no rollout bound and none under the agent_session_id", unbound, true, false, codexReceiptRolloutID, "$boss-verify 3068", true, false},
		{"unbound chat whose rollout is keyed by its agent_session_id", unbound, true, false, "agent-1", "$boss-verify 3068", true, true},
		{"bound id without a rollout falls back to the agent_session_id", boundCodexChat(), true, false, "agent-1", "$boss-verify 3068", true, true},
		{"rollout not readable", boundCodexChat(), false, false, "", "$boss-verify 3068", true, false},
		{"rollout plugin never answers", boundCodexChat(), true, true, "", "$boss-verify 3068", true, false},
		{"codex built-in slash command", boundCodexChat(), true, false, "", "/status", true, false},
		{"prefill", boundCodexChat(), true, false, "", "$boss-verify 3068", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
			tmuxer := &fakeTmuxClient{available: true, hasSession: true}
			s := newSendMessageTestServer(t, tc.chat, sess, tmuxer)
			s.agentClients = map[string]agent.AgentRunnerClient{"codex": &fakeRollout{exists: tc.exists, wedged: tc.wedged, keyedBy: tc.keyedBy}}

			sent := make(chan error, 1)
			go func() {
				_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
					AgentSessionId: "agent-1",
					Message:        tc.message,
					Submit:         tc.submit,
				}))
				sent <- err
			}()
			select {
			case err := <-sent:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case <-time.After(5 * rolloutReceiptReadTimeout):
				t.Fatal("the send never returned: a rollout read that never answers blocked it")
			}
			tmuxer.mu.Lock()
			got, required := tmuxer.lastSubmitted != nil, tmuxer.lastReceiptRequired
			tmuxer.mu.Unlock()
			if got != tc.want || required != tc.want {
				t.Fatalf("receipt check = %v, required = %v; want both %v", got, required, tc.want)
			}
		})
	}
}

// deliveryLogLine returns the "chat message delivery" entry from a JSON log.
func deliveryLogLine(t *testing.T, log string) map[string]any {
	t.Helper()
	for _, raw := range strings.Split(strings.TrimSpace(log), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(raw), &entry) == nil && entry["message"] == "chat message delivery" {
			return entry
		}
	}
	t.Fatalf("no delivery log line in %q", log)
	return nil
}

// TestSendChatMessage_LogsDeliveryOutcomeWithoutText: a send logs one line
// naming the chat, the session, the delivery state and the signal that settled
// it, and never the message text.
func TestSendChatMessage_LogsDeliveryOutcomeWithoutText(t *testing.T) {
	const text = "$boss-verify 3068"
	t.Run("rollout receipt", func(t *testing.T) {
		rollout := &fakeRollout{exists: true}
		panes := &codexPaneFactory{pane: func(int) string { return "› \n" }, onEnter: rollout.recordUserTurn}
		s, _ := newCodexReceiptServer(t, boundCodexChat(), rollout, panes.factory)
		var buf bytes.Buffer
		s.logger = zerolog.New(&buf)

		if _, err := sendCodex(t, s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertDeliveryLog(t, buf.String(), text, "submitted", "receipt")
	})
	t.Run("pane read", func(t *testing.T) {
		chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
		sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
		s := newSendMessageTestServer(t, chat, sess, &fakeTmuxClient{available: true, hasSession: true})
		var buf bytes.Buffer
		s.logger = zerolog.New(&buf)

		if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
			AgentSessionId: "agent-1",
			Message:        text,
			Submit:         true,
		})); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertDeliveryLog(t, buf.String(), text, "submitted", "pane")
	})
}

func assertDeliveryLog(t *testing.T, log, text, state, confirmedBy string) {
	t.Helper()
	if strings.Contains(log, text) {
		t.Fatalf("the delivery log carries the message text %q: %s", text, log)
	}
	line := deliveryLogLine(t, log)
	want := map[string]any{"chat_id": "c1", "session_id": "s1", "delivery_state": state, "confirmed_by": confirmedBy}
	for k, v := range want {
		if line[k] != v {
			t.Fatalf("delivery log %s = %v, want %v (line %v)", k, line[k], v, line)
		}
	}
}
