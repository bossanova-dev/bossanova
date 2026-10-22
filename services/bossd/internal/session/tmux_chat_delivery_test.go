package session

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"

	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/agent"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
)

// newDeliveryHarness is a StartTmuxChat harness whose runner consumes the first
// input from a file and declares it can confirm delivery. Its fresh chat id is
// pinned to agent-1 and its receipt deadline to deadline.
func newDeliveryHarness(t *testing.T, deadline time.Duration) *startTmuxChatHarness {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t, tmux.WithSessionStartReadyDeadline(deadline))
	h.lc.newTmuxChatAgentSessionID = func() string { return "agent-1" }
	h.agentFake.ConsumesInitialInput = true
	h.agentFake.InitialInputFileMode = 0o600
	h.agentFake.CanConfirmDelivery = true
	// Claude adopts bossd's id, so the wait confirms through the hook only.
	h.agentFake.ResolveInteractiveSessionIDFunc = func(req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: req.GetRequestedSessionId()}, nil
	}
	return h
}

// postReceiptWhenAwaited plays the receipt hook: it posts a receipt for id as
// soon as a launch is waiting on it, and returns a channel closed once posted.
func postReceiptWhenAwaited(t *testing.T, lc *Lifecycle, id string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if lc.RecordPromptSubmitted(id, time.Now()) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return done
}

func submitInput(prompt string) ChatInput {
	return ChatInput{Prompt: prompt, Delivery: DeliverySubmit}
}

func (h *startTmuxChatHarness) startFailures() []markStartFailedCall {
	h.chats.mu.Lock()
	defer h.chats.mu.Unlock()
	return append([]markStartFailedCall(nil), h.chats.markStartFailedCalls...)
}

func TestStartTmuxChat_ClaudeReceiptConfirmsStart(t *testing.T) {
	h := newDeliveryHarness(t, 5*time.Second)
	h.lc.SetHookPort(4242)
	posted := postReceiptWhenAwaited(t, h.lc, "agent-1")

	id, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{})
	<-posted
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if id != "agent-1" {
		t.Fatalf("id = %q, want agent-1", id)
	}
	if got := h.agentFake.LastBuildInteractiveCommand.GetHookPort(); got != 4242 {
		t.Fatalf("hook_port = %d, want 4242", got)
	}
	if h.tmuxFake.hasSubcommand("kill-session") || len(h.startFailures()) != 0 {
		t.Fatal("a confirmed start must keep its pane and stay unstamped")
	}
	if h.agentFake.readTranscriptCount() != 0 {
		t.Fatal("an agent that adopts bossd's id confirms through its hook, never its transcript")
	}
}

func TestStartTmuxChat_MissingReceiptFailsStartAndKillsPane(t *testing.T) {
	h := newDeliveryHarness(t, 400*time.Millisecond)

	_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{})
	var unconfirmed *DeliveryUnconfirmedError
	if !errors.As(err, &unconfirmed) || unconfirmed.Outcome != DeliveryReceiptMissing {
		t.Fatalf("err = %v, want receipt-missing DeliveryUnconfirmedError", err)
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Fatal("an unconfirmed start must kill its pane so the input can never run late")
	}
	for _, part := range []string{"diagnosis:", "input-file=unread", "argv-carries-input=unknown", "composer=empty", "working=no", "account=ambient"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("missing %q in start error", part)
		}
	}
	fails := h.startFailures()
	if len(fails) != 1 || fails[0].agentSessionID != "agent-1" {
		t.Fatalf("start failures = %+v, want one for agent-1", fails)
	}
	if !strings.Contains(fails[0].reason, "receipt-missing") || !strings.Contains(fails[0].reason, "Welcome to Claude") {
		t.Fatalf("reason must name the outcome and carry the pane tail: %q", fails[0].reason)
	}
	if path := h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath(); path != "" {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("input file must be removed once the outcome is known; stat err=%v", statErr)
		}
	}
}

func TestStartTmuxChat_AgentExitFailsAtOnce(t *testing.T) {
	const deadline = 30 * time.Second
	h := newDeliveryHarness(t, deadline)
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session: gone"

	start := time.Now()
	_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{})
	var unconfirmed *DeliveryUnconfirmedError
	if !errors.As(err, &unconfirmed) || unconfirmed.Outcome != DeliveryAgentExited {
		t.Fatalf("err = %v, want agent-exited", err)
	}
	if elapsed := time.Since(start); elapsed > deadline/2 {
		t.Fatalf("agent-exited took %s; it must not wait out the %s deadline", elapsed, deadline)
	}
	if fails := h.startFailures(); len(fails) != 1 || !strings.Contains(fails[0].reason, "agent-exited") {
		t.Fatalf("start failures = %+v, want one naming agent-exited", fails)
	}
}

// A Codex rollout can appear well after the 2s foreground discovery window;
// the wait keeps resolving and confirms from a user message after launch.
func TestStartTmuxChat_CodexLateRolloutConfirms(t *testing.T) {
	h := newDeliveryHarness(t, 8*time.Second)
	var appearsAt time.Time
	h.agentFake.ResolveInteractiveSessionIDFunc = func(req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
		if appearsAt.IsZero() {
			appearsAt = req.GetLaunchedAfter().AsTime().Add(freshProviderSessionIDResolveDeadline + 500*time.Millisecond)
		}
		if time.Now().Before(appearsAt) {
			return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: "not yet"}, nil
		}
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: "rollout-1"}, nil
	}
	h.agentFake.ReadTranscriptFunc = func(req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
		if req.GetAgentSessionId() != "rollout-1" {
			t.Errorf("transcript read for %q, want rollout-1", req.GetAgentSessionId())
		}
		return &bossanovav1.ReadTranscriptResponse{Exists: true, Messages: []*bossanovav1.ChatMessage{
			{Role: "user", Text: "implement the plan", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)},
		}}, nil
	}

	id, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if id != "agent-1" || len(h.startFailures()) != 0 {
		t.Fatalf("late rollout must confirm: id=%q failures=%+v", id, h.startFailures())
	}
	h.chats.mu.Lock()
	defer h.chats.mu.Unlock()
	bound := false
	for _, u := range h.chats.providerSessionIDUpdates {
		if u.agentSessionID == "agent-1" && u.providerSessionID != nil && *u.providerSessionID == "rollout-1" {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("the resolved rollout id must be bound: %+v", h.chats.providerSessionIDUpdates)
	}
}

// A resumed rollout already holds older user turns; they never confirm the new
// launch's input.
func TestStartTmuxChat_CodexResumedRolloutWithOnlyOlderTurnsIsReceiptMissing(t *testing.T) {
	h := newDeliveryHarness(t, 600*time.Millisecond)
	provider := "rollout-old"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{SessionID: "sess-1", AgentSessionID: "agent-old", AgentName: "claude", ProviderSessionID: &provider}},
	}
	older := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	h.agentFake.ReadTranscriptFunc = func(*bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
		return &bossanovav1.ReadTranscriptResponse{Exists: true, Messages: []*bossanovav1.ChatMessage{
			{Role: "user", Text: "the original task", Timestamp: older},
			{Role: "assistant", Text: "done", Timestamp: older},
		}}, nil
	}

	input := submitInput("continue")
	input.ResumeAgentSessionID = "agent-old"
	_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", input, "T", HookOpts{})
	var unconfirmed *DeliveryUnconfirmedError
	if !errors.As(err, &unconfirmed) || unconfirmed.Outcome != DeliveryReceiptMissing {
		t.Fatalf("err = %v, want receipt-missing", err)
	}
	if h.agentFake.readTranscriptCount() == 0 {
		t.Fatal("the resumed rollout must have been read")
	}
}

// A runner without the capability (opencode) keeps today's unverified argv
// start: no wait, no transcript read, no failure.
func TestStartTmuxChat_NoCapabilitySkipsConfirmation(t *testing.T) {
	const deadline = 30 * time.Second
	h := newDeliveryHarness(t, deadline)
	h.agentFake.CanConfirmDelivery = false

	start := time.Now()
	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if elapsed := time.Since(start); elapsed > deadline/2 {
		t.Fatalf("start without the capability waited %s against a %s receipt deadline", elapsed, deadline)
	}
	if h.agentFake.readTranscriptCount() != 0 || len(h.startFailures()) != 0 || h.tmuxFake.hasSubcommand("kill-session") {
		t.Fatal("start without the capability must run exactly as before")
	}
}

// The account-switch notice is informational: a missing receipt warns and the
// pane is kept.
func TestStartTmuxChat_OptionalInputMissingReceiptKeepsPane(t *testing.T) {
	h := newDeliveryHarness(t, 400*time.Millisecond)
	input := submitInput("you are now on account B")
	input.DeliveryOptional = true

	id, err := h.lc.StartTmuxChat(context.Background(), "sess-1", input, "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if id != "agent-1" {
		t.Fatalf("id = %q, want agent-1", id)
	}
	if h.tmuxFake.hasSubcommand("kill-session") || len(h.startFailures()) != 0 {
		t.Fatal("an optional input's missing receipt must keep the pane and leave the chat unstamped")
	}
}

func TestUserMessageSince(t *testing.T) {
	launch := time.Date(2026, 10, 11, 10, 0, 1, 500_400_000, time.UTC)
	msg := func(role, ts string) *bossanovav1.ChatMessage {
		return &bossanovav1.ChatMessage{Role: role, Timestamp: ts}
	}
	cases := map[string]struct {
		msgs []*bossanovav1.ChatMessage
		want bool
	}{
		"same millisecond":   {[]*bossanovav1.ChatMessage{msg("user", "2026-10-11T10:00:01.500Z")}, true},
		"after launch":       {[]*bossanovav1.ChatMessage{msg("user", "2026-10-11T10:00:03Z")}, true},
		"before launch":      {[]*bossanovav1.ChatMessage{msg("user", "2026-10-11T10:00:01.499Z")}, false},
		"assistant after":    {[]*bossanovav1.ChatMessage{msg("assistant", "2026-10-11T10:00:03Z")}, false},
		"missing timestamp":  {[]*bossanovav1.ChatMessage{msg("user", "")}, false},
		"older then current": {[]*bossanovav1.ChatMessage{msg("user", "2026-10-11T09:00:00Z"), msg("user", "2026-10-11T10:00:02Z")}, true},
	}
	for name, tc := range cases {
		if got := userMessageSince(tc.msgs, launch); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestLastLinesStripsBlankLinesAndBoundsBytes(t *testing.T) {
	in := "one\n\n  \ntwo\nthree\n"
	if got := lastLines(in, 2, 100); got != "two\nthree" {
		t.Fatalf("lastLines = %q", got)
	}
	if got := lastLines("ééé", 5, 3); got != "é" {
		t.Fatalf("byte bound must keep whole runes, got %q", got)
	}
	if got := ansiEscapeRe.ReplaceAllString("\x1b[31mred\x1b[0m\x1b]0;title\x07", ""); got != "red" {
		t.Fatalf("ansi strip = %q", got)
	}
}

// The launch hands the runner the per-chat receipt token, derived from the
// socket token and keyed on the id the hook posts to, so the hook it installs
// authenticates against ValidPromptReceiptToken.
func TestStartTmuxChat_PassesDerivedReceiptToken(t *testing.T) {
	h := newDeliveryHarness(t, 5*time.Second)
	h.lc.SetHookPort(4242)
	h.lc.SetPromptReceiptKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	posted := postReceiptWhenAwaited(t, h.lc, "agent-1")

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", submitInput("implement the plan"), "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	<-posted
	req := h.agentFake.LastBuildInteractiveCommand
	tok := req.GetHookReceiptToken()
	if tok == "" {
		t.Fatal("hook_receipt_token is empty")
	}
	if !h.lc.ValidPromptReceiptToken(req.GetSessionId(), tok) {
		t.Fatal("hook_receipt_token does not validate for the id the hook posts to")
	}
}

func TestStartTmuxChat_TypedClaudeReceiptConfirmsStart(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	h.lc.newTmuxChatAgentSessionID = func() string { return "agent-1" }
	const command = "boss-retro"
	const payload = "/" + command
	var posted bool
	h.tmuxFake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		for _, call := range calls {
			if call.subcommand == "send-keys" && call.args[len(call.args)-1] == "Enter" {
				posted = h.lc.RecordPromptSubmitted("agent-1", time.Now())
			}
		}
		// Keep showing the original line after Enter, just like the incident.
		return "Welcome to Claude\n❯ " + payload + "\n"
	}
	id, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Command: command, Delivery: DeliverySubmit}, "T", HookOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "agent-1" || !posted {
		t.Fatalf("id=%q receipt awaited=%v", id, posted)
	}
	if h.tmuxFake.hasSubcommand("kill-session") || len(h.startFailures()) != 0 {
		t.Fatal("confirmed typed start lost its pane or failed")
	}
	count := 0
	h.tmuxFake.mu.Lock()
	for _, call := range h.tmuxFake.calls {
		if call.subcommand == "send-keys" && call.args[len(call.args)-1] == payload {
			count++
		}
	}
	h.tmuxFake.mu.Unlock()
	if count != 1 {
		t.Fatalf("command typed %d times, want 1", count)
	}
	if h.lc.RecordPromptSubmitted("agent-1", time.Now()) {
		t.Fatal("receipt slot leaked after delivery")
	}
}

func TestStartTmuxChat_QueuedTypedClaudeKeepsPane(t *testing.T) {
	if testing.Short() {
		t.Skip("start retry waits through receipt grace")
	}
	h := newStartTmuxChatHarness(t)
	var logs bytes.Buffer
	h.lc.logger = zerolog.New(&logs)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate queue fixture")
	}
	queued, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "testdata", "panes", "claude_queued_midturn_real.txt"))
	if err != nil {
		t.Fatal(err)
	}
	const payload = "/boss-retro"
	queuedPane := strings.ReplaceAll(string(queued), "also update the changelog", payload)
	h.tmuxFake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		held := false
		enters := 0
		for _, call := range calls {
			if call.subcommand != "send-keys" {
				continue
			}
			key := call.args[len(call.args)-1]
			switch key {
			case payload:
				held = true
			case "C-u":
				held = false
			case "Enter":
				enters++
			}
		}
		if enters >= 2 {
			return queuedPane
		}
		if held {
			return "Welcome to Claude\n❯ " + payload + "\n"
		}
		return "Welcome to Claude\n❯\n"
	}
	id, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Command: "boss-retro", Delivery: DeliverySubmit}, "T", HookOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || h.tmuxFake.hasSubcommand("kill-session") || len(h.startFailures()) != 0 {
		t.Fatal("queued input killed a working start")
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), "queued duplicate") || !strings.Contains(logs.String(), `"session":"sess-1"`) {
		t.Fatalf("missing queued warning: %s", logs.String())
	}
}

func TestStartTmuxChat_TypedCodexOpensNoReceiptSlot(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].AgentName = "codex"
	h.lc.SetAgents(map[string]agent.RunnerClient{"codex": h.agentFake})
	h.agentFake.ReadyMarker = "›"
	h.lc.newTmuxChatAgentSessionID = func() string { return "agent-1" }
	var awaited bool
	h.tmuxFake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		awaited = h.lc.RecordPromptSubmitted("agent-1", time.Now()) || awaited
		return drawTypedLine("Codex\n›\n", calls)
	}
	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Command: "boss-retro", Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
		t.Fatal(err)
	}
	if awaited {
		t.Fatal("Codex typed start opened a Claude receipt slot")
	}
}
