package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// navigateToRepoAddInput presses 'r' on home then 'a' on the repo list to open
// the add-repo wizard, and picks "Open project" to land on the path-input form.
func navigateToRepoAddInput(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatalf("expected repo list; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Open project"); err != nil {
		t.Fatalf("expected source phase; screen:\n%s", h.Driver.Screen())
	}
	// First row "Open project" is already selected — pick it to advance to input.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add a local repository"); err != nil {
		t.Fatalf("expected input phase; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_RepoAddView_ValidatesPath drives the wizard to the path-input phase,
// forces ValidateRepoPath to return an invalid response, and asserts the view
// surfaces the error and never calls RegisterRepo.
func TestTUI_RepoAddView_ValidatesPath(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)
	h.Daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{
		IsValid:      false,
		ErrorMessage: "path is not a git repository",
	})

	navigateToRepoAddInput(t, h)

	// Submit the pre-filled path — the mock will reject it.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "path is not a git repository"); err != nil {
		t.Fatalf("expected validation error on screen; screen:\n%s", h.Driver.Screen())
	}

	if calls := h.Daemon.RegisterRepoCalls(); len(calls) != 0 {
		t.Fatalf("RegisterRepo should not have been called on invalid path; got %d calls", len(calls))
	}
}

// TestTUI_RepoAddView_CreatesRepo drives the wizard end-to-end — path entry,
// validation success, default-populated name/setup/confirm — and asserts the
// daemon received a RegisterRepoRequest reflecting the validated origin URL
// and detected default branch.
func TestTUI_RepoAddView_CreatesRepo(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)
	// Override validation to return a GitHub repo so the auto-populated name
	// is deterministic ("@acme/widgets") regardless of the developer's $HOME.
	h.Daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		OriginUrl:     "https://github.com/acme/widgets.git",
		DefaultBranch: "develop",
	})

	navigateToRepoAddInput(t, h)

	// Append a deterministic suffix to the pre-filled $HOME + "/" path so the
	// captured LocalPath has a known ending we can assert on.
	if err := h.Driver.SendString("widgets"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Validation advances to the details phase (Name / Setup / Confirm).
	if err := h.Driver.WaitForText(waitTimeout, "Add this repository?"); err != nil {
		t.Fatalf("expected details phase; screen:\n%s", h.Driver.Screen())
	}

	// Walk through the 3 fields: Name (pre-filled), Setup (empty), Confirm (Yes).
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Poll for the captured request.
	deadline := time.Now().Add(waitTimeout)
	var req *pb.RegisterRepoRequest
	for time.Now().Before(deadline) {
		if calls := h.Daemon.RegisterRepoCalls(); len(calls) > 0 {
			req = calls[len(calls)-1]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("RegisterRepo was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.DisplayName != "@acme/widgets" {
		t.Fatalf("RegisterRepo.DisplayName = %q, want %q", req.DisplayName, "@acme/widgets")
	}
	if req.DefaultBaseBranch != "develop" {
		t.Fatalf("RegisterRepo.DefaultBaseBranch = %q, want %q", req.DefaultBaseBranch, "develop")
	}
	if !strings.HasSuffix(req.LocalPath, "/widgets") {
		t.Fatalf("RegisterRepo.LocalPath = %q, want suffix %q", req.LocalPath, "/widgets")
	}
	if req.SetupScript != nil {
		t.Fatalf("RegisterRepo.SetupScript = %v, want nil when setup left blank", req.SetupScript)
	}
}

// TestTUI_RepoAddView_Cancel asserts esc on the source phase pops back to the
// repo list (not home). app.go swaps the active view to ViewRepoList whenever
// the wizard signals Cancelled(), preserving the previous cursor highlight.
func TestTUI_RepoAddView_Cancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatalf("expected repo list; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Open project"); err != nil {
		t.Fatalf("expected source phase; screen:\n%s", h.Driver.Screen())
	}

	// Esc on the source phase cancels the wizard.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Should return to the repo list — "PATH" header + seeded repo name visible,
	// and the "Open project" source row no longer on screen.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "PATH") && strings.Contains(screen, "my-app") &&
			!strings.Contains(screen, "Open project")
	}); err != nil {
		t.Fatalf("expected repo list after cancel; screen:\n%s", h.Driver.Screen())
	}

	if screen := h.Driver.Screen(); strings.Contains(screen, "no active sessions") {
		t.Fatalf("cancel from RepoAdd should return to repo list, not home; screen:\n%s", screen)
	}
}
