package tuitest_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestTUI_NewSessionView_RepoSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	// Press 'n' for new session.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// With 2 repos, it should show "Select a repository".
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatalf("expected repo select; screen:\n%s", h.Driver.Screen())
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected 'my-app' in repo select; screen:\n%s", screen)
	}
	if !strings.Contains(screen, "my-api") {
		t.Fatalf("expected 'my-api' in repo select; screen:\n%s", screen)
	}
}

func TestTUI_NewSessionView_TypeSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatal(err)
	}

	// Select first repo.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should show session type options.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Create a new PR") ||
			strings.Contains(screen, "Quick Chat")
	}); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_NewSessionView_SingleRepoSkipsSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // Only 1 repo.
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// With only 1 repo, should skip repo select and go directly to type select.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Create a new PR") ||
			strings.Contains(screen, "Quick Chat") ||
			strings.Contains(screen, "Starting a new session")
	}); err != nil {
		t.Fatalf("expected type select (skipped repo select); screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_NewSessionView_FormPhase_EscGoesBackToTypeSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // Single repo — skips repo select.
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	// Press 'n' for new session.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Single repo skips repo select — should see type select.
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}

	// Select "Create a new PR" (first option, already highlighted).
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Should be on the form phase with "Session name".
	if err := h.Driver.WaitForText(waitTimeout, "Session name"); err != nil {
		t.Fatalf("expected form phase; screen:\n%s", h.Driver.Screen())
	}

	// Press esc — should go back to type select, not home.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Should see type select options again.
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select after esc from form; screen:\n%s", h.Driver.Screen())
	}

	// Should NOT be on home screen.
	screen := h.Driver.Screen()
	if strings.Contains(screen, "no active sessions") {
		t.Fatalf("should not have returned to home; screen:\n%s", screen)
	}
}

// TestTUI_NewSessionView_SubmitCreatesSession walks the full wizard for the
// NewPR flow — repo (auto-picked, single repo), type select, title entry,
// submit — and asserts the daemon received a CreateSessionRequest with the
// right repo, title, and base branch.
//
// MockDaemon.CreateSession records the request and then returns Unimplemented,
// which the TUI surfaces as an error banner on the form. We assert on the
// captured request rather than a view transition (see FilterSelectsCorrectPR
// for the same pattern).
func TestTUI_NewSessionView_SubmitCreatesSession(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // single repo skips repo select
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	// Open the new session wizard.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}

	// "Create a new PR" is the first row — already highlighted. Pick it.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Session name"); err != nil {
		t.Fatalf("expected form phase; screen:\n%s", h.Driver.Screen())
	}

	// Type a title and submit.
	if err := h.Driver.SendString("fix the navbar"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// CreateSession records the request and returns Unimplemented; poll for
	// the captured request.
	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.RepoId != "repo-1" {
		t.Fatalf("CreateSession.RepoId = %q, want %q", req.RepoId, "repo-1")
	}
	if req.Title != "fix the navbar" {
		t.Fatalf("CreateSession.Title = %q, want %q", req.Title, "fix the navbar")
	}
	if req.BaseBranch != "main" {
		t.Fatalf("CreateSession.BaseBranch = %q, want %q", req.BaseBranch, "main")
	}
	if req.QuickChat {
		t.Fatalf("CreateSession.QuickChat = true, want false for NewPR flow")
	}
	if req.PrNumber != nil {
		t.Fatalf("CreateSession.PrNumber = %v, want nil for NewPR flow", req.PrNumber)
	}
}

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
	// Quick Chat is the third row (index 2): NewPR, ExistingPR, QuickChat.
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

// TestTUI_NewSessionView_QuickChat_NameTyped walks the full Quick Chat flow
// with a user-supplied name: repo (auto-picked, single repo), type select
// → Quick Chat, name entry, submit. Asserts the captured CreateSessionRequest
// has the typed Title and QuickChat == true.
func TestTUI_NewSessionView_QuickChat_NameTyped(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // single repo skips repo select
	)
	navigateToQuickChatForm(t, h)

	if err := h.Driver.SendString("fixing the auth bug"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.Title != "fixing the auth bug" {
		t.Fatalf("CreateSession.Title = %q, want %q", req.Title, "fixing the auth bug")
	}
	if !req.QuickChat {
		t.Fatalf("CreateSession.QuickChat = false, want true for Quick Chat flow")
	}
}

// TestTUI_NewSessionView_QuickChat_EmptyName confirms that submitting the
// Quick Chat form with no input falls back to a timestamped default title.
func TestTUI_NewSessionView_QuickChat_EmptyName(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // single repo skips repo select
	)
	navigateToQuickChatForm(t, h)

	// Press Enter without typing anything — empty submission is legal for Quick Chat.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if !req.QuickChat {
		t.Fatalf("CreateSession.QuickChat = false, want true for Quick Chat flow")
	}
	matched, err := regexp.MatchString(`^Quick Chat \d{4}-\d{2}-\d{2} \d{2}:\d{2}$`, req.Title)
	if err != nil {
		t.Fatalf("regexp.MatchString error: %v", err)
	}
	if !matched {
		t.Fatalf("CreateSession.Title = %q, want it to match `^Quick Chat \\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}$`", req.Title)
	}
}

// TestTUI_NewSessionView_QuickChat_EscReturnsToTypeSelect verifies that Esc
// from the Quick Chat name-entry form returns the user to the type-select
// table rather than firing a session create or popping back home.
func TestTUI_NewSessionView_QuickChat_EscReturnsToTypeSelect(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // single repo skips repo select
	)
	navigateToQuickChatForm(t, h)

	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Should see the type-select rows again.
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select after esc from Quick Chat form; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Quick Chat") {
		t.Fatalf("expected 'Quick Chat' row in type select; screen:\n%s", screen)
	}
	if strings.Contains(screen, "no active sessions") {
		t.Fatalf("should not have returned to home; screen:\n%s", screen)
	}
	if h.Daemon.LastCreateSession() != nil {
		t.Fatalf("CreateSession should not have been called when esc was pressed")
	}
}

func TestTUI_NewSessionView_Cancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testMultiRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Select a repository"); err != nil {
		t.Fatal(err)
	}

	// Press esc to cancel.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatalf("expected home view after cancel; screen:\n%s", h.Driver.Screen())
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

func TestTUI_NewSessionView_PRSelect_FilterActivates(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	// Before "/" is pressed, the bar advertises the feature.
	if !strings.Contains(h.Driver.Screen(), "[/] filter") {
		t.Fatalf("expected '[/] filter' hint in action bar; screen:\n%s", h.Driver.Screen())
	}

	// Press "/" — enter filter mode.
	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "type to filter"); err != nil {
		t.Fatalf("expected filter action bar; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_NewSessionView_PRSelect_FilterNarrows(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "type to filter"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("login"); err != nil {
		t.Fatal(err)
	}

	// Count indicator shows "1 of 5".
	if err := h.Driver.WaitForText(waitTimeout, "1 of 5"); err != nil {
		t.Fatalf("expected '1 of 5' count; screen:\n%s", h.Driver.Screen())
	}
	// Matching row present, non-matching rows hidden.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Fix login flow") {
		t.Fatalf("expected matching PR visible; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Add dark mode") || strings.Contains(screen, "Update dependencies") {
		t.Fatalf("expected non-matching PRs hidden; screen:\n%s", screen)
	}
}

func TestTUI_NewSessionView_PRSelect_FilterEscClears(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("login"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "1 of 5"); err != nil {
		t.Fatal(err)
	}

	// Esc in filter mode clears the query and exits filter mode — it should NOT
	// pop back to type select.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "1 of 5"); err != nil {
		t.Fatalf("expected filter line gone after esc; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("expected full list restored; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Create a new PR") {
		t.Fatalf("esc from filter should NOT have popped to type select; screen:\n%s", screen)
	}
}

func TestTUI_NewSessionView_PRSelect_FilterNoMatches(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("xyzzy"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "no matches"); err != nil {
		t.Fatalf("expected 'no matches'; screen:\n%s", h.Driver.Screen())
	}
	if !strings.Contains(h.Driver.Screen(), "0 of 5") {
		t.Fatalf("expected '0 of 5' count; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_NewSessionView_PRSelect_FilterSelectsCorrectPR is the load-bearing
// coverage for the indexing fix in startCreating: after filter narrows the list
// to a single match, pressing enter must create a session for that PR's
// original number, not the first PR in the unfiltered slice.
func TestTUI_NewSessionView_PRSelect_FilterSelectsCorrectPR(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithPRs("repo-1", manyPRs()...),
	)
	navigateToPRSelect(t, h)

	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	// "refactor" matches only PR #103.
	if err := h.Driver.SendString("refactor"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "1 of 5"); err != nil {
		t.Fatal(err)
	}
	// Commit the filter.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[/] edit filter"); err != nil {
		t.Fatalf("expected applied-filter action bar; screen:\n%s", h.Driver.Screen())
	}
	// Now press enter again to select the single matching PR.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	// The TUI will call CreateSession; we assert on the captured request.
	// Wait briefly for the RPC to arrive, then verify.
	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.PrNumber == nil {
		t.Fatalf("CreateSession.PrNumber was nil; req=%+v", req)
	}
	if *req.PrNumber != 103 {
		t.Fatalf("CreateSession.PrNumber = %d, want 103 (the refactor PR, not the first PR in the unfiltered list)", *req.PrNumber)
	}
}

// testRepoWithLinear returns a single repo with Linear API key set so the
// "Work on a Linear issue" session type is available.
func testRepoWithLinear() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge", LinearApiKey: "test-linear-key"},
	}
}

func manyIssues() []*pb.TrackerIssue {
	return []*pb.TrackerIssue{
		{ExternalId: "ENG-101", Title: "Fix login redirect", State: "Todo", BranchName: "eng-101-fix-login-redirect"},
		{ExternalId: "ENG-102", Title: "Add dark mode toggle", State: "Todo", BranchName: "eng-102-dark-mode"},
		{ExternalId: "ENG-103", Title: "Refactor auth module", State: "In Progress", BranchName: "eng-103-refactor-auth"},
	}
}

func TestTUI_NewSessionView_IssueSelect_FilterNarrows(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepoWithLinear()...),
		tuitest.WithTrackerIssues("repo-1", manyIssues()...),
	)
	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Work on a Linear issue"); err != nil {
		t.Fatalf("expected Linear option; screen:\n%s", h.Driver.Screen())
	}
	// "Work on a Linear issue" is the 3rd row (index 2): NewPR, ExistingPR, Linear, QuickChat.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "ENG-101"); err != nil {
		t.Fatalf("expected issue list; screen:\n%s", h.Driver.Screen())
	}

	// Activate filter, narrow to the refactor issue.
	if err := h.Driver.SendKey('/'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("eng-103"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "1 of 3"); err != nil {
		t.Fatalf("expected '1 of 3'; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Refactor auth module") {
		t.Fatalf("expected matched issue visible; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Fix login redirect") {
		t.Fatalf("expected non-matched issue hidden; screen:\n%s", screen)
	}
}
