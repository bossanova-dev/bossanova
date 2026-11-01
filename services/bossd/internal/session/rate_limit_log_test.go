package session

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// errQuotaRefused is a provider read refused for an exhausted GitHub quota.
var errQuotaRefused = fmt.Errorf("get PR: %w", vcs.ErrRateLimited)

// logLines counts the JSON log lines at level carrying msg.
func logLines(logs, level, msg string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"level":"`+level+`"`) && strings.Contains(line, `"message":"`+msg) {
			n++
		}
	}
	return n
}

// TestPollerLogsQuotaRefusalOncePerWindow pins the poller's log gate: two
// refused reads in one quota window write one Warn and one Debug, emit no
// event, and a refusal after the window ends warns again.
func TestPollerLogsQuotaRefusalOncePerWindow_Sessions_AC221701(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msg   string
		setup func(*mockVCSProvider)
	}{
		{"pr status", "poller: get PR status", func(vp *mockVCSProvider) { vp.prStatusErr = errQuotaRefused }},
		{"check results", "poller: get check results", func(vp *mockVCSProvider) { vp.checkResultsErr = errQuotaRefused }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vp := newMockVCSProvider()
			tc.setup(vp)
			var logs bytes.Buffer
			poller := NewPoller(newMockSessionStore(), newMockRepoStore(), vp, time.Minute, DefaultPollTimeout,
				zerolog.New(&logs).Level(zerolog.DebugLevel))
			clock := clocktest.New(time.Unix(1_800_000_000, 0))
			poller.rateLimitLog.Now = clock.Now
			prNum := 42
			repo := &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			ch := make(chan Event, 4)
			for _, id := range []string{"sess-1", "sess-2"} {
				sess := &models.Session{ID: id, RepoID: "repo-1", State: machine.AwaitingChecks, PRNumber: &prNum}
				poller.checkSession(context.Background(), ch, repo, sess)
			}
			if got := logLines(logs.String(), "warn", tc.msg); got != 1 {
				t.Fatalf("%q logged %d Warn lines for two refusals in one window, want 1:\n%s", tc.msg, got, logs.String())
			}
			if got := logLines(logs.String(), "debug", tc.msg); got != 1 {
				t.Fatalf("%q logged %d Debug lines for the repeat, want 1", tc.msg, got)
			}
			if len(ch) != 0 {
				t.Fatalf("poller emitted %d events on refused reads, want none", len(ch))
			}

			clock.Advance(5 * time.Minute)
			poller.checkSession(context.Background(), ch, repo,
				&models.Session{ID: "sess-1", RepoID: "repo-1", State: machine.AwaitingChecks, PRNumber: &prNum})
			if got := logLines(logs.String(), "warn", tc.msg); got != 2 {
				t.Fatalf("%q logged %d Warn lines after the window ended, want 2", tc.msg, got)
			}
		})
	}
}

// TestDisplayPollerLogsQuotaRefusalOncePerWindowAndKeepsStatus pins that the
// display poller warns once for two refused reads and keeps the last status.
func TestDisplayPollerLogsQuotaRefusalOncePerWindowAndKeepsStatus_Sessions_AC221701_AC221702(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msg   string
		setup func(*mockVCSProvider)
	}{
		{"pr status", "display poller: get PR status", func(vp *mockVCSProvider) { vp.prStatusErr = errQuotaRefused }},
		{"check results", "display poller: get check results", func(vp *mockVCSProvider) { vp.checkResultsErr = errQuotaRefused }},
		{"review comments", "display poller: get review comments", func(vp *mockVCSProvider) { vp.reviewCommentsErr = errQuotaRefused }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
			tc.setup(vp)
			tracker := status.NewDisplayTracker()
			tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HeadSHA: "abc"})
			var logs bytes.Buffer
			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.New(&logs).Level(zerolog.DebugLevel))
			prNum := 10
			repo := &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			sess := &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: &prNum}

			for i := 0; i < 2; i++ {
				if poller.pollSession(context.Background(), repo, sess, prNum) {
					t.Fatalf("poll %d reported an update on a refused read", i)
				}
			}
			if got := logLines(logs.String(), "warn", tc.msg); got != 1 {
				t.Fatalf("%q logged %d Warn lines for two refusals in one window, want 1:\n%s", tc.msg, got, logs.String())
			}
			e := tracker.Get("sess-1")
			if e == nil || e.Status != vcs.DisplayStatusFailing || e.HeadSHA != "abc" {
				t.Fatalf("tracker entry = %+v, want the previous Failing status kept", e)
			}
		})
	}
}

// TestReconcileLogsQuotaRefusalOncePerWindow pins that two sessions whose PR
// listings are refused in one quota window log one Warn and stay unattached.
func TestReconcileLogsQuotaRefusalOncePerWindow_Sessions_AC221701_AC221702(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()
	for _, id := range []string{"1", "2"} {
		origin := "https://github.com/owner/repo-" + id
		repos.repos["repo-"+id] = &models.Repo{ID: "repo-" + id, OriginURL: origin}
		provider.openErr[origin] = errQuotaRefused
		sessions.addSession(&models.Session{
			ID: "sess-" + id, RepoID: "repo-" + id, BranchName: "feature-" + id, State: machine.AwaitingChecks,
		})
	}
	var logs bytes.Buffer
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.New(&logs).Level(zerolog.DebugLevel))

	if _, err := resolver.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	const msg = "reconcile: find PR for session"
	if got := logLines(logs.String(), "warn", msg); got != 1 {
		t.Fatalf("%q logged %d Warn lines for two refusals in one window, want 1:\n%s", msg, got, logs.String())
	}
	if got := logLines(logs.String(), "debug", msg); got != 1 {
		t.Fatalf("%q logged %d Debug lines for the repeat, want 1", msg, got)
	}
	for _, id := range []string{"sess-1", "sess-2"} {
		if sess := sessions.sessions[id]; sess.PRNumber != nil || sess.PRURL != nil {
			t.Fatalf("%s PR = %v / %v after refused listings, want unchanged (nil)", id, sess.PRNumber, sess.PRURL)
		}
	}
}
