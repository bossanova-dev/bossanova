package session

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// namedCronJobStore returns a single cron job by any ID, with a configurable
// name, so title-repair tests can assert against the cron default.
type namedCronJobStore struct {
	stubCronJobStore
	name string
}

func (s *namedCronJobStore) Get(context.Context, string) (*models.CronJob, error) {
	return &models.CronJob{Name: s.name}, nil
}

func titleRepairFixture(t *testing.T, sessionTitle, prTitle string) (
	*reconcileMockSessionStore, *reconcileMockProvider, *namedCronJobStore, *models.Session,
) {
	t.Helper()
	repoID := "repo-1"
	cronID := "cron-1"
	prNum := 873

	sessions := newReconcileMockSessionStore()
	sess := &models.Session{
		ID:        "sess-1",
		RepoID:    repoID,
		Title:     sessionTitle,
		PRNumber:  &prNum,
		CronJobID: &cronID,
	}
	sessions.addSession(sess)

	provider := newReconcileMockProvider()
	provider.prStatus[prNum] = &vcs.PRStatus{Title: prTitle}

	cron := &namedCronJobStore{name: "Bossanova auto-implement"}
	return sessions, provider, cron, sess
}

func TestAdoptPRTitleWhenCronTitleStale(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("renames when title is the cron job name", func(t *testing.T) {
		sessions, provider, cron, sess := titleRepairFixture(t,
			"Bossanova auto-implement", "[BOS-76] Make archive cleanup fail gracefully")
		repos := newMockRepoStore()
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}

		updated, changed, err := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !changed {
			t.Fatal("expected rename")
		}
		if updated.Title != "[BOS-76] Make archive cleanup fail gracefully" {
			t.Fatalf("title = %q", updated.Title)
		}
		if sessions.sessions["sess-1"].Title != "[BOS-76] Make archive cleanup fail gracefully" {
			t.Fatal("store not updated")
		}
	})

	t.Run("renames when title is the normalized cron job name", func(t *testing.T) {
		// normalizeCronPRTitle("Bossanova auto-implement") is unchanged here, so
		// use a job name that normalizes differently to exercise the second form.
		sessions, provider, _, sess := titleRepairFixture(t,
			"Auto implement", "[BOS-9] Real title")
		repos := newMockRepoStore()
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}
		cron := &namedCronJobStore{name: "feat: auto implement"} // normalizes to "Auto implement"

		_, changed, err := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !changed {
			t.Fatalf("expected rename for normalized default; normalizeCronPRTitle(%q)=%q",
				"feat: auto implement", normalizeCronPRTitle("feat: auto implement"))
		}
	})

	t.Run("no-op when title was already set meaningfully", func(t *testing.T) {
		sessions, provider, cron, sess := titleRepairFixture(t,
			"[BOS-1] A deliberate title", "[BOS-1] A deliberate title (newer)")
		repos := newMockRepoStore()
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}

		_, changed, err := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if changed {
			t.Fatal("must not clobber a non-default title")
		}
	})

	t.Run("no-op when PR title is empty or also the default", func(t *testing.T) {
		for _, prTitle := range []string{"", "Bossanova auto-implement"} {
			sessions, provider, cron, sess := titleRepairFixture(t, "Bossanova auto-implement", prTitle)
			repos := newMockRepoStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}

			_, changed, err := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if changed {
				t.Fatalf("must not adopt a useless PR title %q", prTitle)
			}
		}
	})

	t.Run("no-op for non-cron or PR-less sessions", func(t *testing.T) {
		sessions, provider, cron, sess := titleRepairFixture(t, "Bossanova auto-implement", "[BOS-2] Real")
		repos := newMockRepoStore()
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}

		sess.CronJobID = nil
		if _, changed, _ := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess); changed {
			t.Fatal("non-cron session must be untouched")
		}
		cronID := "cron-1"
		sess.CronJobID = &cronID
		sess.PRNumber = nil
		if _, changed, _ := adoptPRTitleWhenCronTitleStale(ctx, sessions, repos, cron, provider, logger, sess); changed {
			t.Fatal("PR-less session must be untouched")
		}
	})
}

func TestReconcileSessionsRepairsStaleCronTitles(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	originURL := "https://github.com/acme/widgets"
	prNum := 877

	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: originURL}

	sessions := newReconcileMockSessionStore()
	cronID := "cron-1"
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bossanova auto-implement",
		BranchName: "cron-bossanova-auto-implement-1",
		PRNumber:   &prNum,
		CronJobID:  &cronID,
	})

	provider := newReconcileMockProvider()
	provider.prStatus[prNum] = &vcs.PRStatus{Title: "[BOS-86] Surface finalize errors"}
	cron := &namedCronJobStore{name: "Bossanova auto-implement"}

	var notified int
	r := NewPRAssociationResolver(sessions, repos, provider, logger).
		WithCronJobs(cron).
		WithUpdateNotifier(func(context.Context, *models.Session) { notified++ })

	active, _ := sessions.ListActive(ctx, "")
	n, err := r.ReconcileSessions(ctx, active)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}
	if got := sessions.sessions["sess-1"].Title; got != "[BOS-86] Surface finalize errors" {
		t.Fatalf("title = %q", got)
	}
	if notified != 1 {
		t.Fatalf("notified = %d, want 1", notified)
	}

	// One-shot: a second pass over the now-meaningful title is a no-op.
	active, _ = sessions.ListActive(ctx, "")
	if n, _ := r.ReconcileSessions(ctx, active); n != 0 {
		t.Fatalf("second pass updated = %d, want 0", n)
	}
}

func TestReconcileSessionsSkipsCronTitleRepairWithoutCronStore(t *testing.T) {
	ctx := context.Background()
	prNum := 873
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/widgets"}
	sessions := newReconcileMockSessionStore()
	cronID := "cron-1"
	sessions.addSession(&models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Bossanova auto-implement",
		BranchName: "b", PRNumber: &prNum, CronJobID: &cronID,
	})
	provider := newReconcileMockProvider()
	provider.prStatus[prNum] = &vcs.PRStatus{Title: "[BOS-1] Real"}

	// No WithCronJobs → repair pass is skipped.
	r := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	active, _ := sessions.ListActive(ctx, "")
	if n, _ := r.ReconcileSessions(ctx, active); n != 0 {
		t.Fatalf("updated = %d, want 0 without a cron store", n)
	}
	if sessions.sessions["sess-1"].Title != "Bossanova auto-implement" {
		t.Fatal("title changed despite no cron store")
	}
}
