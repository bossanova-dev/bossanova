package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// newHarness seeds the standard repos and scripts CreateSession to stream one
// setup line followed by the created session, which is the shape the daemon
// produces on the non-interactive `--repo` + `--prompt` path.
func newHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "installing deps\n"},
		}},
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-new-999",
				RepoId:          "repo-1",
				AgentSessionId:  proto.String("chat-new-888"),
				BranchName:      "boss/add-tmux-unattended",
				State:           pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
				RepoDisplayName: "my-app",
			}},
		}},
	}, 0)
	return h
}

// TestCLI_New_JSONEnvelope pins the BOS-821 machine surface: `--json` emits a
// single JSON object carrying the session id and the chat id. It asserts by
// UNMARSHALLING rather than by substring, so a malformed object or a second
// object written into the same stream fails the test instead of passing on a
// lucky grep.
func TestCLI_New_JSONEnvelope(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	var env struct {
		Session struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			ChatID string `json:"chat_id"`
		} `json:"session"`
	}
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (stdout=%q)", err, res.Stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carried more than one JSON value: %q", res.Stdout)
	}
	if env.Session.ID != "sess-new-999" {
		t.Errorf("session.id = %q, want sess-new-999", env.Session.ID)
	}
	if env.Session.ChatID != "chat-new-888" {
		t.Errorf("session.chat_id = %q, want chat-new-888", env.Session.ChatID)
	}
	// Setup script output is a human progress channel; it must not pollute the
	// stream a driver parses.
	if !strings.Contains(res.Stderr, "installing deps") {
		t.Errorf("stderr = %q, want the setup output", res.Stderr)
	}
}

// TestCLI_New_TwoLineOutputUnchanged pins that adding --json left the default
// scripting surface byte-identical. Existing callers parse these two lines
// positionally, so this asserts the whole of stdout, not a substring.
func TestCLI_New_TwoLineOutputUnchanged(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	const want = "session-id: sess-new-999\nchat-id:    chat-new-888\n"
	if res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
}

// TestCLI_New_PrintsIDsBeforeSetupCompletes pins BOS-803: callers get the
// session and chat ids as soon as SessionCreated arrives, even while later setup
// output is still pending. The delayed setup frame proves stdout was surfaced
// before the stream completed.
func TestCLI_New_PrintsIDsBeforeSetupCompletes(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-early-123",
				RepoId:          "repo-1",
				AgentSessionId:  proto.String("chat-early-456"),
				State:           pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
				RepoDisplayName: "my-app",
			}},
		}},
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "still setting up\n"},
		}},
	}, 2*time.Second)

	running := h.Start("new", "--repo", "repo-1", "--prompt", "add a thing", "--detach")
	waited := false
	defer func() {
		if !waited {
			_ = running.Wait()
		}
	}()

	const want = "session-id: sess-early-123\nchat-id:    chat-early-456\n"
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(running.Stdout(), want) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := running.Stdout(); got != want {
		t.Fatalf("stdout before setup completion = %q, want %q", got, want)
	}
	if strings.Contains(running.Stderr(), "still setting up") {
		t.Fatalf("setup output already arrived; test no longer proves early id emission (stderr=%q)", running.Stderr())
	}

	res := running.Wait()
	waited = true
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "still setting up") {
		t.Errorf("final stderr = %q, want setup progress", res.Stderr)
	}
}

func TestCLI_New_SetupOutputFramesAreLineDelimited(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-progress-123",
				RepoId:          "repo-1",
				AgentSessionId:  proto.String("chat-progress-456"),
				State:           pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
				RepoDisplayName: "my-app",
			}},
		}},
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "creating worktree"},
		}},
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "installing deps"},
		}},
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "worktree startup complete"},
		}},
	}, 0)

	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", "--detach")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	const want = "creating worktree\ninstalling deps\nworktree startup complete\n"
	if !strings.HasSuffix(res.Stderr, want) {
		t.Errorf("stderr = %q, want line-delimited setup progress suffix %q", res.Stderr, want)
	}
}

func TestCLI_New_PrintsSessionIDBeforeSettledChatID(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-late-chat-123",
				RepoId:          "repo-1",
				State:           pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
				RepoDisplayName: "my-app",
			}},
		}},
		{Event: &pb.CreateSessionResponse_SetupOutput{
			SetupOutput: &pb.SetupScriptOutput{Text: "still setting up\n"},
		}},
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-late-chat-123",
				RepoId:          "repo-1",
				AgentSessionId:  proto.String("chat-late-456"),
				State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
				RepoDisplayName: "my-app",
			}},
		}},
	}, 2*time.Second)

	running := h.Start("new", "--repo", "repo-1", "--prompt", "add a thing", "--detach")
	waited := false
	defer func() {
		if !waited {
			_ = running.Wait()
		}
	}()

	const early = "session-id: sess-late-chat-123\n"
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && running.Stdout() != early {
		time.Sleep(10 * time.Millisecond)
	}
	if got := running.Stdout(); got != early {
		t.Fatalf("stdout before setup completion = %q, want only %q", got, early)
	}
	if strings.Contains(running.Stdout(), "chat-id:") {
		t.Fatalf("chat-id was printed before it was populated: %q", running.Stdout())
	}

	res := running.Wait()
	waited = true
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	const want = "session-id: sess-late-chat-123\nchat-id:    chat-late-456\n"
	if res.Stdout != want {
		t.Errorf("final stdout = %q, want %q", res.Stdout, want)
	}
}

// TestCLI_New_DetachFlagsRemainAcceptedNoOps pins that --detach and
// --no-attach are still accepted and still produce the same result, which is
// what "the flag is a no-op there" in the help text means. runNew no longer
// reads either flag — the scripting path always detaches — so this is the
// guard that the flags did not quietly become parse errors.
func TestCLI_New_DetachFlagsRemainAcceptedNoOps(t *testing.T) {
	const want = "session-id: sess-new-999\nchat-id:    chat-new-888\n"
	for _, flag := range []string{"--detach", "--no-attach"} {
		t.Run(flag, func(t *testing.T) {
			h := newHarness(t)
			res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", flag)

			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			if res.Stdout != want {
				t.Errorf("stdout = %q, want %q", res.Stdout, want)
			}
			req := h.Daemon.LastCreateSession()
			if req == nil {
				t.Fatal("no CreateSession request recorded")
			}
			if !req.GetDetach() {
				t.Error("detach = false, want true")
			}
			if req.GetIsTmuxUnattended() {
				t.Error("is_tmux_unattended = true; the detach flags must not set it")
			}
		})
	}
}

// TestCLI_New_TmuxUnattendedAndTrackerFieldsReachDaemon pins that the new flags
// travel over the wire, and that an omitted tracker flag arrives as an absent
// optional field rather than a pointer to "". The recorded request is what
// proves it — the CLI's own struct would agree with itself.
func TestCLI_New_TmuxUnattendedAndTrackerFieldsReachDaemon(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing",
		"--tmux-unattended", "--tracker-id", "BOS-821", "--tracker-source", "linear")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	req := h.Daemon.LastCreateSession()
	if req == nil {
		t.Fatal("no CreateSession request recorded")
	}
	if !req.GetIsTmuxUnattended() {
		t.Error("is_tmux_unattended = false, want true")
	}
	if !req.GetDetach() {
		t.Error("detach = false; --tmux-unattended must not clear it")
	}
	if req.TrackerId == nil || req.GetTrackerId() != "BOS-821" {
		t.Errorf("tracker_id = %v, want BOS-821", req.TrackerId)
	}
	if req.TrackerSource == nil || req.GetTrackerSource() != "linear" {
		t.Errorf("tracker_source = %v, want linear", req.TrackerSource)
	}
	// --tracker-url was not passed, so the optional field must be absent.
	if req.TrackerUrl != nil {
		t.Errorf("tracker_url = %q, want nil for an omitted flag", req.GetTrackerUrl())
	}
	if req.GetPlan() != "add a thing" {
		t.Errorf("plan = %q, want the prompt verbatim", req.GetPlan())
	}
}

// TestCLI_New_InvalidTrackerSourceIssuesNoRPC pins that an unknown
// --tracker-source is rejected locally: exit 1, the shared error envelope on
// stdout under --json, and NO CreateSession ever reaching the daemon. The
// nil recorded request is the load-bearing assertion — an exit 1 alone would
// also be true of a daemon-side rejection.
func TestCLI_New_InvalidTrackerSourceIssuesNoRPC(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing",
		"--tracker-source", "jira", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if h.Daemon.LastCreateSession() != nil {
		t.Error("CreateSession was issued for an invalid --tracker-source")
	}

	var env struct {
		Error struct {
			Code        string `json:"code"`
			ConnectCode string `json:"connect_code"`
			Message     string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON error envelope: %v (stdout=%q)", err, res.Stdout)
	}
	if env.Error.Code != "INVALID_ARGUMENT" {
		t.Errorf("error.code = %q, want INVALID_ARGUMENT", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "jira") {
		t.Errorf("error.message = %q, want the rejected value", env.Error.Message)
	}
}

// idleHarness scripts CreateSession to return a session with NO chat id, which
// is what the daemon produces for a quick chat: server.go routes quick_chat to
// StartQuickChatSession, which never enters StartSession and so launches no
// agent at create time. This is the shape that would otherwise be a silent
// no-op.
func idleHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{Session: &pb.Session{
				Id:              "sess-idle-777",
				RepoId:          "repo-1",
				State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
				RepoDisplayName: "my-app",
			}},
		}},
	}, 0)
	return h
}

// TestCLI_New_DeferPRReachesDaemon pins that --defer-pr travels over the wire.
// The recorded request is what proves it — the CLI's own struct would agree
// with itself. Detach is asserted alongside because it is what mints the
// finalize hook token that opens the deferred PR when commits DID land.
func TestCLI_New_DeferPRReachesDaemon(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", "--defer-pr")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	req := h.Daemon.LastCreateSession()
	if req == nil {
		t.Fatal("no CreateSession request recorded")
	}
	if !req.GetDeferPr() {
		t.Error("defer_pr = false, want true")
	}
	if req.GetIsQuickChat() {
		t.Error("is_quick_chat = true; --defer-pr must not set it")
	}
	if !req.GetDetach() {
		t.Error("detach = false; --defer-pr must not clear it")
	}
}

// TestCLI_New_QuickChatReachesDaemon pins the mirrored case for --quick-chat.
func TestCLI_New_QuickChatReachesDaemon(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", "--quick-chat")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	req := h.Daemon.LastCreateSession()
	if req == nil {
		t.Fatal("no CreateSession request recorded")
	}
	if !req.GetIsQuickChat() {
		t.Error("is_quick_chat = false, want true")
	}
	if req.GetDeferPr() {
		t.Error("defer_pr = true; --quick-chat must not set it")
	}
	if !req.GetDetach() {
		t.Error("detach = false; --quick-chat must not clear it")
	}
}

// TestCLI_New_QuickChatWithDeferPRIssuesNoRPC pins that the contradictory pair
// is refused locally: exit 1, the shared error envelope under --json, and NO
// CreateSession reaching the daemon. The nil recorded request is the
// load-bearing assertion — asserting only the error would also pass against an
// implementation that sends the request first and fails afterwards, which is
// precisely the bug this guards (the daemon would honour quick_chat and
// silently drop defer_pr).
func TestCLI_New_QuickChatWithDeferPRIssuesNoRPC(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing",
		"--quick-chat", "--defer-pr", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if h.Daemon.LastCreateSession() != nil {
		t.Error("CreateSession was issued for the mutually exclusive flag pair")
	}

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON error envelope: %v (stdout=%q)", err, res.Stdout)
	}
	if env.Error.Code != "INVALID_ARGUMENT" {
		t.Errorf("error.code = %q, want INVALID_ARGUMENT", env.Error.Code)
	}
	for _, want := range []string{"--quick-chat", "--defer-pr"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("error.message = %q, want it to name %s", env.Error.Message, want)
		}
	}
}

// TestCLI_New_QuickChatWithDeferPRWithoutJSON pins the same rejection on the
// human path: exit 1, prose on stderr, and still no RPC.
func TestCLI_New_QuickChatWithDeferPRWithoutJSON(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing",
		"--quick-chat", "--defer-pr")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if h.Daemon.LastCreateSession() != nil {
		t.Error("CreateSession was issued for the mutually exclusive flag pair")
	}
	if !strings.Contains(res.Stderr, "quick-chat") {
		t.Errorf("stderr = %q, want the rejection to name the flag", res.Stderr)
	}
}

// TestCLI_New_QuickChatIdleSessionEmitsNextAction is the guard against this
// feature creating a NEW silent failure. A create that launched no agent must
// say so — but only on channels that are safe to change. stdout keeps the
// frozen two-line shape byte-for-byte (including the empty chat-id line, which
// existing callers parse positionally); the notice goes to stderr; and the
// machine surface gets next_action.
func TestCLI_New_QuickChatIdleSessionEmitsNextAction(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		h := idleHarness(t)
		res := h.Run("new", "--repo", "repo-1", "--prompt", "plan a thing", "--quick-chat")

		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		// The whole of stdout, not a substring: the chat-id line must still be
		// printed even though it is empty.
		const want = "session-id: sess-idle-777\nchat-id:    \n"
		if res.Stdout != want {
			t.Errorf("stdout = %q, want %q", res.Stdout, want)
		}
		if !strings.Contains(res.Stderr, "idle awaiting attach") {
			t.Errorf("stderr = %q, want the idle/no-agent notice", res.Stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		h := idleHarness(t)
		res := h.Run("new", "--repo", "repo-1", "--prompt", "plan a thing",
			"--quick-chat", "--json")

		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		var env struct {
			Session struct {
				ID         string `json:"id"`
				ChatID     string `json:"chat_id"`
				NextAction string `json:"next_action"`
			} `json:"session"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
			t.Fatalf("stdout is not the JSON envelope: %v (stdout=%q)", err, res.Stdout)
		}
		if env.Session.ChatID != "" {
			t.Errorf("chat_id = %q, want empty for an idle session", env.Session.ChatID)
		}
		if env.Session.NextAction == "" {
			t.Error("next_action is empty; an idle create must say how to start work")
		}
	})
}

// TestCLI_New_OrdinaryCreateOmitsNextAction is the compatibility half of the
// pair above. An ordinary create's envelope must not carry next_action AT ALL —
// asserted on the raw JSON bytes, because unmarshalling into a struct cannot
// distinguish an absent key from a present-but-empty one and would pass against
// exactly the `omitempty` regression this guards.
func TestCLI_New_OrdinaryCreateOmitsNextAction(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "next_action") {
		t.Errorf("ordinary create's envelope carries next_action; omitempty regressed: %q", res.Stdout)
	}
	// Guard the guard: the assertion above is only meaningful if this really is
	// the populated-chat-id path.
	if !strings.Contains(res.Stdout, "chat-new-888") {
		t.Fatalf("stdout = %q, want the populated chat id", res.Stdout)
	}
	if strings.Contains(res.Stderr, "idle awaiting attach") {
		t.Errorf("stderr = %q, want no idle notice for a launched session", res.Stderr)
	}
}

// TestCLI_New_InvalidTrackerSourceWithoutJSON pins the same rejection on the
// human path: exit 1, prose on stderr, and still no RPC.
func TestCLI_New_InvalidTrackerSourceWithoutJSON(t *testing.T) {
	h := newHarness(t)
	res := h.Run("new", "--repo", "repo-1", "--prompt", "add a thing",
		"--tracker-source", "jira")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if h.Daemon.LastCreateSession() != nil {
		t.Error("CreateSession was issued for an invalid --tracker-source")
	}
	if !strings.Contains(res.Stderr, "tracker-source") {
		t.Errorf("stderr = %q, want the rejection to name the flag", res.Stderr)
	}
}
