package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// repairEligibleRepoStoreFake feeds repairEligibleSessionsCheck a fixed repo
// list (or an error). Only List is exercised; the rest of RepoStore is left to
// the embedded interface and panics if unexpectedly called.
type repairEligibleRepoStoreFake struct {
	db.RepoStore
	repos []*models.Repo
	err   error
}

func (f *repairEligibleRepoStoreFake) List(context.Context) ([]*models.Repo, error) {
	return f.repos, f.err
}

// repairEligibleSessionStoreFake returns a fixed active-session list (and
// optional error) for every repo. ListActive is the only method the check
// touches.
type repairEligibleSessionStoreFake struct {
	db.SessionStore
	sessions []*models.Session
	err      error
}

func (f *repairEligibleSessionStoreFake) ListActive(context.Context, string) ([]*models.Session, error) {
	return f.sessions, f.err
}

func repairEligibleTestServer(repos *repairEligibleRepoStoreFake, sessions *repairEligibleSessionStoreFake) *Server {
	return &Server{
		repos:          repos,
		sessions:       sessions,
		displayTracker: status.NewDisplayTracker(),
	}
}

func TestRepairEligibleSessionsCheck(t *testing.T) {
	ctx := context.Background()
	repo := &models.Repo{ID: "repo-1"}

	t.Run("counts eligible and ineligible with example detail", func(t *testing.T) {
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{sessions: []*models.Session{
				{ID: "eligible-1", State: machine.AwaitingChecks},
				{ID: "ineligible-1", State: machine.Merged},
			}},
		)
		// A live display entry must flow into the example detail.
		srv.displayTracker.Set("eligible-1", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})

		check := srv.repairEligibleSessionsCheck(ctx)

		if !check.GetOk() {
			t.Fatalf("with an eligible session the check must be Ok, detail=%q", check.GetDetail())
		}
		detail := check.GetDetail()
		for _, want := range []string{
			"eligible=1",
			"ineligible=1",
			// shortID of "eligible-1" plus the live display status (Checking=2).
			"display=2",
			"state=",
		} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail missing %q: %q", want, detail)
			}
		}
		// The example list must be populated (kills the `len(examples) < 5`
		// negation that would suppress all examples).
		if !strings.Contains(detail, "examples=[") || strings.Contains(detail, "examples=[]") {
			t.Errorf("expected a non-empty examples list, detail=%q", detail)
		}
	})

	t.Run("nil display entry yields zero display status", func(t *testing.T) {
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{sessions: []*models.Session{
				{ID: "eligible-1", State: machine.ReadyForReview},
			}},
		)
		// No displayTracker.Set: Get returns nil, so display must be 0
		// (kills `entry != nil` -> `entry == nil`, which would read a status).
		check := srv.repairEligibleSessionsCheck(ctx)
		if !strings.Contains(check.GetDetail(), "display=0") {
			t.Errorf("expected display=0 for a missing entry, detail=%q", check.GetDetail())
		}
	})

	t.Run("caps the example list at five", func(t *testing.T) {
		sessions := make([]*models.Session, 0, 6)
		for _, id := range []string{"e1", "e2", "e3", "e4", "e5", "e6"} {
			sessions = append(sessions, &models.Session{ID: id, State: machine.GreenDraft})
		}
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{sessions: sessions},
		)

		check := srv.repairEligibleSessionsCheck(ctx)

		if !strings.Contains(check.GetDetail(), "eligible=6") {
			t.Errorf("expected eligible=6, detail=%q", check.GetDetail())
		}
		// Exactly five example entries (each rendered as "(state=...)") must be
		// recorded even though six sessions are eligible. This pins the
		// `len(examples) < 5` boundary: `<= 5` would record a sixth.
		if got := strings.Count(check.GetDetail(), "(state="); got != 5 {
			t.Errorf("example count = %d, want 5; detail=%q", got, check.GetDetail())
		}
	})

	t.Run("only ineligible sessions is not Ok", func(t *testing.T) {
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{sessions: []*models.Session{
				{ID: "x1", State: machine.Merged},
				{ID: "x2", State: machine.Closed},
			}},
		)

		check := srv.repairEligibleSessionsCheck(ctx)

		// eligible==0 and ineligible>0: Ok must be false. Kills the
		// `eligible > 0` boundary/negation mutants, which flip Ok to true.
		if check.GetOk() {
			t.Fatalf("eligible=0 with ineligible>0 must not be Ok, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "eligible=0") || !strings.Contains(check.GetDetail(), "ineligible=2") {
			t.Errorf("detail = %q, want eligible=0 ineligible=2", check.GetDetail())
		}
	})

	t.Run("no active sessions is Ok", func(t *testing.T) {
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{sessions: nil},
		)

		check := srv.repairEligibleSessionsCheck(ctx)

		// eligible==0 and ineligible==0: Ok must be true. Kills the
		// `ineligible == 0` -> `ineligible != 0` negation, which flips Ok.
		if !check.GetOk() {
			t.Fatalf("empty repo must be Ok, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "eligible=0 ineligible=0") {
			t.Errorf("detail = %q, want eligible=0 ineligible=0", check.GetDetail())
		}
	})

	t.Run("list repos error surfaces", func(t *testing.T) {
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{err: errors.New("boom")},
			&repairEligibleSessionStoreFake{},
		)

		check := srv.repairEligibleSessionsCheck(ctx)

		if check.GetOk() {
			t.Fatalf("a repo-list error must not be Ok, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "list repos: boom") {
			t.Errorf("detail = %q, want list repos error", check.GetDetail())
		}
	})

	t.Run("list-active error skips the repo without counting", func(t *testing.T) {
		// ListActive returns sessions AND an error: the real code must `continue`
		// (ignoring the sessions). A mutant that drops the error guard would
		// count them. Kills `err != nil` -> `err == nil` at the ListActive call.
		srv := repairEligibleTestServer(
			&repairEligibleRepoStoreFake{repos: []*models.Repo{repo}},
			&repairEligibleSessionStoreFake{
				sessions: []*models.Session{{ID: "e1", State: machine.AwaitingChecks}},
				err:      errors.New("query failed"),
			},
		)

		check := srv.repairEligibleSessionsCheck(ctx)

		if !strings.Contains(check.GetDetail(), "eligible=0 ineligible=0") {
			t.Errorf("a ListActive error must skip counting, detail=%q", check.GetDetail())
		}
	})

	t.Run("missing deps reports not configured", func(t *testing.T) {
		// Both deps configured is the happy path (covered above); here the guard
		// itself: a nil sessions store must short-circuit to "not configured"
		// rather than dereferencing it.
		srv := &Server{repos: &repairEligibleRepoStoreFake{}, sessions: nil}

		check := srv.repairEligibleSessionsCheck(ctx)

		if check.GetOk() {
			t.Fatalf("missing deps must not be Ok, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "session deps not configured") {
			t.Errorf("detail = %q, want session deps not configured", check.GetDetail())
		}
	})
}
