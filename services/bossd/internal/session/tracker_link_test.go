package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// fakeTrackerResolver resolves ids from a fixed issue table and records every
// call.
type fakeTrackerResolver struct {
	mu     sync.Mutex
	issues map[string]string // canonical id → url
	err    error             // returned for every call when set
	calls  []string
	// onResolve, when set, runs inside each call (simulates concurrent writes).
	onResolve func(id string)
	// blockFromCall, when > 0, makes that call (1-based) and every later one
	// block until its context ends (simulates a hung tracker).
	blockFromCall int
	// hangIDs makes a call for any of these ids block until its context ends
	// (simulates one candidate whose lookup hangs).
	hangIDs map[string]bool
	// hangErr, when set, is what a blocked call returns once its context
	// ends, instead of ctx.Err() (simulates a transport that reports the
	// deadline in its own error shape).
	hangErr error
}

func (f *fakeTrackerResolver) ResolveTrackerIssue(ctx context.Context, _ *models.Repo, id string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	if (f.blockFromCall > 0 && len(f.calls) >= f.blockFromCall) || f.hangIDs[id] {
		f.mu.Unlock()
		<-ctx.Done()
		if f.hangErr != nil {
			return "", "", f.hangErr
		}
		return "", "", ctx.Err()
	}
	defer f.mu.Unlock()
	if f.onResolve != nil {
		f.onResolve(id)
	}
	if f.err != nil {
		return "", "", f.err
	}
	url, ok := f.issues[id]
	if !ok {
		return "", "", ErrTrackerIssueNotFound
	}
	return id, url, nil
}

func (f *fakeTrackerResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type trackerLinkHarness struct {
	sessions *reconcileMockSessionStore
	repos    *mockRepoStore
	resolver *fakeTrackerResolver
	openPRs  map[string][]vcs.PRSummary // keyed by repo id
	notified []string
	now      time.Time
	linker   *TrackerLinker
}

func newTrackerLinkHarness(t *testing.T) *trackerLinkHarness {
	t.Helper()
	h := &trackerLinkHarness{
		sessions: newReconcileMockSessionStore(),
		repos:    newMockRepoStore(),
		resolver: &fakeTrackerResolver{issues: map[string]string{
			"BOS-1539": "https://linear.app/acme/issue/BOS-1539/add-approve",
		}},
		openPRs: make(map[string][]vcs.PRSummary),
		now:     time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
	}
	h.repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/acme/repo", LinearAPIKey: "lin_key"}
	h.linker = NewTrackerLinker(h.sessions, h.repos, h.resolver,
		func(_ context.Context, repoID, _ string) ([]vcs.PRSummary, error) {
			return h.openPRs[repoID], nil
		},
		func(_ context.Context, sess *models.Session) {
			h.notified = append(h.notified, sess.ID)
		},
		zerolog.Nop(),
	)
	h.linker.SetNowForTest(func() time.Time { return h.now })
	return h
}

func (h *trackerLinkHarness) addSession(id string, mutate func(*models.Session)) *models.Session {
	sess := &models.Session{ID: id, RepoID: "repo-1", State: machine.ImplementingPlan, Title: "Untitled", BranchName: "feature/" + id}
	if mutate != nil {
		mutate(sess)
	}
	h.sessions.addSession(sess)
	return sess
}

// assertLinked checks sess carries the link the fake resolver returns for
// BOS-1539, the id every linking test resolves.
func assertLinked(t *testing.T, sess *models.Session) {
	t.Helper()
	const wantID = "BOS-1539"
	if sess.TrackerID == nil || *sess.TrackerID != wantID {
		t.Fatalf("TrackerID = %v, want %q", sess.TrackerID, wantID)
	}
	wantURL := "https://linear.app/acme/issue/BOS-1539/add-approve"
	if sess.TrackerURL == nil || *sess.TrackerURL != wantURL {
		t.Fatalf("TrackerURL = %v, want %q", sess.TrackerURL, wantURL)
	}
}

func TestTrackerLinker_LinksFromEachSource(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.Session)
		prs    []vcs.PRSummary
	}{
		{
			name: "open PR title",
			mutate: func(s *models.Session) {
				n := 2995
				s.PRNumber = &n
			},
			prs: []vcs.PRSummary{{Number: 2995, Title: "[BOS-1539] Add boss pr approve and send-back", State: vcs.PRStateOpen}},
		},
		{
			name:   "branch name",
			mutate: func(s *models.Session) { s.BranchName = "dave/bos-1539-add-approve" },
		},
		{
			name:   "session title",
			mutate: func(s *models.Session) { s.Title = "[BOS-1539] Add boss pr approve" },
		},
		{
			name:   "existing tracker id without url",
			mutate: func(s *models.Session) { s.TrackerID = trackerStr("bos-1539") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTrackerLinkHarness(t)
			h.openPRs["repo-1"] = tt.prs
			sess := h.addSession("s1", tt.mutate)

			n, err := h.linker.LinkActive(context.Background())
			if err != nil {
				t.Fatalf("LinkActive: %v", err)
			}
			if n != 1 {
				t.Fatalf("linked = %d, want 1", n)
			}
			assertLinked(t, sess)
			if len(h.notified) != 1 || h.notified[0] != "s1" {
				t.Fatalf("notified = %v, want [s1]", h.notified)
			}
		})
	}
}

func TestTrackerLinker_ExistingTrackerIDIsNeverReplaced(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.addSession("s1", func(s *models.Session) {
		s.TrackerID = trackerStr("BOS-9")
		s.Title = "[BOS-1539] resolvable title"
	})

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0", n)
	}
	if got := h.resolver.calls; len(got) != 1 || got[0] != "BOS-9" {
		t.Fatalf("resolver calls = %v, want only the existing id [BOS-9]", got)
	}
	if len(h.sessions.trackerLinks["s1"]) != 0 {
		t.Fatalf("unexpected link writes: %v", h.sessions.trackerLinks["s1"])
	}
}

func TestTrackerLinker_ManualLinkDuringResolveIsNotOverwritten(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] resolvable title" })
	h.resolver.onResolve = func(string) {
		// A concurrent SetSessionTracker stores a corrected link as a new row.
		cur := *h.sessions.sessions["s1"]
		cur.TrackerID = trackerStr("BOS-7")
		cur.TrackerURL = trackerStr("https://linear.app/acme/issue/BOS-7/manual")
		h.sessions.sessions["s1"] = &cur
	}

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0", n)
	}
	got := h.sessions.sessions["s1"]
	if got.TrackerID == nil || *got.TrackerID != "BOS-7" {
		t.Fatalf("TrackerID = %v, want manual BOS-7 kept", got.TrackerID)
	}
	if len(h.sessions.trackerLinks["s1"]) != 0 {
		t.Fatalf("unexpected link writes: %v", h.sessions.trackerLinks["s1"])
	}
	if len(h.notified) != 0 {
		t.Fatalf("notified = %v, want none", h.notified)
	}
}

// TestTrackerLinker_QuickChatMarkedDuringResolveIsNotLinked marks the session
// a quick chat after the pass listed it, as CreateSession does in an Update
// right after the insert; the compare-and-set write must skip it.
func TestTrackerLinker_QuickChatMarkedDuringResolveIsNotLinked(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] resolvable title" })
	h.resolver.onResolve = func(string) {
		cur := *h.sessions.sessions["s1"]
		cur.IsQuickChat = true
		h.sessions.sessions["s1"] = &cur
	}

	n, err := h.linker.LinkActive(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("LinkActive = (%d, %v), want (0, nil)", n, err)
	}
	if got := h.sessions.sessions["s1"]; got.TrackerURL != nil || len(h.sessions.trackerLinks["s1"]) != 0 {
		t.Fatalf("quick chat was linked: url=%v writes=%v", got.TrackerURL, h.sessions.trackerLinks["s1"])
	}
	if len(h.notified) != 0 {
		t.Fatalf("notified = %v, want none", h.notified)
	}
}

// TestTrackerLinker_ManualLinkAfterLastReadIsNotOverwritten lands the manual
// SetSessionTracker after every read the linker makes — just before its store
// write — so only a compare-and-set write can keep it.
func TestTrackerLinker_ManualLinkAfterLastReadIsNotOverwritten(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] resolvable title" })
	h.sessions.beforeWrite = func(id string) {
		cur := *h.sessions.sessions[id]
		cur.TrackerID = trackerStr("BOS-7")
		cur.TrackerURL = trackerStr("https://linear.app/acme/issue/BOS-7/manual")
		h.sessions.sessions[id] = &cur
	}

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0", n)
	}
	got := h.sessions.sessions["s1"]
	if got.TrackerID == nil || *got.TrackerID != "BOS-7" ||
		got.TrackerURL == nil || *got.TrackerURL != "https://linear.app/acme/issue/BOS-7/manual" {
		t.Fatalf("tracker = (%v, %v), want manual BOS-7 kept", got.TrackerID, got.TrackerURL)
	}
	if len(h.notified) != 0 {
		t.Fatalf("notified = %v, want none", h.notified)
	}
}

func TestTrackerLinker_UnresolvedCandidateGetsNoLink(t *testing.T) {
	h := newTrackerLinkHarness(t)
	sess := h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-404] Unknown ticket" })

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0", n)
	}
	if h.resolver.callCount() != 1 {
		t.Fatalf("resolver calls = %d, want 1", h.resolver.callCount())
	}
	if len(h.sessions.trackerLinks["s1"]) != 0 || sess.TrackerURL != nil {
		t.Fatalf("expected no link write, got %v", h.sessions.trackerLinks["s1"])
	}
	if len(h.notified) != 0 {
		t.Fatalf("notified = %v, want none", h.notified)
	}
}

func TestTrackerLinker_RepoWithoutLinearKeyMakesNoResolverCall(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.repos.repos["repo-1"].LinearAPIKey = ""
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] x" })

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 || h.resolver.callCount() != 0 {
		t.Fatalf("linked = %d, resolver calls = %d; want 0, 0", n, h.resolver.callCount())
	}
	if len(h.sessions.trackerLinks["s1"]) != 0 {
		t.Fatalf("expected no link write")
	}
}

func TestTrackerLinker_NotConfiguredStopsRepoForTick(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.resolver.err = fmt.Errorf("%w: no linear plugin", ErrTrackerNotConfigured)
	for i := range 3 {
		h.addSession(fmt.Sprintf("s%d", i), func(s *models.Session) { s.Title = "[BOS-1539] x" })
	}

	if _, err := h.linker.LinkActive(context.Background()); err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if got := h.resolver.callCount(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1 (the repo is skipped after not-configured)", got)
	}
}

func TestTrackerLinker_MissWindowAndBudget(t *testing.T) {
	h := newTrackerLinkHarness(t)
	for i := range 8 {
		h.addSession(fmt.Sprintf("s%d", i), func(s *models.Session) { s.Title = fmt.Sprintf("[BOS-%d] unknown", 100+i) })
	}

	ctx := context.Background()
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := h.resolver.callCount(); got != maxTrackerResolvesPerPass {
		t.Fatalf("pass 1 resolver calls = %d, want %d", got, maxTrackerResolvesPerPass)
	}

	// Pass 2 inside the window: only the three sessions not yet tried.
	h.now = h.now.Add(time.Minute)
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := h.resolver.callCount(); got != 8 {
		t.Fatalf("after pass 2 resolver calls = %d, want 8", got)
	}

	// Pass 3 still inside every window: nothing is retried.
	h.now = h.now.Add(10 * time.Minute)
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if got := h.resolver.callCount(); got != 8 {
		t.Fatalf("after pass 3 resolver calls = %d, want 8 (all in miss window)", got)
	}

	// Pass 4 after the first window expires: retried, still within the budget.
	h.now = h.now.Add(5 * time.Minute)
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	if got := h.resolver.callCount(); got != 8+maxTrackerResolvesPerPass {
		t.Fatalf("after pass 4 resolver calls = %d, want %d", got, 8+maxTrackerResolvesPerPass)
	}
}

// TestTrackerLinker_LaterCandidateResolvesAfterAMiss walks one session's
// candidates in priority order: the PR-title id misses, the branch id
// resolves. The first pass's write fails, so the second pass shows the miss
// was recorded (the PR-title id is skipped inside its window) while the
// resolved candidate, which is not a miss, is retried and linked.
func TestTrackerLinker_LaterCandidateResolvesAfterAMiss(t *testing.T) {
	h := newTrackerLinkHarness(t)
	sess := h.addSession("s1", func(s *models.Session) {
		n := 12
		s.PRNumber = &n
		s.BranchName = "dave/bos-1539-add-approve"
	})
	h.openPRs["repo-1"] = []vcs.PRSummary{{Number: 12, Title: "[BOS-404] stale ticket"}}
	h.sessions.updateErr["s1"] = errors.New("database is locked")

	ctx := context.Background()
	if n, err := h.linker.LinkActive(ctx); err != nil || n != 0 {
		t.Fatalf("pass 1 = (%d, %v), want (0, nil)", n, err)
	}
	if got := h.resolver.calls; len(got) != 2 || got[0] != "BOS-404" || got[1] != "BOS-1539" {
		t.Fatalf("pass 1 resolver calls = %v, want [BOS-404 BOS-1539]", got)
	}

	delete(h.sessions.updateErr, "s1")
	h.now = h.now.Add(time.Minute)
	if n, err := h.linker.LinkActive(ctx); err != nil || n != 1 {
		t.Fatalf("pass 2 = (%d, %v), want (1, nil)", n, err)
	}
	if got := h.resolver.calls[2:]; len(got) != 1 || got[0] != "BOS-1539" {
		t.Fatalf("pass 2 resolver calls = %v, want only [BOS-1539] (BOS-404 is in its miss window)", got)
	}
	assertLinked(t, sess)
	if got := h.sessions.trackerLinks["s1"]; len(got) != 1 || got[0] != "BOS-1539" {
		t.Fatalf("link writes = %v, want [BOS-1539]", got)
	}
}

// TestTrackerLinker_BudgetStopsMidSession gives two sessions three missing
// candidates each, one more than the pass budget: whichever session runs
// second stops after its first two candidates, and the next pass tries its
// third.
func TestTrackerLinker_BudgetStopsMidSession(t *testing.T) {
	h := newTrackerLinkHarness(t)
	candidates := map[string][]string{}
	for i, id := range []string{"a", "b"} {
		base := 100 * (i + 1)
		pr := base
		h.addSession(id, func(s *models.Session) {
			s.PRNumber = &pr
			s.BranchName = fmt.Sprintf("bos-%d-x", base+2)
			s.Title = fmt.Sprintf("[BOS-%d] t", base+3)
		})
		h.openPRs["repo-1"] = append(h.openPRs["repo-1"], vcs.PRSummary{Number: pr, Title: fmt.Sprintf("[BOS-%d] p", base+1)})
		candidates[id] = []string{fmt.Sprintf("BOS-%d", base+1), fmt.Sprintf("BOS-%d", base+2), fmt.Sprintf("BOS-%d", base+3)}
	}
	if 2*len(candidates["a"]) <= maxTrackerResolvesPerPass {
		t.Fatalf("fixture needs more candidates (%d) than the budget (%d)", 2*len(candidates["a"]), maxTrackerResolvesPerPass)
	}

	ctx := context.Background()
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	calls := append([]string(nil), h.resolver.calls...)
	if len(calls) != maxTrackerResolvesPerPass {
		t.Fatalf("pass 1 resolver calls = %v, want %d", calls, maxTrackerResolvesPerPass)
	}
	first, second := "a", "b"
	if calls[0] != candidates["a"][0] {
		first, second = "b", "a"
	}
	want := append(append([]string(nil), candidates[first]...), candidates[second][:2]...)
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("pass 1 resolver calls = %v, want %v", calls, want)
		}
	}

	h.now = h.now.Add(time.Minute)
	if _, err := h.linker.LinkActive(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := h.resolver.calls[len(calls):]; len(got) != 1 || got[0] != candidates[second][2] {
		t.Fatalf("pass 2 resolver calls = %v, want only [%s]", got, candidates[second][2])
	}
}

func TestTrackerLinker_PluginErrorRecordsMiss(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.resolver.err = errors.New("plugin down")
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] x" })

	ctx := context.Background()
	for range 2 {
		if _, err := h.linker.LinkActive(ctx); err != nil {
			t.Fatalf("LinkActive: %v", err)
		}
	}
	if got := h.resolver.callCount(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1 (the error is a miss)", got)
	}
}

func TestTrackerLinker_BackfillsSeededUnlinkedSessions(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.resolver.issues["BOS-1540"] = "https://linear.app/acme/issue/BOS-1540/x"
	a := h.addSession("a", func(s *models.Session) { s.BranchName = "bos-1539-a" })
	b := h.addSession("b", func(s *models.Session) { s.Title = "[BOS-1540] b" })

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 2 {
		t.Fatalf("linked = %d, want 2", n)
	}
	assertLinked(t, a)
	if b.TrackerID == nil || *b.TrackerID != "BOS-1540" || b.TrackerURL == nil || *b.TrackerURL != "https://linear.app/acme/issue/BOS-1540/x" {
		t.Fatalf("b not linked: id=%v url=%v", b.TrackerID, b.TrackerURL)
	}
	if len(h.notified) != 2 {
		t.Fatalf("notified = %v, want one per linked session", h.notified)
	}

	// A second pass finds nothing left to link and notifies nobody again.
	n, err = h.linker.LinkActive(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second pass = (%d, %v), want (0, nil)", n, err)
	}
	if len(h.notified) != 2 {
		t.Fatalf("notified after second pass = %v", h.notified)
	}
}

func TestTrackerLinker_SkipsIneligibleSessions(t *testing.T) {
	h := newTrackerLinkHarness(t)
	archivedAt := h.now
	h.addSession("quick", func(s *models.Session) {
		s.IsQuickChat = true
		s.Title = "[BOS-1539] q"
	})
	h.addSession("plan", func(s *models.Session) {
		s.Plan = "/boss-plan BOS-1539"
		s.Title = "[BOS-1539] p"
	})
	h.addSession("linked", func(s *models.Session) {
		s.TrackerID = trackerStr("BOS-1")
		s.TrackerURL = trackerStr("https://linear.app/acme/issue/BOS-1")
		s.Title = "[BOS-1539] l"
	})
	h.addSession("archived", func(s *models.Session) {
		s.ArchivedAt = &archivedAt
		s.Title = "[BOS-1539] a"
	})

	n, err := h.linker.LinkActive(context.Background())
	if err != nil {
		t.Fatalf("LinkActive: %v", err)
	}
	if n != 0 || h.resolver.callCount() != 0 {
		t.Fatalf("linked = %d, resolver calls = %d; want 0, 0", n, h.resolver.callCount())
	}
	for id, links := range h.sessions.trackerLinks {
		if len(links) != 0 {
			t.Fatalf("session %s was linked: %v", id, links)
		}
	}
}

func TestTrackerLinker_ClosedPRTitleIsIgnored(t *testing.T) {
	h := newTrackerLinkHarness(t)
	// The PR is not in the open listing, so its title is not a source.
	h.addSession("s1", func(s *models.Session) {
		n := 77
		s.PRNumber = &n
	})
	h.openPRs["repo-1"] = []vcs.PRSummary{{Number: 78, Title: "[BOS-1539] other PR"}}

	n, err := h.linker.LinkActive(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("LinkActive = (%d, %v), want (0, nil)", n, err)
	}
	if h.resolver.callCount() != 0 {
		t.Fatalf("resolver calls = %d, want 0", h.resolver.callCount())
	}
}

func TestTrackerLinker_ContextErrorEndsPass(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] x" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := h.linker.LinkActive(ctx)
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("LinkActive = (%d, %v), want (0, context.Canceled)", n, err)
	}
	if h.resolver.callCount() != 0 {
		t.Fatalf("resolver calls = %d, want 0", h.resolver.callCount())
	}
}

func TestTrackerLinker_PassStopsAtItsOwnTimeout(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.resolver.issues["BOS-1540"] = "https://linear.app/acme/issue/BOS-1540/x"
	h.resolver.blockFromCall = 2
	h.addSession("a", func(s *models.Session) { s.Title = "[BOS-1539] a" })
	h.addSession("b", func(s *models.Session) { s.Title = "[BOS-1540] b" })
	h.linker.SetPassTimeoutForTest(50 * time.Millisecond)

	// The caller's context outlives the pass; it is cancelled only so a
	// broken bound cannot leak the blocked goroutine past the test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := h.linker.LinkActive(ctx)
		done <- result{n, err}
	}()

	select {
	case r := <-done:
		if r.n != 1 {
			t.Fatalf("linked = %d, want the partial count 1", r.n)
		}
		if !errors.Is(r.err, ErrTrackerLinkPassTimedOut) || !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrTrackerLinkPassTimedOut wrapping context.DeadlineExceeded", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LinkActive did not return within its pass timeout")
	}
	if got := h.resolver.callCount(); got != 2 {
		t.Fatalf("resolver calls = %d, want 2", got)
	}
	// The interrupted candidate is not a miss: the next pass retries it.
	h.resolver.blockFromCall = 0
	if n, err := h.linker.LinkActive(context.Background()); err != nil || n != 1 {
		t.Fatalf("next pass = (%d, %v), want (1, nil)", n, err)
	}
}

// TestTrackerLinker_LinkCommittedAsThePassEndsStillNotifies ends the pass's
// context right after the link write commits. The re-read and the update
// event are post-commit work, so they still run, on a live context.
func TestTrackerLinker_LinkCommittedAsThePassEndsStillNotifies(t *testing.T) {
	h := newTrackerLinkHarness(t)
	sess := h.addSession("s1", func(s *models.Session) { s.Title = "[BOS-1539] resolvable title" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.sessions.getHonorsCtx = true
	h.sessions.afterLink = func(string) { cancel() }
	var notifyCtxErr error
	h.linker.notify = func(nctx context.Context, s *models.Session) {
		notifyCtxErr = nctx.Err()
		h.notified = append(h.notified, s.ID)
	}

	if n, err := h.linker.LinkActive(ctx); n != 1 {
		t.Fatalf("LinkActive = (%d, %v), want 1 linked", n, err)
	}
	assertLinked(t, sess)
	if len(h.notified) != 1 || h.notified[0] != "s1" {
		t.Fatalf("notified = %v, want [s1]: a committed link must publish its update", h.notified)
	}
	if notifyCtxErr != nil {
		t.Fatalf("notify ran on a done context (%v); post-commit work must be detached", notifyCtxErr)
	}
}

// TestTrackerLinker_HungResolveIsAMissAndThePassMovesOn hangs one session's
// only candidate. The resolve ends at its own per-call bound while the pass is
// still live, so it is recorded as a miss, the other session is linked in the
// same pass, and the next pass inside the miss window does not retry it.
func TestTrackerLinker_HungResolveIsAMissAndThePassMovesOn(t *testing.T) {
	h := newTrackerLinkHarness(t)
	h.resolver.hangIDs = map[string]bool{"BOS-1541": true}
	h.addSession("hung", func(s *models.Session) { s.Title = "[BOS-1541] hangs" })
	linked := h.addSession("ok", func(s *models.Session) { s.Title = "[BOS-1539] resolves" })
	h.linker.SetResolveTimeoutForTest(20 * time.Millisecond)
	h.linker.SetPassTimeoutForTest(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := h.linker.LinkActive(ctx)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.n != 1 {
			t.Fatalf("pass 1 = (%d, %v), want (1, nil): the hung resolve must end at its own bound", r.n, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LinkActive did not return")
	}
	assertLinked(t, linked)
	if got := h.resolver.callCount(); got != 2 {
		t.Fatalf("pass 1 resolver calls = %d, want 2", got)
	}

	h.now = h.now.Add(time.Minute)
	if n, err := h.linker.LinkActive(ctx); err != nil || n != 0 {
		t.Fatalf("pass 2 = (%d, %v), want (0, nil)", n, err)
	}
	if got := h.resolver.callCount(); got != 2 {
		t.Fatalf("after pass 2 resolver calls = %d, want 2 (the hung candidate is in its miss window)", got)
	}
}

// TestTrackerLinker_ResolveTimeoutWarnsWhateverTheTransportErrorShape hangs a
// resolve past its per-call bound and returns the error the gRPC plugin path
// produces for it: a status error that does not unwrap to
// context.DeadlineExceeded. The linker classifies the timeout by its own
// per-call context, so the Warn still fires and the miss is still recorded.
func TestTrackerLinker_ResolveTimeoutWarnsWhateverTheTransportErrorShape(t *testing.T) {
	h := newTrackerLinkHarness(t)
	rpcErr := fmt.Errorf("look up tracker issue BOS-1541: %w",
		status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error()))
	if errors.Is(rpcErr, context.DeadlineExceeded) {
		t.Fatal("fixture: the gRPC status error must not unwrap to context.DeadlineExceeded")
	}
	h.resolver.hangIDs = map[string]bool{"BOS-1541": true}
	h.resolver.hangErr = rpcErr
	h.addSession("hung", func(s *models.Session) { s.Title = "[BOS-1541] hangs" })
	linked := h.addSession("ok", func(s *models.Session) { s.Title = "[BOS-1539] resolves" })
	var logs bytes.Buffer
	h.linker.logger = zerolog.New(&logs)
	h.linker.SetResolveTimeoutForTest(20 * time.Millisecond)
	h.linker.SetPassTimeoutForTest(2 * time.Second)

	if n, err := h.linker.LinkActive(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass 1 = (%d, %v), want (1, nil)", n, err)
	}
	assertLinked(t, linked)

	var warned bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry struct {
			Level     string `json:"level"`
			Message   string `json:"message"`
			Candidate string `json:"candidate"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry.Level == "warn" && entry.Candidate == "BOS-1541" && strings.Contains(entry.Message, "timed out") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no timed-out Warn for BOS-1541; logs:\n%s", logs.String())
	}

	h.now = h.now.Add(time.Minute)
	if n, err := h.linker.LinkActive(context.Background()); err != nil || n != 0 {
		t.Fatalf("pass 2 = (%d, %v), want (0, nil)", n, err)
	}
	if got := h.resolver.callCount(); got != 2 {
		t.Fatalf("resolver calls = %d, want 2 (the timed-out candidate is in its miss window)", got)
	}
}

// TestTrackerLinker_EveryResolveFitsInsideOnePass pins the budget relation the
// per-call bound depends on: a pass whose every call hangs still records each
// one as a miss before the pass deadline.
func TestTrackerLinker_EveryResolveFitsInsideOnePass(t *testing.T) {
	if total := maxTrackerResolvesPerPass * trackerResolveTimeout; total >= trackerLinkPassTimeout {
		t.Fatalf("%d resolves x %s = %s, want under the %s pass budget",
			maxTrackerResolvesPerPass, trackerResolveTimeout, total, trackerLinkPassTimeout)
	}
}

// TestTrackerLinker_OnlyItsOwnBudgetIsAPassTimeout keeps
// ErrTrackerLinkPassTimedOut for the pass's own budget: a caller deadline or
// a store failure is reported as itself.
func TestTrackerLinker_OnlyItsOwnBudgetIsAPassTimeout(t *testing.T) {
	t.Run("caller deadline", func(t *testing.T) {
		h := newTrackerLinkHarness(t)
		h.resolver.blockFromCall = 1
		h.addSession("a", func(s *models.Session) { s.Title = "[BOS-1539] a" })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := h.linker.LinkActive(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTrackerLinkPassTimedOut) {
			t.Fatalf("err = %v, want the caller's context.DeadlineExceeded, not ErrTrackerLinkPassTimedOut", err)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		h := newTrackerLinkHarness(t)
		h.sessions.listActiveErr = errors.New("db locked")

		_, err := h.linker.LinkActive(context.Background())
		if err == nil || errors.Is(err, ErrTrackerLinkPassTimedOut) {
			t.Fatalf("err = %v, want the store error, not ErrTrackerLinkPassTimedOut", err)
		}
	})
	// A dependency's own shorter deadline wraps context.DeadlineExceeded while
	// the pass still has budget left; only the pass's own expiry is a pass
	// timeout.
	t.Run("dependency deadline", func(t *testing.T) {
		h := newTrackerLinkHarness(t)
		h.sessions.listActiveErr = fmt.Errorf("query sessions: %w", context.DeadlineExceeded)

		_, err := h.linker.LinkActive(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTrackerLinkPassTimedOut) {
			t.Fatalf("err = %v, want the store's deadline error, not ErrTrackerLinkPassTimedOut", err)
		}
	})
}

func trackerStr(s string) *string { return &s }

// trackerLinkCASHolds is the SetTrackerIfUnlinked predicate the session-store
// fakes share: active, not a quick chat, no tracker URL, tracker id still the
// expected one.
func trackerLinkCASHolds(s *models.Session, expectedTrackerID string) bool {
	return s.ArchivedAt == nil && !s.IsQuickChat && (s.TrackerURL == nil || *s.TrackerURL == "") && trackerIDOf(s) == expectedTrackerID
}
