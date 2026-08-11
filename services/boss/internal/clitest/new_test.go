package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"

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
