package session

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/ghusage"
	"github.com/recurser/bossd/internal/status"
)

// labelLog collects the usage label each provider call carried.
type labelLog struct {
	mu     sync.Mutex
	labels []string
}

func (l *labelLog) add(ctx context.Context) {
	l.mu.Lock()
	l.labels = append(l.labels, ghusage.LabelFrom(ctx))
	l.mu.Unlock()
}

// only fails unless at least one call was recorded and every call carried want.
func (l *labelLog) only(t *testing.T, want string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.labels) == 0 {
		t.Fatalf("no GitHub call reached the provider; want calls labelled %q", want)
	}
	if i := slices.IndexFunc(l.labels, func(got string) bool { return got != want }); i >= 0 {
		t.Fatalf("GitHub call %d label = %q, want every call labelled %q (all: %v)", i, l.labels[i], want, l.labels)
	}
}

// statusLabelVCS records the label every PR status read carries.
type statusLabelVCS struct {
	*mockVCSProvider
	log labelLog
}

func (v *statusLabelVCS) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	v.log.add(ctx)
	return v.mockVCSProvider.GetPRStatus(ctx, repoPath, prID)
}

func pollerLabelFixture() (*mockSessionStore, *mockRepoStore, *statusLabelVCS) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	prNum := 42
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", State: machine.AwaitingChecks, PRNumber: &prNum,
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "sha-1", Mergeable: boolPtr(true)}
	return sessions, repos, &statusLabelVCS{mockVCSProvider: vp}
}

// TestPRPollerMetersItsCallsAsThePRPoller pins the PR status poller's label:
// a sweep reached with an unlabelled context reads GitHub as daemon:pr-poller.
func TestPRPollerMetersItsCallsAsThePRPoller(t *testing.T) {
	sessions, repos, vp := pollerLabelFixture()
	p := NewPoller(sessions, repos, vp, time.Minute, DefaultPollTimeout, zerolog.Nop())
	p.poll(context.Background(), make(chan Event, 64))
	vp.log.only(t, ghusage.LabelPRPoller)
}

// TestDisplayPollerMetersItsCallsAsTheDisplayPoller pins the display poller's
// label on its scheduled sweep and on the webhook-triggered RefreshPR, while
// the uncredited refresh keeps the label of the caller that acted on the PR.
func TestDisplayPollerMetersItsCallsAsTheDisplayPoller(t *testing.T) {
	sessions, repos, vp := pollerLabelFixture()
	p := NewDisplayPoller(sessions, repos, vp, status.NewDisplayTracker(), time.Minute, zerolog.Nop())
	p.poll(context.Background())
	vp.log.only(t, ghusage.LabelDisplayPoller)

	vp.log = labelLog{}
	if err := p.RefreshPR(context.Background(), "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}
	vp.log.only(t, ghusage.LabelDisplayPoller)

	vp.log = labelLog{}
	mergeCtx := ghusage.WithLabel(context.Background(), ghusage.LabelMerge)
	if err := p.RefreshPRWithoutWebhookCredit(mergeCtx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPRWithoutWebhookCredit: %v", err)
	}
	vp.log.only(t, ghusage.LabelMerge)
}

// reconcileLabelVCS records the label of reconcile's two GitHub reads.
type reconcileLabelVCS struct {
	*reconcileMockProvider
	status, listing labelLog
}

func (v *reconcileLabelVCS) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	v.status.add(ctx)
	return v.reconcileMockProvider.GetPRStatus(ctx, repoPath, prID)
}

func (v *reconcileLabelVCS) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	v.listing.add(ctx)
	return v.reconcileMockProvider.ListOpenPRs(ctx, repoPath)
}

// TestReconcileMetersItsTwoReadsUnderTheirOwnLabels pins reconcile's labels:
// the no-commits title repair reads as daemon:reconcile-title and the
// find-PR-for-session listing as daemon:reconcile-pr-lookup.
func TestReconcileMetersItsTwoReadsUnderTheirOwnLabels(t *testing.T) {
	ctx := context.Background()
	originURL := "https://github.com/acme/widgets"
	prNum := 877
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: originURL}
	sessions := newReconcileMockSessionStore()
	cronID := "cron-1"
	sessions.addSession(&models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Bossanova auto-implement",
		BranchName: "cron-bossanova-auto-implement-1", PRNumber: &prNum, CronJobID: &cronID,
	})
	provider := &reconcileLabelVCS{reconcileMockProvider: newReconcileMockProvider()}
	provider.prStatus[prNum] = &vcs.PRStatus{Title: "[BOS-86] Surface finalize errors"}
	r := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithCronJobs(&namedCronJobStore{name: "Bossanova auto-implement"})

	active, _ := sessions.ListActive(ctx, "")
	if _, err := r.ReconcileSessions(ctx, active); err != nil {
		t.Fatalf("ReconcileSessions: %v", err)
	}
	provider.status.only(t, ghusage.LabelReconcileTitle)

	if _, err := r.OpenPRs(ctx, "repo-1", originURL); err != nil {
		t.Fatalf("OpenPRs: %v", err)
	}
	provider.listing.only(t, ghusage.LabelReconcilePRLookup)
}
