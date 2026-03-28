package tuitest_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const waitTimeout = 10 * time.Second

func TestMain(m *testing.M) {
	cleanup := tuitest.BuildBoss()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func testRepos() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main"},
	}
}

func testSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id:              "sess-aaa-111",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Add dark mode",
			BranchName:      "boss/add-dark-mode",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		},
		{
			Id:              "sess-bbb-222",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Fix login bug",
			BranchName:      "boss/fix-login-bug",
			State:           pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
		},
	}
}

func TestTUI_HomeView_ShowsSessions(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew session"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_HomeView_EmptyState(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "No active sessions"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_Navigation_JK(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press 'j' to move cursor down, then 'k' to move back up.
	// We're just verifying no crash and both sessions remain visible.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected 'Add dark mode' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Fix login bug") {
		t.Fatalf("expected 'Fix login bug' on screen:\n%s", screen)
	}
}

func TestTUI_ArchiveSession(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press 'a' to initiate archive.
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}

	// Wait for confirmation prompt.
	if err := h.Driver.WaitForText(waitTimeout, "Archive"); err != nil {
		t.Fatal(err)
	}

	// Confirm with 'y'.
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// Session should disappear from the list after the next poll cycle.
	err := h.Driver.WaitForNoText(waitTimeout, "Add dark mode")
	if err != nil {
		t.Fatal(err)
	}

	// Verify the daemon state.
	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt == nil {
				t.Fatal("expected session to be archived in daemon")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
}

func TestTUI_ArchiveCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press 'a' to initiate archive, then 'n' to cancel.
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archive"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Session should still be visible.
	time.Sleep(500 * time.Millisecond)
	if !h.Driver.ScreenContains("Add dark mode") {
		t.Fatalf("session disappeared after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_TrashView(t *testing.T) {
	sessions := testSessions()
	// Archive one session so trash isn't empty.
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	// Wait for home (only non-archived sessions show).
	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}

	// The archived session should appear in trash.
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_TrashRestore(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}

	// Press 'r' to restore.
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}

	// Wait for session to disappear from trash (restored).
	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Trash is empty") ||
			!strings.Contains(screen, "Add dark mode")
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify daemon state.
	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt != nil {
				t.Fatal("expected session to be restored in daemon")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 not found in daemon state")
	}
}

func TestTUI_TrashDelete(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Press 'd' to delete, then 'y' to confirm.
	if err := h.Driver.SendKey('d'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Permanently delete"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// Wait for session to disappear.
	err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Trash is empty") ||
			!strings.Contains(screen, "Add dark mode")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTUI_ViewNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	// Wait for home.
	if err := h.Driver.WaitForText(waitTimeout, "Bossanova"); err != nil {
		t.Fatal(err)
	}

	// Navigate to settings.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// Go back with esc.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatal(err)
	}

	// Go back.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_QuitWithQ(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('q'); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.Driver.Done():
		// Process exited cleanly.
	case <-time.After(5 * time.Second):
		t.Fatal("boss process did not exit after 'q'")
	}
}
