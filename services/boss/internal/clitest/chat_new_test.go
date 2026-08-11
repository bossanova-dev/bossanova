package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
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
		h.Daemon.SetRecordChatResponse(&pb.ClaudeChat{
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
		h.Daemon.SetRecordChatResponse(&pb.ClaudeChat{
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
