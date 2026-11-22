package server

import (
	"context"
	"errors"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/adapter/verifybridge"
	"github.com/recurser/bossd/internal/db"
)

// pendingTrackerMergesFake is the queue; rows is read directly by tests that
// run the worker on their own goroutine, and through row by those that run it
// on another.
type pendingTrackerMergesFake struct {
	mu   sync.Mutex
	rows map[string]db.PendingTrackerMerge
}

func newPendingTrackerMergesFake() *pendingTrackerMergesFake {
	return &pendingTrackerMergesFake{rows: map[string]db.PendingTrackerMerge{}}
}

func (f *pendingTrackerMergesFake) row(sessionID string) (db.PendingTrackerMerge, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[sessionID]
	return p, ok
}

func (f *pendingTrackerMergesFake) Upsert(_ context.Context, p db.PendingTrackerMerge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[p.SessionID] = p
	return nil
}

func (f *pendingTrackerMergesFake) ListByDueAt(_ context.Context, now time.Time) ([]db.PendingTrackerMerge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.PendingTrackerMerge
	for _, p := range f.rows {
		if !p.NextAttemptAt.After(now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextAttemptAt.Before(out[j].NextAttemptAt) })
	return out, nil
}

func (f *pendingTrackerMergesFake) Delete(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, sessionID)
	return nil
}

// notesFake records the notes the daemon leaves; every other NoteStore method
// is unused here.
type notesFake struct {
	db.NoteStore
	created []db.CreateNoteParams
}

func (f *notesFake) Create(_ context.Context, p db.CreateNoteParams) (*domain.Note, error) {
	f.created = append(f.created, p)
	return &domain.Note{}, nil
}

// wantTicketNote fails unless notes holds exactly one note on repo r1 for
// session s1 saying body.
func wantTicketNote(t *testing.T, notes *notesFake, body string) {
	t.Helper()
	if len(notes.created) != 1 {
		t.Fatalf("notes = %+v, want one saying %q", notes.created, body)
	}
	n := notes.created[0]
	if n.RepoID != "r1" || n.Body != body || n.SessionID == nil || *n.SessionID != "s1" || n.IdempotencyKey == nil || *n.IdempotencyKey != "ticket-not-moved:s1" {
		t.Fatalf("note = %+v, want %q on r1 from s1, once per session", n, body)
	}
	if !slices.Contains(n.Tags, models.NoteTagTicketNotMoved) {
		t.Fatalf("note tags = %v, want %q so Home shows it", n.Tags, models.NoteTagTicketNotMoved)
	}
}

// The write is owed, as due now, before the reply and before the worker runs
// it, so a daemon that dies in between still runs it at its next start; the
// worker's run clears it.
func TestOperatorMergeSession_TheTrackerWriteIsOwedBeforeTheWorkerRunsIt(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := h.queue()

	before := time.Now()
	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	owed, wasOwed := queue.rows["s1"]
	if !wasOwed || owed.AttemptCount != 0 || owed.NextAttemptAt.Before(before) || owed.NextAttemptAt.After(time.Now()) || slices.Contains(h.calls, "mark-merged") {
		t.Fatalf("after the reply the queue held %+v (queued %v) and calls = %q, want s1 owed as due now with nothing run yet", owed, wasOwed, h.calls)
	}
	h.wantPendingThenBookkept(t, resp)
	if len(queue.rows) != 0 || !slices.Contains(h.calls, "mark-merged") {
		t.Fatalf("queue = %+v, calls = %q; want the owed entry cleared once the write landed", queue.rows, h.calls)
	}
}

func TestOperatorMergeSession_AWriteTheTrackerRefusesForGoodIsNotedNotRetried(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	notes := &notesFake{}
	h.srv.notes = notes
	h.verbs.markMerged = verifybridge.MarkMergedResult{
		Merged: true, TrackerWrites: "permanent", Why: "state-not-found: Done",
		Writes: []verifybridge.MarkMergedWrite{{Kind: "tracker", IssueID: "MAD-1764"}},
	}

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	h.wantPendingThenBookkept(t, resp)
	if len(h.queue().rows) != 0 {
		t.Fatalf("queue = %+v, want nothing queued for a write no retry can land", h.queue().rows)
	}
	wantTicketNote(t, notes, `PR #42 merged, but its ticket MAD-1764 could not be moved (state "Done" not found)`)
}

func TestOperatorMergeSession_AFailedTrackerWriteIsQueuedForARetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		markMerged verifybridge.MarkMergedResult
		markErr    error
	}{
		{name: "the PR read failed", markMerged: verifybridge.MarkMergedResult{Reason: "pr-unreadable"}},
		{name: "the write failed", markMerged: verifybridge.MarkMergedResult{Merged: true, TrackerWrites: "failed", Why: "issue-read-failed: socket hang up"}},
		{name: "the toolbox failed", markErr: errors.New("verify-gate exited non-zero")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			queue := h.queue()
			notes := &notesFake{}
			h.srv.notes = notes
			h.verbs.markMerged, h.verbs.markErr = tc.markMerged, tc.markErr

			resp, err := h.merge(false)
			if err != nil {
				t.Fatalf("OperatorMergeSession: %v", err)
			}
			before := time.Now()
			h.wantPendingThenBookkept(t, resp)
			p, ok := queue.rows["s1"]
			if !ok || p.AttemptCount != 1 || p.LastReason == "" || p.NextAttemptAt.Before(before.Add(trackerRetryDelays[0])) {
				t.Fatalf("queue = %+v, want s1 after one attempt, due no sooner than %v", queue.rows, trackerRetryDelays[0])
			}
			if p.NextAttemptAt.After(time.Now().Add(trackerRetryDelays[0])) {
				t.Fatalf("queue = %+v, want the retry due %v after the failure", queue.rows, trackerRetryDelays[0])
			}
			if len(notes.created) != 0 {
				t.Fatalf("notes = %+v, want none while a retry is due", notes.created)
			}
		})
	}
}

func TestOperatorMergeSession_ASettledTrackerOutcomeIsNotQueued(t *testing.T) {
	for _, r := range []verifybridge.MarkMergedResult{
		{Merged: true, TrackerWrites: "ok"},
		{Merged: true, TrackerWrites: "none", Why: "no-ticket"},
		{Merged: true, TrackerWrites: "unavailable", Why: "no-ticket-line"},
		{Merged: true, TrackerWrites: "unavailable", Why: "missing-credential"},
	} {
		h := newOperatorMergeHarness(t, headPin)
		notes := &notesFake{}
		h.srv.notes = notes
		h.verbs.markMerged = r

		resp, err := h.merge(false)
		if err != nil {
			t.Fatalf("OperatorMergeSession: %v", err)
		}
		h.wantPendingThenBookkept(t, resp)
		if len(h.queue().rows) != 0 || len(notes.created) != 0 {
			t.Fatalf("%+v: queue = %+v, notes = %+v; want a settled write to leave nothing queued and nothing noted", r, h.queue().rows, notes.created)
		}
	}
}

func TestRetryPendingTrackerMerges_RunsInTheCheckoutOnceTheWorktreeIsGone(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := newPendingTrackerMergesFake()
	h.srv.pendingTrackerMerges = queue
	sess := h.srv.sessions.(*lifecycleSessionStoreFake).session
	worktree := sess.WorktreePath
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	h.srv.repos.(*archiveRepoStoreFake).repo.LocalPath = checkout
	now := time.Now()
	queue.rows["s1"] = db.PendingTrackerMerge{SessionID: "s1", AttemptCount: 1, NextAttemptAt: now, LastReason: "pr-unreadable"}

	h.srv.retryPendingTrackerMerges(context.Background(), now)

	if len(h.verbs.targets) != 1 || h.verbs.targets[0].Dir != checkout || h.verbs.targets[0].PR != 42 {
		t.Fatalf("targets = %+v, want one mark-merged for PR 42 in the checkout %s", h.verbs.targets, checkout)
	}
	if len(queue.rows) != 0 {
		t.Fatalf("queue = %+v, want the settled entry cleared", queue.rows)
	}
}

func TestRetryPendingTrackerMerges_ReschedulesThenGivesUp(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := newPendingTrackerMergesFake()
	notes := &notesFake{}
	h.srv.pendingTrackerMerges, h.srv.notes = queue, notes
	h.verbs.markMerged = verifybridge.MarkMergedResult{Reason: "pr-unreadable"}
	now := time.Now()
	queue.rows["s1"] = db.PendingTrackerMerge{SessionID: "s1", AttemptCount: 1, NextAttemptAt: now}

	// Nothing is due before its time.
	h.srv.retryPendingTrackerMerges(context.Background(), now.Add(-time.Second))
	if len(h.verbs.targets) != 0 {
		t.Fatalf("an entry ran before it was due: %+v", h.verbs.targets)
	}

	for attempt := 2; attempt <= len(trackerRetryDelays)+1; attempt++ {
		h.srv.retryPendingTrackerMerges(context.Background(), now)
		p, queued := queue.rows["s1"]
		if attempt <= len(trackerRetryDelays) {
			if !queued || p.AttemptCount != attempt || !p.NextAttemptAt.Equal(now.Add(trackerRetryDelays[attempt-1])) || p.LastReason != "pr-unreadable" {
				t.Fatalf("after attempt %d: entry = %+v (queued %v), want attempt %d due %v later", attempt, p, queued, attempt, trackerRetryDelays[attempt-1])
			}
			if len(notes.created) != 0 {
				t.Fatalf("after attempt %d: notes = %+v, want none while retries remain", attempt, notes.created)
			}
			queue.rows["s1"] = db.PendingTrackerMerge{SessionID: "s1", AttemptCount: p.AttemptCount, NextAttemptAt: now, LastReason: p.LastReason}
			continue
		}
		if queued {
			t.Fatalf("after the last attempt the entry is still queued: %+v", p)
		}
	}
	if got := len(h.verbs.targets); got != len(trackerRetryDelays) {
		t.Fatalf("mark-merged ran %d times, want %d retries", got, len(trackerRetryDelays))
	}
	wantTicketNote(t, notes, "PR #42 merged, but the ticket update for it was given up (the PR could not be read)")
}

func TestRetryPendingTrackerMerges_AWriteTheTrackerRefusesForGoodIsNotedAndDropped(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := newPendingTrackerMergesFake()
	notes := &notesFake{}
	h.srv.pendingTrackerMerges, h.srv.notes = queue, notes
	h.verbs.markMerged = verifybridge.MarkMergedResult{
		Merged: true, TrackerWrites: "permanent", Why: "state-not-found: Done",
		Writes: []verifybridge.MarkMergedWrite{{Kind: "tracker", IssueID: "MAD-1764"}},
	}
	now := time.Now()
	queue.rows["s1"] = db.PendingTrackerMerge{SessionID: "s1", AttemptCount: 1, NextAttemptAt: now, LastReason: "pr-unreadable"}

	h.srv.retryPendingTrackerMerges(context.Background(), now)

	if len(queue.rows) != 0 {
		t.Fatalf("queue = %+v, want the entry dropped on the first refusal", queue.rows)
	}
	wantTicketNote(t, notes, `PR #42 merged, but its ticket MAD-1764 could not be moved (state "Done" not found)`)
}

func TestTicketNotMovedBody_SaysWhyAndClaimsNoTicketItDidNotSee(t *testing.T) {
	for _, tc := range []struct {
		o    trackerMergeOutcome
		want string
	}{
		{trackerMergeOutcome{writes: "permanent", why: "label-not-found: shipped", ticket: "MAD-1"}, `PR #7 merged, but its ticket MAD-1 could not be moved (label "shipped" not found)`},
		{trackerMergeOutcome{writes: "failed", why: "issue-read-failed: socket hang up", ticket: "MAD-1"}, "PR #7 merged, but its ticket MAD-1 could not be moved (the ticket could not be read)"},
		{trackerMergeOutcome{writes: "failed", why: "verify-gate exited non-zero"}, "PR #7 merged, but the ticket update for it was given up (verify-gate exited non-zero)"},
		{trackerMergeOutcome{writes: "failed", why: "state-rejected", ticket: "MAD-1"}, "PR #7 merged, but its ticket MAD-1 could not be moved (the tracker refused the state change)"},
		{trackerMergeOutcome{writes: "failed", why: "addLabel-rejected", ticket: "MAD-1"}, "PR #7 merged, but its ticket MAD-1 could not be moved (the tracker refused the label change)"},
		{trackerMergeOutcome{writes: "failed", why: "state: Entity not found: WorkflowState", ticket: "MAD-1"}, "PR #7 merged, but its ticket MAD-1 could not be moved (the state change failed)"},
		{trackerMergeOutcome{writes: "indeterminate", ticket: "MAD-1"}, "PR #7 merged, but its ticket MAD-1 could not be moved (whether the last attempt landed could not be confirmed)"},
	} {
		if got := ticketNotMovedBody(7, tc.o); got != tc.want {
			t.Errorf("ticketNotMovedBody(%+v) = %q, want %q", tc.o, got, tc.want)
		}
	}
}
