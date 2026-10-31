package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// stubRepairLease is a RepairActivity whose live leases are a fixed set.
type stubRepairLease map[string]bool

func (s stubRepairLease) Active(sessionID string) bool { return s[sessionID] }

func TestRepairLapsed(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time { ts := now.Add(-ago); return &ts }
	for _, tc := range []struct {
		name     string
		state    machine.State
		entered  *time.Time
		activity RepairActivity
		want     bool
	}{
		{"not fixing_checks", machine.AwaitingChecks, at(time.Hour), stubRepairLease{}, false},
		{"lease live", machine.FixingChecks, at(time.Hour), stubRepairLease{"sess-1": true}, false},
		{"inside grace", machine.FixingChecks, at(RepairLapseGrace - time.Second), stubRepairLease{}, false},
		{"exactly at grace", machine.FixingChecks, at(RepairLapseGrace), stubRepairLease{}, true},
		{"past grace", machine.FixingChecks, at(time.Hour), stubRepairLease{}, true},
		{"unknown entry time", machine.FixingChecks, nil, stubRepairLease{}, true},
		{"no activity wired", machine.FixingChecks, at(time.Hour), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &models.Session{ID: "sess-1", State: tc.state, StateEnteredAt: tc.entered}
			if got := repairLapsed(sess, tc.activity, now); got != tc.want {
				t.Fatalf("repairLapsed = %v, want %v", got, tc.want)
			}
		})
	}
}

// lapseFixture is one FixingChecks session on PR 42 plus a poller and a
// dispatcher whose clocks sit past the repair grace.
type lapseFixture struct {
	sessions *mockSessionStore
	vcs      *mockVCSProvider
	repo     *models.Repo
	poller   *Poller
	disp     *Dispatcher
	logs     *syncBuf
}

func newLapseFixture(t *testing.T, state machine.State, lease stubRepairLease) *lapseFixture {
	t.Helper()
	f := &lapseFixture{
		sessions: newMockSessionStore(),
		vcs:      newMockVCSProvider(),
		repo:     &models.Repo{ID: "repo-1", OriginURL: "owner/repo"},
		logs:     &syncBuf{},
	}
	repos := newMockRepoStore()
	repos.repos["repo-1"] = f.repo
	pr := 42
	entered := time.Now()
	f.sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", State: state, PRNumber: &pr, StateEnteredAt: &entered,
	}
	logger := zerolog.New(f.logs)
	f.poller = NewPoller(f.sessions, repos, f.vcs, time.Hour, DefaultPollTimeout, logger)
	f.disp = NewDispatcher(f.sessions, repos, f.vcs, logger)
	f.poller.SetRepairActivity(lease)
	f.disp.SetRepairActivity(lease)
	f.advancePastGrace()
	return f
}

// advancePastGrace moves both clocks past the grace of a state entered now.
// The mock store stamps StateEnteredAt with time.Now on every state change.
func (f *lapseFixture) advancePastGrace() {
	later := time.Now().Add(RepairLapseGrace + time.Minute)
	f.poller.now = func() time.Time { return later }
	f.disp.now = func() time.Time { return later }
}

func (f *lapseFixture) session() *models.Session { return f.sessions.sessions["sess-1"] }

// lapseHead is the PR head every fixture result reports, so a repeated
// conflict is a re-observation of the same commit.
const lapseHead = "sha-a"

func (f *lapseFixture) conflicted() {
	mergeable := false
	f.vcs.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, Mergeable: &mergeable, HeadSHA: lapseHead}
}

func (f *lapseFixture) checks(conclusion vcs.CheckConclusion) {
	mergeable := true
	f.vcs.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, Mergeable: &mergeable, HeadSHA: lapseHead}
	f.vcs.nextCheckResults = []vcs.CheckResult{{ID: "c1", Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &conclusion}}
}

// poll runs one checkSession and returns what it emitted.
func (f *lapseFixture) poll(t *testing.T) []Event {
	t.Helper()
	ch := make(chan Event, 8)
	f.poller.checkSession(context.Background(), ch, f.repo, f.session())
	close(ch)
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// sweep is one poll followed by dispatching everything it emitted.
func (f *lapseFixture) sweep(t *testing.T) {
	t.Helper()
	for _, ev := range f.poll(t) {
		if err := f.disp.dispatch(context.Background(), ev); err != nil {
			t.Fatalf("dispatch %T: %v", ev.Event, err)
		}
	}
}

func TestPollerReleasesLapsedFixingChecks_Sessions_AC216201(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		f := newLapseFixture(t, machine.FixingChecks, stubRepairLease{})
		f.conflicted()
		evs := f.poll(t)
		if len(evs) != 1 {
			t.Fatalf("emitted %d events, want 1 ConflictDetected", len(evs))
		}
		if _, ok := evs[0].Event.(vcs.ConflictDetected); !ok {
			t.Fatalf("emitted %T, want ConflictDetected", evs[0].Event)
		}
		if n := strings.Count(f.logs.String(), "releasing fixing_checks"); n != 1 {
			t.Fatalf("release log lines = %d, want 1; logs:\n%s", n, f.logs.String())
		}
		if !strings.Contains(f.logs.String(), `"pr":42`) || !strings.Contains(f.logs.String(), `"session":"sess-1"`) {
			t.Fatalf("release log line must name the session and PR; logs:\n%s", f.logs.String())
		}
	})
	t.Run("green", func(t *testing.T) {
		f := newLapseFixture(t, machine.FixingChecks, stubRepairLease{})
		f.checks(vcs.CheckConclusionSuccess)
		evs := f.poll(t)
		if len(evs) != 1 {
			t.Fatalf("emitted %d events, want 1 ChecksPassed", len(evs))
		}
		if _, ok := evs[0].Event.(vcs.ChecksPassed); !ok {
			t.Fatalf("emitted %T, want ChecksPassed", evs[0].Event)
		}
	})
}

func TestPollerHoldsFixingChecksWhileRepairLive_Sessions_AC216202(t *testing.T) {
	for name, setup := range map[string]func(*lapseFixture){
		"conflict": func(f *lapseFixture) { f.conflicted() },
		"red":      func(f *lapseFixture) { f.checks(vcs.CheckConclusionFailure) },
		"green":    func(f *lapseFixture) { f.checks(vcs.CheckConclusionSuccess) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newLapseFixture(t, machine.FixingChecks, stubRepairLease{"sess-1": true})
			setup(f)
			if evs := f.poll(t); len(evs) != 0 {
				t.Fatalf("emitted %T with a live repair lease, want nothing", evs[0].Event)
			}
		})
	}
}

func TestDispatcherReleasesLapsedFixingChecks_Sessions_AC216201(t *testing.T) {
	for name, ev := range map[string]vcs.Event{
		"conflict": vcs.ConflictDetected{PRID: 42, HeadSHA: "sha-b"},
		"red":      vcs.ChecksFailed{PRID: 42, HeadSHA: "sha-b"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLapseFixture(t, machine.FixingChecks, stubRepairLease{})
			prior := lapseHead
			f.session().AttemptCount, f.session().LastAttemptHeadSHA = 2, &prior
			// Dispatched directly, as the unguarded webhook path delivers it.
			if err := f.disp.dispatch(context.Background(), Event{SessionID: "sess-1", Event: ev}); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			sess := f.session()
			if sess.State != machine.AwaitingChecks {
				t.Fatalf("state = %v, want AwaitingChecks", sess.State)
			}
			if sess.AttemptCount != 2 {
				t.Fatalf("AttemptCount = %d, want 2 (a release consumes no attempt)", sess.AttemptCount)
			}
			if sess.LastAttemptHeadSHA == nil || *sess.LastAttemptHeadSHA != lapseHead {
				t.Fatalf("LastAttemptHeadSHA = %v, want unchanged sha-a", sess.LastAttemptHeadSHA)
			}
			logs := f.logs.String()
			if n := strings.Count(logs, "released fixing_checks: no repair lease"); n != 1 || !strings.Contains(logs, `"pr":42`) {
				t.Fatalf("want one release line naming PR 42; logs:\n%s", logs)
			}
		})
	}
}

func TestDispatcherRejectsWhileRepairLive_Sessions_AC216202(t *testing.T) {
	for name, ev := range map[string]vcs.Event{
		"conflict": vcs.ConflictDetected{PRID: 42, HeadSHA: "sha-b"},
		"red":      vcs.ChecksFailed{PRID: 42, HeadSHA: "sha-b"},
		"green":    vcs.ChecksPassed{PRID: 42, HeadSHA: "sha-b"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLapseFixture(t, machine.FixingChecks, stubRepairLease{"sess-1": true})
			f.session().AttemptCount = 2
			if err := f.disp.dispatch(context.Background(), Event{SessionID: "sess-1", Event: ev}); err == nil {
				t.Fatal("dispatch with a live repair lease: want a rejected transition, got nil")
			}
			if sess := f.session(); sess.State != machine.FixingChecks || sess.AttemptCount != 2 {
				t.Fatalf("state = %v attempts = %d, want FixingChecks with 2", sess.State, sess.AttemptCount)
			}
		})
	}
}

// TestDeclinedRepairReleasesThenReenters is the #3795 regression: a conflict
// sends the session to FixingChecks, the repair declines and takes no lease,
// and one sweep past the grace releases it. From there a green PR reaches
// GreenDraft, and a conflict still at the same head re-enters FixingChecks as a
// free lap (BOS-235).
func TestDeclinedRepairReleasesThenReenters_Sessions_AC216201(t *testing.T) {
	start := func(t *testing.T) *lapseFixture {
		t.Helper()
		f := newLapseFixture(t, machine.AwaitingChecks, stubRepairLease{})
		f.conflicted()
		f.sweep(t)
		sess := f.session()
		if sess.State != machine.FixingChecks || sess.AttemptCount != 1 || sess.LastAttemptHeadSHA == nil || *sess.LastAttemptHeadSHA != lapseHead {
			t.Fatalf("after first conflict: state=%v attempts=%d head=%v, want FixingChecks/1/sha-a",
				sess.State, sess.AttemptCount, sess.LastAttemptHeadSHA)
		}
		f.advancePastGrace()
		f.sweep(t)
		if sess := f.session(); sess.State != machine.AwaitingChecks || sess.AttemptCount != 1 {
			t.Fatalf("after lapse sweep: state=%v attempts=%d, want AwaitingChecks/1", sess.State, sess.AttemptCount)
		}
		return f
	}
	t.Run("turns green", func(t *testing.T) {
		f := start(t)
		f.checks(vcs.CheckConclusionSuccess)
		f.sweep(t)
		if sess := f.session(); sess.State != machine.GreenDraft {
			t.Fatalf("state = %v, want GreenDraft", sess.State)
		}
	})
	t.Run("still conflicted at the same head", func(t *testing.T) {
		f := start(t)
		f.sweep(t)
		if sess := f.session(); sess.State != machine.FixingChecks || sess.AttemptCount != 1 {
			t.Fatalf("state=%v attempts=%d, want FixingChecks with attempts still 1 (free lap)", sess.State, sess.AttemptCount)
		}
	})
}
