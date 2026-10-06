package session

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/status"
)

// stubWebhookHealth reports a fixed set of repos as webhook-healthy.
type stubWebhookHealth map[string]bool

func (s stubWebhookHealth) WebhookDeliveryHealthy(repo string) bool { return s[repo] }

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
			ch := make(chan SessionEvent, 64)

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
