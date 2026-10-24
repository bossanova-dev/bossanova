package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// chatNewEnvelope mirrors `boss chat new --json`. The test unmarshals rather
// than matching substrings so a malformed object fails here instead of passing
// a `strings.Contains` that a log line would also satisfy.
type chatNewEnvelope struct {
	Chat struct {
		AgentSessionID  string `json:"agent_session_id"`
		SessionID       string `json:"session_id"`
		Title           string `json:"title"`
		TmuxSessionName string `json:"tmux_session_name"`
	} `json:"chat"`
}

type chatNewErrEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

func newChatHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	return clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
}

func TestCLI_ChatNew(t *testing.T) {
	t.Run("mints a uuid and records a fresh chat", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}

		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 {
			t.Fatalf("RecordChat calls = %d, want 1", len(calls))
		}
		req := calls[0]
		if req.GetSessionId() != "sess-aaa-111" {
			t.Fatalf("session_id = %q, want sess-aaa-111", req.GetSessionId())
		}
		if req.GetResume() {
			t.Fatal("resume = true, want false: `chat new` starts a chat, it does not resume one")
		}
		if _, err := uuid.Parse(req.GetAgentSessionId()); err != nil {
			t.Fatalf("agent_session_id %q is not a UUID: %v", req.GetAgentSessionId(), err)
		}
		// Stdout must name the chat the caller now has to address.
		if !strings.Contains(res.Stdout, req.GetAgentSessionId()) {
			t.Fatalf("stdout does not name the new chat id %q:\n%s", req.GetAgentSessionId(), res.Stdout)
		}
		if !strings.Contains(res.Stdout, "sess-aaa-111") {
			t.Fatalf("stdout does not name the session:\n%s", res.Stdout)
		}
	})

	t.Run("the caller cannot supply the chat id", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111", "--agent-session-id", "caller-chosen")
		if res.ExitCode == 0 {
			t.Fatal("exit = 0, want non-zero: registering a caller-supplied id is record_chat's job")
		}
	})

	t.Run("json envelope carries the id the rpc received", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111", "--title", "repair round", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}

		var env chatNewEnvelope
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Stdout, err)
		}
		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 {
			t.Fatalf("RecordChat calls = %d, want 1", len(calls))
		}
		if env.Chat.AgentSessionID != calls[0].GetAgentSessionId() {
			t.Fatalf("envelope agent_session_id = %q, RPC received %q",
				env.Chat.AgentSessionID, calls[0].GetAgentSessionId())
		}
		if env.Chat.SessionID != "sess-aaa-111" {
			t.Fatalf("envelope session_id = %q, want sess-aaa-111", env.Chat.SessionID)
		}
		if env.Chat.Title != "repair round" {
			t.Fatalf("envelope title = %q, want repair round", env.Chat.Title)
		}
		if env.Chat.TmuxSessionName == "" {
			t.Fatal("envelope tmux_session_name is empty on a success envelope")
		}
	})

	t.Run("empty tmux session name is rejected", func(t *testing.T) {
		h := newChatHarness(t)
		// The real daemon persists this row on a host with no tmux: a chat with
		// nothing listening behind it. Handing that id back as a success is the
		// failure this command exists to prevent.
		h.Daemon.SetRecordChatResponse(&pb.AgentChat{
			SessionId:      "sess-aaa-111",
			AgentSessionId: "11111111-2222-3333-4444-555555555555",
			Title:          "no agent",
		})

		res := h.Run("chat", "new", "sess-aaa-111", "--json")
		if res.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1 (stdout: %s)", res.ExitCode, res.Stdout)
		}
		var env chatNewErrEnvelope
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Stdout, err)
		}
		if env.Error.Code != "AGENT_NOT_SPAWNED" {
			t.Fatalf("error.code = %q, want AGENT_NOT_SPAWNED", env.Error.Code)
		}
		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 {
			t.Fatalf("RecordChat calls = %d, want 1", len(calls))
		}
		if !strings.Contains(env.Error.Message, calls[0].GetAgentSessionId()) {
			t.Fatalf("error.message %q does not name the chat id %q",
				env.Error.Message, calls[0].GetAgentSessionId())
		}
	})

	t.Run("empty tmux session name is rejected without --json too", func(t *testing.T) {
		h := newChatHarness(t)
		h.Daemon.SetRecordChatResponse(&pb.AgentChat{
			SessionId:      "sess-aaa-111",
			AgentSessionId: "11111111-2222-3333-4444-555555555555",
		})
		res := h.Run("chat", "new", "sess-aaa-111")
		if res.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1 (stdout: %s)", res.ExitCode, res.Stdout)
		}
		if strings.Contains(res.Stdout, "chat-id") {
			t.Fatalf("stdout reported a chat id for a chat with no live agent:\n%s", res.Stdout)
		}
	})

	t.Run("session id prefix resolves", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-bbb")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 || calls[0].GetSessionId() != "sess-bbb-222" {
			t.Fatalf("RecordChat calls = %+v, want one for sess-bbb-222", calls)
		}
	})

	t.Run("unknown session errors without recording a chat", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-nope", "--json")
		if res.ExitCode == 0 {
			t.Fatal("exit = 0, want non-zero for an unknown session")
		}
		if calls := h.Daemon.RecordChatCalls(); len(calls) != 0 {
			t.Fatalf("RecordChat calls = %d, want 0 for an unknown session", len(calls))
		}
		var env chatNewErrEnvelope
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Stdout, err)
		}
		if env.Error.Code != "NOT_FOUND" {
			t.Fatalf("error.code = %q, want NOT_FOUND", env.Error.Code)
		}
	})

	t.Run("title and agent are forwarded", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111", "--title", "repair round", "--agent", "codex")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 {
			t.Fatalf("RecordChat calls = %d, want 1", len(calls))
		}
		if calls[0].GetTitle() != "repair round" {
			t.Fatalf("title = %q, want repair round", calls[0].GetTitle())
		}
		if calls[0].GetAgentName() != "codex" {
			t.Fatalf("agent_name = %q, want codex", calls[0].GetAgentName())
		}
	})

	t.Run("omitted agent inherits the session's agent", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		calls := h.Daemon.RecordChatCalls()
		if len(calls) != 1 {
			t.Fatalf("RecordChat calls = %d, want 1", len(calls))
		}
		// An empty agent_name is what makes the daemon inherit. A CLI-invented
		// default would pin the chat to whatever the CLI guessed instead.
		if got := calls[0].GetAgentName(); got != "" {
			t.Fatalf("agent_name = %q, want empty so the session's agent is inherited", got)
		}
	})

	t.Run("the emitted chat id composes with chat send", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("chat new exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		var env chatNewEnvelope
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Stdout, err)
		}

		send := h.Run("chat", "send", env.Chat.AgentSessionID, "go again", "--submit")
		if send.ExitCode != 0 {
			t.Fatalf("chat send exit = %d, want 0 (stderr: %s)", send.ExitCode, send.Stderr)
		}
		sends := h.Daemon.SendChatMessageCalls()
		if len(sends) != 1 {
			t.Fatalf("SendChatMessage calls = %d, want 1", len(sends))
		}
		if sends[0].GetAgentSessionId() != env.Chat.AgentSessionID {
			t.Fatalf("send targeted %q, want the new chat %q",
				sends[0].GetAgentSessionId(), env.Chat.AgentSessionID)
		}
		if !sends[0].GetWakeIfAsleep() {
			t.Fatal("wake_if_asleep = false with the flag omitted")
		}
		if !sends[0].GetSubmit() {
			t.Fatal("submit = false, want true")
		}
	})

	t.Run("chat send --wake-if-asleep=false reaches the daemon", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "new", "sess-aaa-111", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("chat new exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		var env chatNewEnvelope
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Stdout, err)
		}

		send := h.Run("chat", "send", env.Chat.AgentSessionID, "quietly", "--wake-if-asleep=false")
		if send.ExitCode != 0 {
			t.Fatalf("chat send exit = %d, want 0 (stderr: %s)", send.ExitCode, send.Stderr)
		}
		sends := h.Daemon.SendChatMessageCalls()
		if len(sends) != 1 {
			t.Fatalf("SendChatMessage calls = %d, want 1", len(sends))
		}
		if sends[0].GetWakeIfAsleep() {
			t.Fatal("wake_if_asleep = true, want false")
		}
	})

	t.Run("appears in chat --help", func(t *testing.T) {
		h := newChatHarness(t)
		res := h.Run("chat", "--help")
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "Start a new live chat") {
			t.Fatalf("chat --help does not list `new`:\n%s", res.Stdout)
		}
	})
}

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
		clitest.WithChats(&pb.AgentChat{
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

// chatJSON mirrors the `boss chats --json` row. Every assertion below
// unmarshals into this and compares fields: substring matching on the rendered
// envelope would pass on a row that merely *contains* "IDLE" somewhere, which
// is precisely the confusion a settled-green driver must not inherit.
type chatJSON struct {
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
	Chats []chatJSON `json:"chats"`
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
func chatsHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
		clitest.WithChats(
			&pb.AgentChat{Id: "chat-1", SessionId: chatsSessionID, AgentSessionId: idleChatID, Title: "Implement the parser", CreatedAt: timestamppb.New(timestampDaysAgo(2))},
			&pb.AgentChat{Id: "chat-2", SessionId: chatsSessionID, AgentSessionId: workingChatID, Title: "Fix the flake", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
			&pb.AgentChat{Id: "chat-3", SessionId: chatsSessionID, AgentSessionId: waitingChatID, Title: "Wait on CI", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
			&pb.AgentChat{Id: "chat-4", SessionId: chatsSessionID, AgentSessionId: unknownChatID, Title: "No heartbeat yet", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
		),
	)
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId:          idleChatID,
		Status:                  pb.ChatStatus_CHAT_STATUS_IDLE,
		LastOutputAt:            timestamppb.New(chatsLastOutput),
		LastSubstantiveOutputAt: timestamppb.New(chatsLastSubstantive),
	})
	// The spinning-but-idle pane: a live spinner keeps last_output_at fresh
	// while nothing substantive has EVER been observed, so the seed flag is
	// still set. This is the shape a settled-green driver must not read as work.
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId:   workingChatID,
		Status:           pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:     timestamppb.New(chatsLastOutput),
		SpinnerPresent:   true,
		LastOutputSeeded: true,
		Phase:            "reviewing",
	})
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: waitingChatID,
		Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
		LastOutputAt:   timestamppb.New(chatsLastOutput),
		WaitingReason:  "awaiting checks_passed_ready on acme/app#42",
	})
	return h
}

// TestCLI_Chats_TableRendersStatusAndLastOutput pins the two new columns and
// the enum rendering: a caller reading the table must see IDLE/WORKING, never a
// numeric enum value.
func TestCLI_Chats_TableRendersStatusAndLastOutput(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"STATUS", "LAST OUTPUT", "IDLE", "WORKING"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, res.Stdout)
		}
	}
	// The numeric enum value must never reach a reader. CHAT_STATUS_IDLE is 2
	// and CHAT_STATUS_WORKING is 1; a table rendering the raw enum would print
	// a bare digit in the STATUS column.
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(line, idleChatID) && strings.Contains(line, " 2 ") {
			t.Errorf("status rendered as a numeric enum value in %q", line)
		}
	}
}

// TestCLI_Chats_UnknownStatusRendersUnspecified covers the left join's missing
// side: a chat ListChats returns but GetChatStatuses has no entry for must read
// UNSPECIFIED, not blank and not anything a caller could mistake for settled.
func TestCLI_Chats_UnknownStatusRendersUnspecified(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var row string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(line, unknownChatID) {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("no table row for the un-statused chat; got:\n%s", res.Stdout)
	}
	if !strings.Contains(row, "UNSPECIFIED") {
		t.Errorf("row = %q, want UNSPECIFIED for a chat with no cached status", row)
	}
	if strings.Contains(row, "IDLE") {
		t.Errorf("row = %q, must not read as a settled chat", row)
	}
}

// TestCLI_Chats_JSONShape asserts the machine contract by unmarshalling: field
// names, the trimmed enum string, and RFC3339 timestamps.
func TestCLI_Chats_JSONShape(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if len(env.Chats) != 4 {
		t.Fatalf("len(chats) = %d, want 4; got %+v", len(env.Chats), env.Chats)
	}

	byID := make(map[string]chatJSON, len(env.Chats))
	for _, c := range env.Chats {
		byID[c.AgentSessionID] = c
	}

	idle, ok := byID[idleChatID]
	if !ok {
		t.Fatalf("no row for %s; got %+v", idleChatID, env.Chats)
	}
	if idle.Status != "IDLE" {
		t.Errorf("status = %q, want the trimmed enum string %q", idle.Status, "IDLE")
	}
	if idle.Title != "Implement the parser" {
		t.Errorf("title = %q, want the chat title verbatim", idle.Title)
	}
	if got, want := idle.LastOutputAt, chatsLastOutput.Format(time.RFC3339); got != want {
		t.Errorf("last_output_at = %q, want RFC3339 %q", got, want)
	}
	if _, err := time.Parse(time.RFC3339, idle.CreatedAt); err != nil {
		t.Errorf("created_at = %q, not RFC3339: %v", idle.CreatedAt, err)
	}
	if idle.WaitingReason != "" {
		t.Errorf("waiting_reason = %q, want empty for a non-WAITING chat", idle.WaitingReason)
	}

	if got := byID[workingChatID].Phase; got != "reviewing" {
		t.Errorf("working phase = %q, want reviewing", got)
	}
	if idle.Phase != "" {
		t.Errorf("idle phase = %q, want empty", idle.Phase)
	}

	unknown, ok := byID[unknownChatID]
	if !ok {
		t.Fatalf("no row for %s; got %+v", unknownChatID, env.Chats)
	}
	if unknown.Status != "UNSPECIFIED" {
		t.Errorf("status = %q, want UNSPECIFIED for a chat with no cached status", unknown.Status)
	}
	if unknown.LastOutputAt != "" {
		t.Errorf("last_output_at = %q, want empty for a chat with no cached status", unknown.LastOutputAt)
	}
}

// TestCLI_Chats_JSONEmitsNoSettledBoolean pins that the CLI reports state and
// the caller owns the threshold: no settled/ready/done verdict is emitted, so a
// driver cannot inherit a staleness policy that is wrong for it.
//
// The invariant is about WHO decided, not about the Go type. spinner_present
// and last_output_seeded are booleans the DAEMON computes and puts on
// ChatStatusEntry — observations, like status and last_output_at, not
// thresholds the CLI applied — so they are named in the allowlist below. What
// stays forbidden is a CLI-derived verdict: any field the CLI would have to
// pick a cutoff to answer.
func TestCLI_Chats_JSONEmitsNoSettledBoolean(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var generic struct {
		Chats []map[string]any `json:"chats"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	want := map[string]bool{
		"agent_session_id": true, "title": true, "created_at": true, "role": true,
		"status": true, "last_output_at": true, "waiting_reason": true, "phase": true,
		"spinner_present": true, "last_substantive_output_at": true,
		"last_output_seeded": true, "reclaimed_at": true, "reclaim_reason": true,
	}
	// Booleans the daemon computes and the CLI copies through verbatim. A
	// boolean NOT listed here is a verdict the CLI invented, which is the thing
	// this test exists to stop.
	daemonBool := map[string]bool{"spinner_present": true, "last_output_seeded": true}
	for _, row := range generic.Chats {
		for k, v := range row {
			if !want[k] {
				t.Errorf("unexpected field %q = %v in a chat row", k, v)
			}
			if _, isBool := v.(bool); isBool && !daemonBool[k] {
				t.Errorf("field %q is a boolean the daemon does not compute; the CLI must not bake a settled threshold into the transport", k)
			}
		}
		for k := range want {
			if _, ok := row[k]; !ok {
				t.Errorf("row %v is missing required field %q", row, k)
			}
		}
	}
}

// TestCLI_Chats_WaitingReasonSurfaced covers a WAITING chat in both modes: the
// reason it exists for has to reach the reader.
func TestCLI_Chats_WaitingReasonSurfaced(t *testing.T) {
	const reason = "awaiting checks_passed_ready on acme/app#42"

	h := chatsHarness(t)
	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d stderr=%q", table.ExitCode, table.Stderr)
	}
	if !strings.Contains(table.Stdout, "WAITING") {
		t.Errorf("table missing WAITING; got:\n%s", table.Stdout)
	}
	if !strings.Contains(table.Stdout, "checks_passed_ready") {
		t.Errorf("table missing the waiting reason; got:\n%s", table.Stdout)
	}

	jsonRes := h.Run("chats", chatsSessionID, "--json")
	if jsonRes.ExitCode != 0 {
		t.Fatalf("json exit=%d stderr=%q", jsonRes.ExitCode, jsonRes.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(jsonRes.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", jsonRes.Stdout, err)
	}
	for _, c := range env.Chats {
		if c.AgentSessionID != waitingChatID {
			continue
		}
		if c.Status != "WAITING" {
			t.Errorf("status = %q, want WAITING", c.Status)
		}
		if c.WaitingReason != reason {
			t.Errorf("waiting_reason = %q, want %q", c.WaitingReason, reason)
		}
		return
	}
	t.Fatalf("no row for the waiting chat; got %+v", env.Chats)
}

// TestCLI_Chats_StatusUnavailableTable covers the human half of the degraded
// path: rows still print, every status cell reads "?", one stderr line explains
// why, and the command exits 0 so an operator's pipeline is not broken by a
// display gap.
func TestCLI_Chats_StatusUnavailableTable(t *testing.T) {
	h := chatsHarness(t)
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("chats", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, idleChatID) {
		t.Errorf("stdout dropped the chat rows; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "?") {
		t.Errorf("stdout has no ? status cell; got:\n%s", res.Stdout)
	}
	// No cell may read as a real status when none was read.
	for _, forbidden := range []string{"IDLE", "WORKING", "WAITING", "UNSPECIFIED"} {
		if strings.Contains(res.Stdout, forbidden) {
			t.Errorf("stdout shows %q though the status read failed; got:\n%s", forbidden, res.Stdout)
		}
	}
	if !strings.Contains(res.Stderr, "chat status unavailable") {
		t.Errorf("stderr = %q, want one line explaining the unavailable status", res.Stderr)
	}
	if got := strings.Count(res.Stderr, "chat status unavailable"); got != 1 {
		t.Errorf("stderr explains the unavailable status %d times, want exactly 1: %q", got, res.Stderr)
	}
}

// TestCLI_Chats_StatusUnavailableJSONFailsLoud is the property this command
// exists for. A naive implementation that still emits rows — with status
// omitted, empty or null — fails here, because a driver that forgets one null
// check reads a missing status as not-WORKING and merges. The assertions are
// therefore both halves: the error envelope must be present AND no `chats`
// array may be emitted at all.
func TestCLI_Chats_StatusUnavailableJSONFailsLoud(t *testing.T) {
	h := chatsHarness(t)
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1; stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	var errEnv chatsErrorEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &errEnv); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if errEnv.Error.Code != "CHAT_STATUS_UNAVAILABLE" {
		t.Errorf("error.code = %q, want CHAT_STATUS_UNAVAILABLE", errEnv.Error.Code)
	}
	if errEnv.Error.ConnectCode == "" {
		t.Errorf("error.connect_code is empty; a caller meeting an unfamiliar code has nothing to fall back on")
	}

	// The envelope must carry no rows — not an empty array, not rows with an
	// absent status. Decoding generically is what makes "no chats key" testable
	// separately from "chats: []".
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal generic %q: %v", res.Stdout, err)
	}
	if _, ok := generic["chats"]; ok {
		t.Errorf("envelope carries a chats array though status was unavailable: %s", res.Stdout)
	}
}

// TestCLI_Chats_OrderingStable pins that successive polls are diffable: a
// driver comparing two reads must not see spurious reordering.
//
// Both runs are compared against a *fixed* expected order, not merely against
// each other: two runs of an implementation that shuffled rows (a map range,
// say) would still agree with one another roughly one time in twenty-four, so
// run-vs-run equality alone is close to a coin flip rather than a gate.
func TestCLI_Chats_OrderingStable(t *testing.T) {
	h := chatsHarness(t)

	// The daemon returns chats in the order they were recorded; the CLI must
	// pass that through untouched.
	want := []string{idleChatID, workingChatID, waitingChatID, unknownChatID}

	ids := func(out string) []string {
		var env chatsEnvelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", out, err)
		}
		got := make([]string, len(env.Chats))
		for i, c := range env.Chats {
			got[i] = c.AgentSessionID
		}
		return got
	}

	for run := 1; run <= 2; run++ {
		res := h.Run("chats", chatsSessionID, "--json")
		if res.ExitCode != 0 {
			t.Fatalf("run %d exit=%d stderr=%q", run, res.ExitCode, res.Stderr)
		}
		if got := ids(res.Stdout); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("run %d order = %v, want %v", run, got, want)
		}
	}
}

// TestCLI_Chats_UnknownEnumValueRendersUnspecified covers the status this build
// has no name for — a newer daemon reporting a ChatStatus added after this
// binary was compiled. protobuf-go's generated String() falls back to the
// decimal value for anything absent from ChatStatus_name, so an unguarded
// TrimPrefix leaks a bare integer into both output modes. AC 2 says the status
// is never numeric; UNSPECIFIED is the honest rendering because it is already
// documented as not-settled.
func TestCLI_Chats_UnknownEnumValueRendersUnspecified(t *testing.T) {
	const futureStatus = 9999

	h := chatsHarness(t)
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: unknownChatID,
		Status:         pb.ChatStatus(futureStatus),
		LastOutputAt:   timestamppb.New(chatsLastOutput),
	})

	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d stderr=%q", table.ExitCode, table.Stderr)
	}
	if strings.Contains(table.Stdout, "9999") {
		t.Errorf("table rendered the raw enum value; got:\n%s", table.Stdout)
	}

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("json exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	for _, c := range env.Chats {
		if c.AgentSessionID != unknownChatID {
			continue
		}
		if c.Status != "UNSPECIFIED" {
			t.Fatalf("status = %q, want UNSPECIFIED for an enum value this build does not know", c.Status)
		}
		return
	}
	t.Fatalf("no row for %s; got %+v", unknownChatID, env.Chats)
}

// TestCLI_Chats_StatusUnavailableIsNotCodeSpecific pins that the fail-loud
// contract keys on "the status read failed", not on Unimplemented specifically.
// A daemon that dies mid-read answers CodeInternal, and a driver reading rows
// with an absent status merges on a WORKING chat just as readily then.
func TestCLI_Chats_StatusUnavailableIsNotCodeSpecific(t *testing.T) {
	h := chatsHarness(t)
	h.Daemon.SetChatStatusesError(connect.CodeInternal, "status store unavailable")

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 1 {
		t.Fatalf("json exit=%d, want 1; stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	var errEnv chatsErrorEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &errEnv); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if errEnv.Error.Code != "CHAT_STATUS_UNAVAILABLE" {
		t.Errorf("error.code = %q, want CHAT_STATUS_UNAVAILABLE", errEnv.Error.Code)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal generic %q: %v", res.Stdout, err)
	}
	if _, ok := generic["chats"]; ok {
		t.Errorf("envelope carries a chats array though status was unavailable: %s", res.Stdout)
	}

	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d, want 0; stderr=%q", table.ExitCode, table.Stderr)
	}
	if !strings.Contains(table.Stderr, "chat status unavailable") {
		t.Errorf("stderr = %q, want the unavailable-status explanation", table.Stderr)
	}
}

// TestCLI_Chats_JSONNoChats pins the zero-chat envelope: a `chats` key holding
// an empty array, not a null and not an absent key. The failure envelope is
// distinguished by the *absence* of `chats`, so the success envelope has to
// carry it unconditionally for that distinction to mean anything.
func TestCLI_Chats_JSONNoChats(t *testing.T) {
	h := chatsHarness(t)

	res := h.Run("chats", "sess-bbb-222", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	raw, ok := generic["chats"]
	if !ok {
		t.Fatalf("success envelope has no chats key: %s", res.Stdout)
	}
	rows, ok := raw.([]any)
	if !ok {
		t.Fatalf("chats = %v (%T), want an array", raw, raw)
	}
	if len(rows) != 0 {
		t.Errorf("chats = %v, want empty for a session with no chats", rows)
	}
}

// TestCLI_Show_ChatTableCarriesStatus pins that `boss show` — which renders the
// same chat table — gained the columns too. The status join is shared, so a
// reader who reaches the table through show must not be handed the older,
// status-free rows that say nothing about whether a chat has settled.
func TestCLI_Show_ChatTableCarriesStatus(t *testing.T) {
	h := chatsHarness(t)

	res := h.Run("show", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"STATUS", "LAST OUTPUT", "IDLE", "WORKING", "UNSPECIFIED"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, res.Stdout)
		}
	}
}

// TestCLI_Show_StatusUnavailableWarns covers show's degraded path: like the
// table half of `boss chats` it still prints rows, marks every status cell "?",
// explains itself once on stderr and exits 0.
func TestCLI_Show_StatusUnavailableWarns(t *testing.T) {
	h := chatsHarness(t)
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("show", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, idleChatID) {
		t.Errorf("stdout dropped the chat rows; got:\n%s", res.Stdout)
	}
	for _, forbidden := range []string{"IDLE", "WORKING", "WAITING", "UNSPECIFIED"} {
		if strings.Contains(res.Stdout, forbidden) {
			t.Errorf("stdout shows %q though the status read failed; got:\n%s", forbidden, res.Stdout)
		}
	}
	if got := strings.Count(res.Stderr, "chat status unavailable"); got != 1 {
		t.Errorf("stderr explains the unavailable status %d times, want exactly 1: %q", got, res.Stderr)
	}
}

// TestCLI_Chats_JSONCarriesLivenessDiscriminators pins the central claim: every
// row carries the three fields the daemon already computes, always emitted, so
// a CLI-transport driver can follow the same gate the MCP tool description
// prescribes instead of reading last_output_at and guessing.
func TestCLI_Chats_JSONCarriesLivenessDiscriminators(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	byID := make(map[string]chatJSON, len(env.Chats))
	for _, c := range env.Chats {
		byID[c.AgentSessionID] = c
	}

	// Every field is always emitted — an absent key is precisely what a driver
	// must not have to guess about. Checked against the raw object rather than
	// the typed struct, which cannot tell absent from zero.
	var raw struct {
		Chats []map[string]json.RawMessage `json:"chats"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		t.Fatalf("unmarshal raw %q: %v", res.Stdout, err)
	}
	if len(raw.Chats) != 4 {
		t.Fatalf("len(chats) = %d, want 4", len(raw.Chats))
	}
	for i, row := range raw.Chats {
		for _, key := range []string{"spinner_present", "last_substantive_output_at", "last_output_seeded"} {
			if _, ok := row[key]; !ok {
				t.Errorf("chats[%d] is missing the always-emitted key %q", i, key)
			}
		}
	}

	// The spinning-but-idle pane: last_output_at is fresh, but the spinner is
	// what advanced it and nothing substantive has been observed.
	working, ok := byID[workingChatID]
	if !ok {
		t.Fatalf("working chat missing from %+v", env.Chats)
	}
	if !working.SpinnerPresent {
		t.Error("spinner_present = false, want true for the spinning chat")
	}
	if !working.LastOutputSeeded {
		t.Error("last_output_seeded = false, want true for a chat with no substantive observation")
	}
	if working.LastSubstantiveOutputAt != "" {
		t.Errorf("last_substantive_output_at = %q, want empty when unset", working.LastSubstantiveOutputAt)
	}
	if working.LastOutputAt == "" {
		t.Error("last_output_at should still be carried — the spinner advanced it")
	}

	// The genuinely-idle pane is the contrast: no spinner, not seeded, and a
	// real substantive timestamp that is OLDER than last_output_at.
	idle, ok := byID[idleChatID]
	if !ok {
		t.Fatalf("idle chat missing from %+v", env.Chats)
	}
	if idle.SpinnerPresent {
		t.Error("spinner_present = true, want false for the idle chat")
	}
	if idle.LastOutputSeeded {
		t.Error("last_output_seeded = true, want false once a substantive observation landed")
	}
	if want := chatsLastSubstantive.Format(time.RFC3339); idle.LastSubstantiveOutputAt != want {
		t.Errorf("last_substantive_output_at = %q, want %q", idle.LastSubstantiveOutputAt, want)
	}

	// A chat with no cached status reports the zero values rather than
	// inventing a liveness claim.
	unknown, ok := byID[unknownChatID]
	if !ok {
		t.Fatalf("unknown chat missing from %+v", env.Chats)
	}
	if unknown.SpinnerPresent || unknown.LastOutputSeeded || unknown.LastSubstantiveOutputAt != "" {
		t.Errorf("a chat with no cached status must claim nothing, got %+v", unknown)
	}
}

// TestCLI_Chats_TableCarriesLiveness proves the human table gained the same
// discrimination, appended as a new column so an existing field-index reader is
// widened rather than shifted.
func TestCLI_Chats_TableCarriesLiveness(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "LIVENESS") {
		t.Errorf("stdout missing the LIVENESS column; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "spinner") {
		t.Errorf("the spinning chat should be named as such; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "seeded") {
		t.Errorf("a still-seeded timestamp should be named as such; got:\n%s", res.Stdout)
	}
}
