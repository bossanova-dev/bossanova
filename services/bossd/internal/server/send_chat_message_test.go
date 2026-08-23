package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

// errTestDelivery is a canned delivery failure for exercising the error path.
var errTestDelivery = errors.New("delivery failed")

// newSendMessageTestServer wires a Server with the minimum surface SendChatMessage needs.
func newSendMessageTestServer(t *testing.T, chat *models.AgentChat, sess *models.Session, tmuxer *fakeTmuxClient) *Server {
	t.Helper()
	return &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		chatStatus: status.NewTracker(),
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        claudeArgvBuilder(),
		},
	}
}

func TestSendChatMessage_NotFound_UnknownChat(t *testing.T) {
	s := newSendMessageTestServer(t, nil, nil, &fakeTmuxClient{available: true, hasSession: false})
	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "missing",
		Message:        "hello",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

func TestSendChatMessage_LiveChat_Delivers(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}
	wantTmuxName := tmux.ChatSessionName("r1", "agent-1")
	if resp.Msg.TmuxSessionName != wantTmuxName {
		t.Errorf("TmuxSessionName = %q, want %q", resp.Msg.TmuxSessionName, wantTmuxName)
	}

	// Verify SendMessage was called with the right session name and message.
	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()

	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].sessionName != wantTmuxName {
		t.Errorf("SendMessage sessionName = %q, want %q", msgs[0].sessionName, wantTmuxName)
	}
	if msgs[0].text != "do the thing" {
		t.Errorf("SendMessage text = %q, want %q", msgs[0].text, "do the thing")
	}
}

func TestSendChatMessage_LiveChat_HonorsPersistedTmuxName(t *testing.T) {
	// A legacy/relocated chat is live under a persisted name that differs from
	// the deterministic one. SendChatMessage must check liveness and deliver to
	// the persisted name, not the recomputed deterministic name.
	persisted := "boss-chat-legacy-name"
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", TmuxSessionName: &persisted}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if deterministic := tmux.ChatSessionName("r1", "agent-1"); deterministic == persisted {
		t.Fatalf("test precondition broken: persisted name %q equals deterministic name", persisted)
	}

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}
	if resp.Msg.TmuxSessionName != persisted {
		t.Errorf("TmuxSessionName = %q, want persisted %q", resp.Msg.TmuxSessionName, persisted)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].sessionName != persisted {
		t.Errorf("SendMessage sessionName = %q, want persisted %q", msgs[0].sessionName, persisted)
	}
}

func TestSendChatMessage_AsleepChat_WakeIfAsleep_WakesThenDelivers(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	// starts asleep
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "wake up and work",
		WakeIfAsleep:   true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}

	// Wake should have spawned a new session.
	tmuxer.mu.Lock()
	spawnCount := tmuxer.createdN
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()

	if spawnCount != 1 {
		t.Errorf("expected 1 spawn (wake), got %d", spawnCount)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call after wake, got %d", len(msgs))
	}
	if msgs[0].text != "wake up and work" {
		t.Errorf("SendMessage text = %q, want %q", msgs[0].text, "wake up and work")
	}
}

func TestSendChatMessage_AsleepChat_NoWake_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
		WakeIfAsleep:   false,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
}

// TestSendChatMessage_HeadlessRunActive_FailedPrecondition guards against
// waking a tmux agent on a worktree whose headless run (codex exec / claude
// --print) is still in progress: the chat reports WORKING in the status
// tracker but has no tmux session, so wake_if_asleep must be refused (not
// spawn a second, competing agent on the same worktree).
func TestSendChatMessage_HeadlessRunActive_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "race me",
		WakeIfAsleep:   true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}

	// No tmux agent must have been spawned for the still-running headless run.
	tmuxer.mu.Lock()
	spawnCount := tmuxer.createdN
	tmuxer.mu.Unlock()
	if spawnCount != 0 {
		t.Errorf("expected 0 spawns while headless run active, got %d", spawnCount)
	}
}

// TestSendChatMessage_Submit_ThreadsSubmitAndClaudeMarker verifies the handler
// forwards submit=true and resolves the claude ready marker for a chat with no
// (legacy "") agent name.
func TestSendChatMessage_Submit_ThreadsSubmitAndClaudeMarker(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if !msgs[0].submit {
		t.Errorf("submit = false, want true")
	}
	if msgs[0].readyMarker != "❯" {
		t.Errorf("readyMarker = %q, want claude marker %q", msgs[0].readyMarker, "❯")
	}
}

// TestSendChatMessage_DefaultsToPrefill verifies that an omitted submit field
// defaults to false (prefill) — the behavior change the caller audit guards.
func TestSendChatMessage_DefaultsToPrefill(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].submit {
		t.Errorf("submit = true, want false (default prefill)")
	}
}

// TestSendChatMessage_CodexChat_UsesCodexMarker verifies the ready marker is
// resolved from the chat's agent so the submit path waits for codex's composer
// glyph, not claude's.
func TestSendChatMessage_CodexChat_UsesCodexMarker(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/status",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].readyMarker != "›" {
		t.Errorf("readyMarker = %q, want codex marker %q", msgs[0].readyMarker, "›")
	}
}

// TestSendChatMessage_CodexChat_RewritesCommandPrefix is the regression guard for
// the codex slash-command rejection: a caller dispatches an agent-neutral
// "/boss-repair watch", and a codex chat must receive it with codex's "$" prefix
// ("$boss-repair watch"), since codex reserves "/" for its own built-ins and
// rejects "/boss-repair" as unrecognized. Mirrors the plan-launch render path.
func TestSendChatMessage_CodexChat_RewritesCommandPrefix(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	writeSendChatTestSkill(t, filepath.Join(sess.WorktreePath, ".codex", "skills", "bossanova", "boss-repair"))
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].text != "$boss-repair watch" {
		t.Errorf("SendMessage text = %q, want codex-prefixed %q", msgs[0].text, "$boss-repair watch")
	}
}

func TestSendChatMessage_CodexChat_RewritesProjectCustomCommandPrefix(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	writeSendChatTestSkill(t, filepath.Join(sess.WorktreePath, ".codex", "skills", "api-review"))
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/api-review services/bossd/internal/session/tmux_chat.go",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].text != "$api-review services/bossd/internal/session/tmux_chat.go" {
		t.Errorf("SendMessage text = %q, want codex-prefixed custom skill", msgs[0].text)
	}
}

// TestSendChatMessage_CodexChat_PreservesNativeSlashCommand guards the scope of
// the prefix rewrite: a codex user's native built-in ("/status", "/model") must
// reach codex verbatim, not be rewritten to an invalid "$status". Only installed
// custom skills are re-prefixed.
func TestSendChatMessage_CodexChat_PreservesNativeSlashCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/status",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].text != "/status" {
		t.Errorf("SendMessage text = %q, want native command %q unchanged", msgs[0].text, "/status")
	}
}

func writeSendChatTestSkill(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: test\n---\n"), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
}

// TestSendChatMessage_ClaudeChat_PreservesSlashCommand verifies the mirror case:
// a claude chat keeps the "/" prefix, and free text is never mistaken for a
// command.
func TestSendChatMessage_ClaudeChat_PreservesSlashCommand(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].text != "/boss-repair watch" {
		t.Errorf("SendMessage text = %q, want claude-prefixed %q", msgs[0].text, "/boss-repair watch")
	}
}

// TestSendChatMessage_DeliveryError_SurfacesInternal verifies a delivery/verify
// failure surfaces as a CodeInternal error rather than a silent "delivered".
func TestSendChatMessage_DeliveryError_SurfacesInternal(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true, sendMessageErr: errTestDelivery}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", connect.CodeOf(err))
	}
}

func TestSendChatMessage_WorktreeMissing_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: "/nonexistent/path"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
		WakeIfAsleep:   true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
}

// verifierPaneFactory builds a tmux CommandFactory that drives the real submit
// verifier through a scripted sequence of capture-pane results. The first
// capture satisfies sendPlan's ready-marker wait; every capture after it is a
// verification poll and is answered by verify.
//
// These tests deliberately drive the REAL tmux.Client rather than the fake
// spawner: the outcome classification under test lives inside the verifier, so a
// fake that simply returns a canned error would prove nothing about how a real
// capture failure is classified.
func verifierPaneFactory(verify func() *exec.Cmd) tmux.CommandFactory {
	var mu sync.Mutex
	captures := 0
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		if len(args) > 0 && args[0] == "capture-pane" {
			captures++
			if captures == 1 {
				return exec.CommandContext(ctx, "printf", "%s", "❯ \n")
			}
			return verify()
		}
		return exec.CommandContext(ctx, "true")
	}
}

// newVerifierTestServer wires a Server whose tmux surface is a real client
// driven by factory, so SendChatMessage exercises the production verifier.
func newVerifierTestServer(t *testing.T, factory tmux.CommandFactory) *Server {
	t.Helper()
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	return &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmux.NewClient(tmux.WithCommandFactory(factory)),
	}
}

// TestSendChatMessage_CaptureFailure_ReturnsUnconfirmedResponse is BOS-598
// acceptance criterion 2. A capture-pane failure during verification means the
// payload MAY have been submitted. Reporting that as a CodeInternal error reads
// to a caller as "it failed" and invites a retry that double-types into the
// agent's composer, so it must come back as a RESPONSE carrying the state —
// with delivered=false, so every caller already keying on delivered (notably
// bossd's own deliverChatMessage) still fails closed.
func TestSendChatMessage_CaptureFailure_ReturnsUnconfirmedResponse(t *testing.T) {
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		// Verification cannot read the pane at all.
		return exec.CommandContext(context.Background(), "false")
	}))

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("expected a response, got error: %v (code %v)", err, connect.CodeOf(err))
	}
	if resp.Msg.GetDelivered() {
		t.Error("delivered = true, want false for an unconfirmed submit")
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_UNCONFIRMED", got)
	}
	if resp.Msg.GetNoticeText() == "" {
		t.Error("notice_text is empty; an unconfirmed response must carry human-readable detail")
	}
	// notice_text must also carry the one action this state calls for. It is the
	// only field the hand-rolled converters (services/boss/internal/client/remote.go,
	// services/mcp-gateway/internal/proxybackend/proxybackend.go) forward, so a
	// caller reached over the proxy learns "unconfirmed" from here or nowhere.
	if !strings.Contains(resp.Msg.GetNoticeText(), "check the pane before resending") {
		t.Errorf("notice_text = %q, want it to carry the resend guidance", resp.Msg.GetNoticeText())
	}
	// It must carry it ONCE, and must not re-state the condition the verifier's
	// own error already opens with: notice_text is rendered verbatim by the
	// surfaces that never see delivery_state, so every duplicated clause here is
	// a duplicated clause in front of an operator.
	if n := strings.Count(resp.Msg.GetNoticeText(), "may already have been submitted"); n != 1 {
		t.Errorf("notice_text = %q, states the resend guidance %d times, want exactly once", resp.Msg.GetNoticeText(), n)
	}
	if n := strings.Count(resp.Msg.GetNoticeText(), "could not be"); n > 1 {
		t.Errorf("notice_text = %q, states the unverifiable condition %d times, want at most once", resp.Msg.GetNoticeText(), n)
	}
	// The pane is what the guidance sends the operator to look at, so the notice
	// must name it even for a caller that only ever sees notice_text.
	if !strings.Contains(resp.Msg.GetNoticeText(), resp.Msg.GetTmuxSessionName()) {
		t.Errorf("notice_text = %q, want it to name tmux session %q", resp.Msg.GetNoticeText(), resp.Msg.GetTmuxSessionName())
	}
}

// TestSendChatMessage_ConfirmedPending_ErrorsNamingState is BOS-598 acceptance
// criterion 3. A payload confirmed to be still sitting in the composer is a
// genuine failure and must stay loud — and the error has to name the state, so
// the distinction survives surfaces that carry no delivery_state (the proxied
// bossanova.v1 path).
func TestSendChatMessage_ConfirmedPending_ErrorsNamingState(t *testing.T) {
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		// The payload never leaves the prompt.
		return exec.CommandContext(context.Background(), "printf", "%s", "❯ do the thing\n")
	}))

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	}))
	if err == nil {
		t.Fatal("expected an error for a confirmed-pending payload, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), tmux.OutcomeNotSubmitted.String()) {
		t.Errorf("error %q does not name the delivery state %q", err.Error(), tmux.OutcomeNotSubmitted)
	}
}

// TestSendChatMessage_Submitted_ReportsSubmittedState pins the positive half of
// the enum: a verified submit reports DELIVERY_STATE_SUBMITTED, so a caller can
// read one field for all three outcomes instead of inferring from delivered.
func TestSendChatMessage_Submitted_ReportsSubmittedState(t *testing.T) {
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		// Composer cleared: the payload was accepted.
		return exec.CommandContext(context.Background(), "printf", "%s", "❯ \n")
	}))

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.GetDelivered() {
		t.Error("delivered = false, want true")
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_SUBMITTED", got)
	}
}

// TestSendChatMessage_Prefill_LeavesDeliveryStateUnspecified guards the honesty
// of the enum on the prefill path: submit=false runs no Enter and no
// verification, so claiming SUBMITTED would be a lie.
func TestSendChatMessage_Prefill_LeavesDeliveryStateUnspecified(t *testing.T) {
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		return exec.CommandContext(context.Background(), "printf", "%s", "❯ \n")
	}))

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_UNSPECIFIED for a prefill", got)
	}
}

// TestSendChatMessage_WhitespaceSubmit_LeavesDeliveryStateUnspecified covers the
// prefill the request does not ask for. tmux.Client.SendMessage routes on
// `submit && the trimmed text is non-empty`, so a whitespace-only body with
// submit=true has nothing to submit and takes the PREFILL path: no Enter, no
// verification. Deriving the state from submit alone reported SUBMITTED for it —
// the exact unearned claim the field exists to remove — so the state must be
// derived from the same predicate tmux routes on.
func TestSendChatMessage_WhitespaceSubmit_LeavesDeliveryStateUnspecified(t *testing.T) {
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		return exec.CommandContext(context.Background(), "printf", "%s", "❯ \n")
	}))

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "   \n  ",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The delivery itself succeeded — it was simply a prefill, so delivered stays
	// true and only the claim about submission is withheld.
	if !resp.Msg.GetDelivered() {
		t.Error("delivered = false, want true (the prefill itself succeeded)")
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED {
		t.Errorf("delivery_state = %v, want DELIVERY_STATE_UNSPECIFIED (nothing was submitted or verified)", got)
	}
}
