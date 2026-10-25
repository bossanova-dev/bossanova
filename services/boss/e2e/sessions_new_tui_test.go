package e2e_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// navigateToQuickChatForm presses 'n' on home (single repo skips repo select),
// navigates to the "Quick Chat" type-select row (third option after NewPR and
// ExistingPR), and presses Enter to advance into the form phase. Returns once
// the form is visible.
func navigateToQuickChatForm(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Quick Chat"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}
	// Quick Chat is the third row (index 2): NewPR, ExistingPR, IsQuickChat.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Session name"); err != nil {
		t.Fatalf("expected Quick Chat form; screen:\n%s", h.Driver.Screen())
	}
}

// TestJourney_Sessions_AC1_NewSessionView_QuickChat_NameTyped walks the full Quick Chat flow
// with a user-supplied name: repo (auto-picked, single repo), type select
// → Quick Chat, name entry, submit. Checks the stored title and reuse of the existing checkout.
func TestJourney_Sessions_AC1_NewSessionView_QuickChat_NameTyped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	for _, name := range []string{"fixing the auth bug", ""} {
		t.Run(fmt.Sprintf("name=%q", name), func(t *testing.T) {
			h := tuitest.New(t, tuitest.WithRepos(testRepos()...))
			navigateToQuickChatForm(t, h)
			if name != "" {
				if err := h.Driver.SendString(name); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.Driver.SendEnter(); err != nil {
				t.Fatal(err)
			}
			session := waitForCreatedSession(t, h)
			if name != "" && session.Title != name {
				t.Fatalf("stored title = %q, want %q", session.Title, name)
			}
			if name == "" && !regexp.MustCompile(`^Quick Chat \d{4}-\d{2}-\d{2} \d{2}:\d{2}$`).MatchString(session.Title) {
				t.Fatalf("expected timestamped default title, got %q", session.Title)
			}
			if session.WorktreePath != newWizardRepoPath(t, h) {
				t.Fatalf("Quick Chat must use the existing checkout, got %q", session.WorktreePath)
			}
		})
	}
}

// manyPRs builds a set of PR summaries large enough to exercise filtering.
// Titles are chosen to have distinct substrings so we can narrow to exactly
// one match and so "no matches" queries are possible.
func manyPRs() []*pb.PRSummary {
	return []*pb.PRSummary{
		{Number: 101, Title: "Fix login flow", HeadBranch: "boss/fix-login", State: pb.PRState_PR_STATE_OPEN, Author: "dave"},
		{Number: 102, Title: "Add dark mode", HeadBranch: "boss/dark-mode", State: pb.PRState_PR_STATE_OPEN, Author: "dave"},
		{Number: 103, Title: "Refactor auth middleware", HeadBranch: "boss/refactor-auth", State: pb.PRState_PR_STATE_OPEN, Author: "dave"},
		{Number: 104, Title: "Update dependencies", HeadBranch: "boss/deps", State: pb.PRState_PR_STATE_OPEN, Author: "dave"},
		{Number: 105, Title: "Improve test coverage", HeadBranch: "boss/tests", State: pb.PRState_PR_STATE_OPEN, Author: "dave"},
	}
}

// navigateToPRSelect presses 'n' on home, selects the single repo, then picks
// "Work on an existing PR" (the second row). Returns once the PR table is visible.
func navigateToPRSelect(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}
	// Second row is "Work on an existing PR".
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	// The PR list should now show a PR title.
	if err := h.Driver.WaitForText(waitTimeout, "Fix login flow"); err != nil {
		t.Fatalf("expected PR select; screen:\n%s", h.Driver.Screen())
	}
}

func TestJourney_Sessions_AC58_NewSessionView_PRSelect_FilterActivates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	// Before "/" is pressed, the bar advertises the feature.
	if !strings.Contains(h.Driver.Screen(), "[/] search") {
		t.Fatalf("expected '[/] search' hint in action bar; screen:\n%s", h.Driver.Screen())
	}

	// Press "/" — enter filter mode.
	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "type to search"); err != nil {
		t.Fatalf("expected filter action bar; screen:\n%s", h.Driver.Screen())
	}
}

// newWizardSessions reads the isolated daemon's stored sessions.
func newWizardSessions(t *testing.T, h *tuitest.Harness) []*pb.Session {
	t.Helper()
	response, err := h.Stack.Client().ListSessions(context.Background(), realstackrpc.Request(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Sessions
}

func waitForCreatedSession(t *testing.T, h *tuitest.Harness) *pb.Session {
	t.Helper()
	var session *pb.Session
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		sessions := newWizardSessions(t, h)
		if len(sessions) == 1 && sessions[0].WorktreePath != "" {
			session = sessions[0]
		}
		return session != nil
	}); err != nil {
		t.Fatalf("no stored session after submit: %v\n%s\n%s", err, h.Driver.Screen(), h.Stack.LogTail())
	}
	return session
}

func newWizardRepoPath(t *testing.T, h *tuitest.Harness) string {
	t.Helper()
	response, err := h.Stack.Client().ListRepos(context.Background(), realstackrpc.Request(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.Repos) != 1 {
		t.Fatalf("expected one repository, got %d", len(response.Msg.Repos))
	}
	return response.Msg.Repos[0].LocalPath
}
