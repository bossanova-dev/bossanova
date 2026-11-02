package session

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// fallbackCatchUpBound is the operator's requirement for a daemon that reads
// GitHub without fresh webhooks (no cloud login, or the installation-token
// fallback): every open-PR session is still checked at least this often.
const fallbackCatchUpBound = 10 * time.Minute

// catchUpCheckRecorder records when each PR's status was read.
type catchUpCheckRecorder struct {
	*mockVCSProvider

	mu     sync.Mutex
	checks map[string][]time.Time
}

func (r *catchUpCheckRecorder) GetPRStatus(ctx context.Context, repoPath string, prNumber int) (*vcs.PRStatus, error) {
	r.mu.Lock()
	key := fmt.Sprintf("%s#%d", repoPath, prNumber)
	r.checks[key] = append(r.checks[key], time.Now())
	r.mu.Unlock()
	return r.mockVCSProvider.GetPRStatus(ctx, repoPath, prNumber)
}

// TestPollerFallbackCatchUp_Sessions_AC226401_WithinBound pins that the CI poller, at the
// production DefaultPollInterval, checks every pollable open-PR session in
// every fallbackCatchUpBound window, whether webhooks are absent, unhealthy
// or healthy (the safety net). A cadence change that breaks the bound fails.
func TestPollerFallbackCatchUp_Sessions_AC226401_WithinBound(t *testing.T) {
	const (
		originA = "https://github.com/owner/a"
		originB = "https://github.com/owner/b"
		runFor  = time.Hour
	)
	cases := []struct {
		name   string
		health WebhookHealth
	}{
		{"no webhook health (no cloud login)", nil},
		{"every repo unhealthy", stubWebhookHealth{}},
		{"every repo healthy", stubWebhookHealth{originA: true, originB: true}},
	}
	checksPerCase := map[string]int{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sessions := newMockSessionStore()
				repos := newMockRepoStore()
				repos.repos["repo-a"] = &models.Repo{ID: "repo-a", OriginURL: originA}
				repos.repos["repo-b"] = &models.Repo{ID: "repo-b", OriginURL: originB}

				var want []string
				pr := 0
				for _, repo := range []struct{ id, origin string }{{"repo-a", originA}, {"repo-b", originB}} {
					for _, state := range []machine.State{machine.AwaitingChecks, machine.FixingChecks, machine.GreenDraft, machine.ReadyForReview} {
						pr++
						n := pr
						id := repo.id + "-" + state.String()
						sessions.sessions[id] = &models.Session{ID: id, RepoID: repo.id, State: state, PRNumber: &n}
						want = append(want, fmt.Sprintf("%s#%d", repo.origin, n))
					}
				}
				sessions.sessions["no-pr"] = &models.Session{ID: "no-pr", RepoID: "repo-a", State: machine.AwaitingChecks}

				vp := &catchUpCheckRecorder{mockVCSProvider: newMockVCSProvider(), checks: map[string][]time.Time{}}
				poller := NewPoller(sessions, repos, vp, DefaultPollInterval, DefaultPollTimeout, zerolog.Nop())
				poller.SetWebhookHealth(tc.health)

				ctx, cancel := context.WithCancel(context.Background())
				start := time.Now()
				events := poller.Run(ctx)
				drained := make(chan struct{})
				go func() {
					defer close(drained)
					for range events {
					}
				}()

				time.Sleep(runFor)
				synctest.Wait()
				end := time.Now()
				cancel()
				<-poller.Done()
				<-drained

				vp.mu.Lock()
				defer vp.mu.Unlock()
				if len(vp.checks) != len(want) {
					t.Fatalf("checked %d PRs, want %d pollable PR sessions", len(vp.checks), len(want))
				}
				total := 0
				for _, key := range want {
					times := vp.checks[key]
					if len(times) == 0 {
						t.Fatalf("%s was never checked", key)
					}
					total += len(times)
					prev := start
					for _, at := range append(slices.Clone(times), end) {
						if gap := at.Sub(prev); gap > fallbackCatchUpBound {
							t.Fatalf("%s went %s unchecked (after %s), want at most %s", key, gap, prev.Sub(start), fallbackCatchUpBound)
						}
						prev = at
					}
				}
				checksPerCase[tc.name] = total
			})
		})
	}
	// The healthy case must actually be on the safety net, or it proves
	// nothing beyond the unhealthy case.
	if healthy, unhealthy := checksPerCase["every repo healthy"], checksPerCase["every repo unhealthy"]; healthy == 0 || healthy >= unhealthy {
		t.Fatalf("healthy-repo checks = %d, unhealthy = %d; want the safety net to check less often", healthy, unhealthy)
	}
}
