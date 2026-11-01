package session

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// stubWebhookHealth reports a fixed set of repos as webhook-healthy.
type stubWebhookHealth map[string]bool

func (s stubWebhookHealth) WebhookDeliveryHealthy(repo string) bool { return s[repo] }

func (stubWebhookHealth) NotePolledChange(string, time.Time) {}

func pendingPollerFixture() (*mockSessionStore, *mockRepoStore, *mockVCSProvider) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	prNum := 42
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", State: machine.AwaitingChecks, PRNumber: &prNum,
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "sha"}
	vp.nextCheckResults = []vcs.CheckResult{{Status: vcs.CheckStatusInProgress}}
	return sessions, repos, vp
}

// TestPollerSlowsToSafetyNetForWebhookHealthyRepos pins the read budget the
// webhook pipeline buys: a session whose repo's webhooks reach the daemon is
// checked once per webhookSafetyNetInterval, not every sweep — and a repo
// without that evidence keeps the full cadence.
func TestPollerSlowsToSafetyNetForWebhookHealthyRepos(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
		want    int
	}{
		{"webhook-healthy repo", true, 2},
		{"repo without webhook evidence", false, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, repos, vp := pendingPollerFixture()
			p := NewPoller(sessions, repos, vp, 2*time.Minute, DefaultPollTimeout, zerolog.Nop())
			p.SetWebhookHealth(stubWebhookHealth{"owner/repo": tc.healthy})
			now := time.Unix(1_800_000_000, 0)
			p.now = func() time.Time { return now }
			ch := make(chan Event, 64)

			// Six 2-minute sweeps span 10 minutes: t=0,2,4,6,8,10.
			for i := 0; i < 6; i++ {
				p.sweep(context.Background(), ch)
				now = now.Add(2 * time.Minute)
			}
			if got := len(vp.getPRStatusPRNumbers); got != tc.want {
				t.Fatalf("PR status read %d times over six sweeps, want %d", got, tc.want)
			}
		})
	}
}

// TestPollerPollPRChecksOnDemand pins the webhook's fast path: PollPR checks the
// named PR immediately (so green advances without waiting for the safety-net
// sweep), ignores other PRs, and is inert once Run has stopped.
func TestPollerPollPRChecksOnDemand(t *testing.T) {
	sessions, repos, vp := pendingPollerFixture()
	p := NewPoller(sessions, repos, vp, time.Hour, DefaultPollTimeout, zerolog.Nop())
	p.SetWebhookHealth(stubWebhookHealth{"owner/repo": true})
	ctx, cancel := context.WithCancel(context.Background())
	ch := p.Run(ctx)
	<-p.FirstPollDone()

	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}}

	p.PollPR(context.Background(), "owner/repo", 7)
	select {
	case ev := <-ch:
		t.Fatalf("PollPR for another PR emitted %T", ev.Event)
	default:
	}

	p.PollPR(context.Background(), "owner/repo", 42)
	select {
	case ev := <-ch:
		if _, ok := ev.Event.(vcs.ChecksPassed); !ok || ev.SessionID != "sess-1" {
			t.Fatalf("event = %s %T, want sess-1 ChecksPassed", ev.SessionID, ev.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PollPR did not check the PR")
	}

	cancel()
	<-p.Done()
	// Must not send on the closed channel.
	p.PollPR(context.Background(), "owner/repo", 42)
}

// TestDisplayPollerIntervalOnlyLengthens pins that webhook evidence never
// SHORTENS the configured interval (the old code substituted the 5-minute
// webhook back-off for it), and that a webhook-healthy repo backs off to the
// safety net.
func TestDisplayPollerIntervalOnlyLengthens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	newPoller := func(interval time.Duration) *DisplayPoller {
		return NewDisplayPoller(newMockSessionStore(), newMockRepoStore(), newMockVCSProvider(),
			status.NewDisplayTracker(), interval, zerolog.Nop())
	}

	long := newPoller(15 * time.Minute)
	long.recordRefresh("sess-1", now)
	if got := long.intervalFor("owner/repo", "sess-1", now); got != 15*time.Minute {
		t.Fatalf("credited session with a 15m interval polls every %s, want 15m", got)
	}

	healthy := newPoller(2 * time.Minute)
	healthy.SetWebhookHealth(stubWebhookHealth{"owner/repo": true})
	if got := healthy.intervalFor("owner/repo", "sess-1", now); got != webhookSafetyNetInterval {
		t.Fatalf("webhook-healthy repo polls every %s, want %s", got, webhookSafetyNetInterval)
	}
	if got := healthy.intervalFor("owner/other", "sess-2", now); got != 2*time.Minute {
		t.Fatalf("repo without webhook evidence polls every %s, want 2m", got)
	}
}

// parkedCadence is the interval intervalFor gives one session after it was
// polled, in a repo whose webhook health is health (nil: no tracker wired).
func parkedCadence(t *testing.T, configured time.Duration, health DisplayWebhookHealth, display vcs.DisplayStatus) time.Duration {
	t.Helper()
	now := time.Now()
	tracker := status.NewDisplayTracker()
	p := NewDisplayPoller(newMockSessionStore(), newMockRepoStore(), newMockVCSProvider(),
		tracker, configured, zerolog.Nop())
	if health != nil {
		p.SetWebhookHealth(health)
	}
	tracker.Set("sess-1", vcs.DisplayInfo{Status: display})
	return p.intervalFor("owner/repo", "sess-1", now)
}

// TestDisplayPollerParkedInterval_Sessions_AC164_ParkedPolling pins the
// healthy-webhook cadence: a parked PR with no pending base move backs off to
// the safety net (a base move arrives as a branch push instead), a longer
// configured interval still wins, and a non-parked session keeps today's rules.
func TestDisplayPollerParkedInterval_Sessions_AC164_ParkedPolling(t *testing.T) {
	healthy := stubWebhookHealth{"owner/repo": true}
	for _, tc := range []struct {
		name       string
		configured time.Duration
		display    vcs.DisplayStatus
		want       time.Duration
	}{
		{"parked, 2m configured", 2 * time.Minute, vcs.DisplayStatusNeedsHuman, webhookSafetyNetInterval},
		{"parked, 15m configured", 15 * time.Minute, vcs.DisplayStatusNeedsHuman, 15 * time.Minute},
		{"passing, 2m configured", 2 * time.Minute, vcs.DisplayStatusPassing, webhookSafetyNetInterval},
		{"passing, 15m configured", 15 * time.Minute, vcs.DisplayStatusPassing, 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parkedCadence(t, tc.configured, healthy, tc.display); got != tc.want {
				t.Fatalf("polls every %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDisplayPollerParkedFallback_Sessions_AC226001_NoWebhookPolling pins the
// cadence without webhook delivery: no push can announce a base move, so a
// parked PR is still read every parkedFallbackInterval (5 minutes) unless the
// configured interval is longer, while a non-parked session keeps the
// configured interval.
func TestDisplayPollerParkedFallback_Sessions_AC226001_NoWebhookPolling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		health     DisplayWebhookHealth
		configured time.Duration
		display    vcs.DisplayStatus
		want       time.Duration
	}{
		{"parked, no tracker, 2m", nil, 2 * time.Minute, vcs.DisplayStatusNeedsHuman, 5 * time.Minute},
		{"parked, unhealthy, 2m", stubWebhookHealth{}, 2 * time.Minute, vcs.DisplayStatusNeedsHuman, 5 * time.Minute},
		{"parked, no tracker, 15m", nil, 15 * time.Minute, vcs.DisplayStatusNeedsHuman, 15 * time.Minute},
		{"parked, unhealthy, 15m", stubWebhookHealth{}, 15 * time.Minute, vcs.DisplayStatusNeedsHuman, 15 * time.Minute},
		{"passing, no tracker, 2m", nil, 2 * time.Minute, vcs.DisplayStatusPassing, 2 * time.Minute},
		{"passing, unhealthy, 2m", stubWebhookHealth{}, 2 * time.Minute, vcs.DisplayStatusPassing, 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parkedCadence(t, tc.configured, tc.health, tc.display); got != tc.want {
				t.Fatalf("polls every %s, want %s", got, tc.want)
			}
		})
	}
}

// baseMoveFixture is a webhook-healthy display poller over two open PRs in one
// repo: #1 targets main and #2 targets develop. Both are parked and were just
// polled, so neither is due on the safety-net cadence.
func baseMoveFixture(t *testing.T) (*DisplayPoller, *mockVCSProvider) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	one, two := 1, 2
	sessions.sessions["sess-main"] = &models.Session{ID: "sess-main", RepoID: "repo-1", PRNumber: &one, BaseBranch: "main"}
	sessions.sessions["sess-dev"] = &models.Session{ID: "sess-dev", RepoID: "repo-1", PRNumber: &two, BaseBranch: "develop"}
	mergeable := true
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1", Mergeable: &mergeable}
	vp.nextCheckResults = parkedChecks()
	tracker := status.NewDisplayTracker()
	p := NewDisplayPoller(sessions, repos, vp, tracker, 2*time.Minute, zerolog.Nop())
	p.SetWebhookHealth(stubWebhookHealth{"owner/repo": true})
	p.poll(context.Background())
	for _, id := range []string{"sess-main", "sess-dev"} {
		if got := tracker.Get(id); got == nil || got.Status != vcs.DisplayStatusNeedsHuman {
			t.Fatalf("%s display = %+v, want NEEDS_HUMAN", id, got)
		}
	}
	vp.getPRStatusPRNumbers = nil
	p.poll(context.Background())
	if got := vp.getPRStatusPRNumbers; len(got) != 0 {
		t.Fatalf("parked PRs re-read %v right after a poll, want none", got)
	}
	return p, vp
}

// TestDisplayPollerNoteBaseMovedMarksTargetingPRsDue pins due-marking: only
// the PR targeting the pushed branch is read on the very next cycle, and
// NoteBaseMoved returns exactly its number.
func TestDisplayPollerNoteBaseMovedMarksTargetingPRsDue(t *testing.T) {
	p, vp := baseMoveFixture(t)

	if got := p.NoteBaseMoved(context.Background(), "owner/repo", "main"); !slices.Equal(got, []int{1}) {
		t.Fatalf("NoteBaseMoved = %v, want [1]", got)
	}
	p.poll(context.Background())
	if got := vp.getPRStatusPRNumbers; !slices.Equal(got, []int{1}) {
		t.Fatalf("PRs read after the base move = %v, want only #1", got)
	}
	if got := p.NoteBaseMoved(context.Background(), "owner/other", "main"); got != nil {
		t.Fatalf("NoteBaseMoved for an unknown repo = %v, want none", got)
	}
}

// TestDisplayPollerNoteBaseMovedUsesTheReadBase pins that a PR retargeted on
// GitHub is matched by the base its last read reported, not the session's.
func TestDisplayPollerNoteBaseMovedUsesTheReadBase(t *testing.T) {
	p, vp := baseMoveFixture(t)
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1", BaseBranch: "release"}
	p.NoteBaseMoved(context.Background(), "owner/repo", "main")
	p.poll(context.Background()) // #1 re-read: it now targets release

	if got := p.NoteBaseMoved(context.Background(), "owner/repo", "main"); len(got) != 0 {
		t.Fatalf("NoteBaseMoved(main) after the retarget = %v, want none", got)
	}
	if got := p.NoteBaseMoved(context.Background(), "owner/repo", "release"); !slices.Equal(got, []int{1}) {
		t.Fatalf("NoteBaseMoved(release) = %v, want [1]", got)
	}
}

// TestDisplayPollerBaseMoveHoldsUntilMergeabilityKnown pins the mark's life: a
// read that reports mergeability unknown keeps the configured interval, a read
// that reports it returns the PR to its parked cadence, and a mark older than
// the safety net no longer shortens the interval.
func TestDisplayPollerBaseMoveHoldsUntilMergeabilityKnown(t *testing.T) {
	p, vp := baseMoveFixture(t)
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"} // mergeability unknown
	p.NoteBaseMoved(context.Background(), "owner/repo", "main")
	p.poll(context.Background())

	now := time.Now()
	if got := p.intervalFor("owner/repo", "sess-main", now); got != 2*time.Minute {
		t.Fatalf("unknown mergeability after a base move: polls every %s, want the configured 2m", got)
	}
	if got := p.intervalFor("owner/repo", "sess-main", now.Add(webhookSafetyNetInterval)); got != webhookSafetyNetInterval {
		t.Fatalf("expired base-move mark: polls every %s, want %s", got, webhookSafetyNetInterval)
	}

	mergeable := true
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1", Mergeable: &mergeable}
	p.NoteBaseMoved(context.Background(), "owner/repo", "main")
	p.poll(context.Background())
	if got := p.intervalFor("owner/repo", "sess-main", time.Now()); got != webhookSafetyNetInterval {
		t.Fatalf("known mergeability: polls every %s, want parked cadence %s", got, webhookSafetyNetInterval)
	}
}

// TestDisplayPollerBaseMoveConflictReplacesPark pins the outcome: a parked PR
// that a base push made conflicting shows CONFLICT after the first cycle that
// reads Mergeable == false.
func TestDisplayPollerBaseMoveConflictReplacesPark(t *testing.T) {
	p, vp := baseMoveFixture(t)
	conflicting := false
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1", Mergeable: &conflicting}

	p.NoteBaseMoved(context.Background(), "owner/repo", "main")
	p.poll(context.Background())

	if got := p.tracker.Get("sess-main"); got == nil || got.Status != vcs.DisplayStatusConflict {
		t.Fatalf("sess-main display = %+v, want CONFLICT", got)
	}
	if got := p.tracker.Get("sess-dev"); got == nil || got.Status != vcs.DisplayStatusNeedsHuman {
		t.Fatalf("sess-dev display = %+v, want NEEDS_HUMAN (its base did not move)", got)
	}
}

type polledChange struct {
	repo  string
	since time.Time
}

// recordingMissHealth records every PR change the display poller reports.
type recordingMissHealth struct{ changes []polledChange }

func (*recordingMissHealth) WebhookDeliveryHealthy(string) bool { return false }

func (r *recordingMissHealth) NotePolledChange(repo string, since time.Time) {
	r.changes = append(r.changes, polledChange{repo, since})
}

// TestDisplayPollerReportsWebhookAnnouncedChanges pins the evidence behind
// Repo.is_missing_webhooks: the display poller reports a new head or a PR
// ending between two of its own reads, dated by the earlier read, and nothing
// else (the first read after a restart, an unchanged PR, a checks-only change).
func TestDisplayPollerReportsWebhookAnnouncedChanges(t *testing.T) {
	poller, _ := newEvidencePoller(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	health := &recordingMissHealth{}
	poller.SetWebhookHealth(health)
	vp := poller.provider.(*mockVCSProvider)
	ctx := context.Background()
	read := func() time.Time {
		t.Helper()
		at := time.Now()
		if err := poller.RefreshPRWithoutWebhookCredit(ctx, "owner/repo", 2979); err != nil {
			t.Fatalf("RefreshPRWithoutWebhookCredit: %v", err)
		}
		return at
	}
	wantChanges := func(n int, step string) {
		t.Helper()
		if len(health.changes) != n {
			t.Fatalf("%s: reported changes = %+v, want %d", step, health.changes, n)
		}
	}

	firstRead := read()
	wantChanges(0, "first read since start")
	vp.nextCheckResults = nil
	read()
	wantChanges(0, "checks-only change")

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-2"}
	headRead := read()
	wantChanges(1, "new head")
	if got := health.changes[0]; got.repo != "owner/repo" || got.since.Before(firstRead) || !got.since.Before(headRead) {
		t.Fatalf("new head reported as %+v, want repo owner/repo dated by an earlier read (in [%v, %v))", got, firstRead, headRead)
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}
	read()
	wantChanges(2, "PR merged")
}
