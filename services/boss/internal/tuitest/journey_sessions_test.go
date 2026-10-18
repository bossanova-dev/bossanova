package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// One operator path across the wizard, the launch screen, the chat list and the
// home board, checking at each step what the daemon was asked to do.
func TestJourney_Sessions_AC1_AC2_AC14_AC25_CreateOpenListDismiss(t *testing.T) {
	const (
		sessID   = "sess-journey-1"
		title    = "journey chat"
		alertKey = "alert-journey-1"
	)
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...))
	h.Daemon.SetCreateSessionScript([]*pb.CreateSessionResponse{
		{Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{Session: &pb.Session{
			Id:              sessID,
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			BranchName:      "boss/journey-chat",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		}}}},
	}, 0)
	h.Daemon.SetCreateSessionPersists(true)
	// Hold the chat launch on the launch screen so esc lands before tmux would
	// take the terminal.
	h.Daemon.SetRPCStallFor("RecordChat", time.Minute)

	if !t.Run("AC1 n, Quick Chat, a name and enter create the session and open it", func(t *testing.T) {
		waitText(t, h, "no active sessions")
		sendKeys(t, h, 'n')
		waitText(t, h, "Quick Chat")
		sendKeys(t, h, 'j', 'j')
		sendEnter(t, h)
		waitText(t, h, "Session name")
		if err := h.Driver.SendString(title); err != nil {
			t.Fatal(err)
		}
		sendEnter(t, h)
		waitText(t, h, "Launching")
		waitText(t, h, title)

		req := h.Daemon.LastCreateSession()
		if req == nil || req.GetRepoId() != "repo-1" || req.GetTitle() != title || !req.GetIsQuickChat() {
			t.Fatalf("CreateSession request = %v", req)
		}

		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "[esc] back")
		h.Daemon.SetRPCStallFor("RecordChat", 0)
		calls := waitCalls(t, h, h.Daemon.RecordChatCalls, 1)
		if c := calls[0]; c.GetSessionId() != sessID || c.GetResume() || c.GetRole() != pb.ChatRole_CHAT_ROLE_OPERATOR {
			t.Fatalf("RecordChat request = %v, want a fresh operator chat in %s", c, sessID)
		}
	}) {
		return
	}

	if !t.Run("AC2 home half: the new session is listed on the home board", func(t *testing.T) {
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "[n]ew session")
		waitText(t, h, title)
		if h.Driver.ScreenContains("no active sessions") {
			t.Fatalf("home still shows the empty state:\n%s", h.Driver.Screen())
		}
	}) {
		return
	}

	if !t.Run("AC14 in part: a fix alert from the daemon turns the row into a fix row without its sentence", func(t *testing.T) {
		h.Daemon.SetSessionAttention(sessID, &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			Sentence: "Run blocked: the tests failed twice",
			AlertKey: alertKey,
			Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS},
		})
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return rowReadsFix(screen, title) }); err != nil {
			t.Fatalf("row never read fix: %v\n%s", err, h.Driver.Screen())
		}
		screen := h.Driver.Screen()
		if strings.Contains(screen, "Run blocked") {
			t.Fatalf("home list shows the attention sentence:\n%s", screen)
		}
		if strings.Contains(screen, "[x] dismiss") {
			t.Fatalf("home advertises the unadvertised dismiss:\n%s", screen)
		}
	}) {
		return
	}

	if !t.Run("AC25 x confirms once, then dismisses the captured alert key", func(t *testing.T) {
		sendKeys(t, h, 'x')
		waitText(t, h, "Dismiss this alert?")
		if calls := h.Daemon.DismissSessionAlertCalls(); len(calls) != 0 {
			t.Fatalf("DismissSessionAlert before confirmation: %v", calls)
		}
		sendEnter(t, h)
		waitText(t, h, "Alert dismissed")
		calls := h.Daemon.DismissSessionAlertCalls()
		if len(calls) != 1 || calls[0].GetSessionId() != sessID || calls[0].GetAlertKey() != alertKey {
			t.Fatalf("DismissSessionAlert calls = %v, want one for %s/%s", calls, sessID, alertKey)
		}

		h.Daemon.SetSessionAttention(sessID, nil)
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return screenRow(screen, title) != "" && !rowReadsFix(screen, title)
		}); err != nil {
			t.Fatalf("row kept the fix status after the daemon cleared the alert: %v\n%s", err, h.Driver.Screen())
		}
		if calls := h.Daemon.DismissSessionAlertCalls(); len(calls) != 1 {
			t.Fatalf("DismissSessionAlert calls = %d after settling, want 1", len(calls))
		}
	}) {
		return
	}
}

// rowReadsFix reports whether the home row naming title ends in the fix status.
func rowReadsFix(screen, title string) bool {
	return strings.HasSuffix(strings.TrimSpace(screenRow(screen, title)), " fix")
}

func waitText(t *testing.T, h *tuitest.Harness, text string) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, text); err != nil {
		t.Fatalf("waiting for %q: %v\n%s", text, err, h.Driver.Screen())
	}
}

func sendKeys(t *testing.T, h *tuitest.Harness, keys ...byte) {
	t.Helper()
	for _, k := range keys {
		if err := h.Driver.SendKey(k); err != nil {
			t.Fatal(err)
		}
	}
}

func sendEnter(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
}

// waitCalls polls a mock daemon recorder until it holds want calls.
func waitCalls[T any](t *testing.T, h *tuitest.Harness, calls func() []T, want int) []T {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		got := calls()
		if len(got) >= want || time.Now().After(deadline) {
			if len(got) != want {
				t.Fatalf("daemon recorded %d calls, want %d\n%s", len(got), want, h.Driver.Screen())
			}
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}
