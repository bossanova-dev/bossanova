package server

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/verifybridge"
)

type pendingTrackerMergesFake struct {
	rows map[string]db.PendingTrackerMerge
}

func newPendingTrackerMergesFake() *pendingTrackerMergesFake {
	return &pendingTrackerMergesFake{rows: map[string]db.PendingTrackerMerge{}}
}

func (f *pendingTrackerMergesFake) Schedule(_ context.Context, p db.PendingTrackerMerge) error {
	f.rows[p.SessionID] = p
	return nil
}

func (f *pendingTrackerMergesFake) ListDue(_ context.Context, now time.Time) ([]db.PendingTrackerMerge, error) {
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
	delete(f.rows, sessionID)
	return nil
}

// notesFake records the notes the daemon leaves; every other NoteStore method
// is unused here.
type notesFake struct {
	db.NoteStore
	created []db.CreateNoteParams
}

func (f *notesFake) Create(_ context.Context, p db.CreateNoteParams) (*models.Note, error) {
	f.created = append(f.created, p)
	return &models.Note{}, nil
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
}

func TestOperatorMergeSession_TheTrackerWriteIsOwedBeforeItRuns(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := newPendingTrackerMergesFake()
	h.srv.pendingTrackerMerges = queue
	var owed db.PendingTrackerMerge
	var wasOwed bool
	h.verbs.onMarkMerged = func() { owed, wasOwed = queue.rows["s1"] }

	before := time.Now()
	if _, err := h.merge(false); err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !wasOwed || owed.AttemptCount != 1 || owed.NextAttemptAt.Before(before.Add(defaultOperatorMergeBudget.total)) {
		t.Fatalf("while mark-merged ran the queue held %+v (queued %v), want s1 due only after the merge's budget", owed, wasOwed)
	}
	if len(queue.rows) != 0 {
		t.Fatalf("queue = %+v, want the owed entry cleared once the write landed", queue.rows)
	}
}

func TestOperatorMergeSession_AWriteTheTrackerRefusesForGoodIsNotedNotRetried(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	queue := newPendingTrackerMergesFake()
	notes := &notesFake{}
	h.srv.pendingTrackerMerges, h.srv.notes = queue, notes
	h.verbs.markMerged = verifybridge.MarkMergedResult{
		Merged: true, TrackerWrites: "permanent", Why: "state-not-found: Done",
		Writes: []verifybridge.MarkMergedWrite{{Kind: "tracker", IssueID: "MAD-1764"}},
	}

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !resp.GetIsMerged() || resp.GetTrackerWrites() != "permanent" || resp.GetTrackerTicket() != "MAD-1764" {
		t.Fatalf("response = %+v, want a landed merge whose ticket MAD-1764 will not move", resp)
	}
	if len(queue.rows) != 0 {
		t.Fatalf("queue = %+v, want nothing queued for a write no retry can land", queue.rows)
	}
	wantTicketNote(t, notes, "PR #42 merged, but its ticket MAD-1764 could not be moved")
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
			queue := newPendingTrackerMergesFake()
			notes := &notesFake{}
			h.srv.pendingTrackerMerges, h.srv.notes = queue, notes
			h.verbs.markMerged, h.verbs.markErr = tc.markMerged, tc.markErr

			before := time.Now()
			resp, err := h.merge(false)
			if err != nil {
				t.Fatalf("OperatorMergeSession: %v", err)
			}
			if !resp.GetIsMerged() || resp.GetTrackerWrites() != trackerWritesRetrying {
				t.Fatalf("response = %+v, want a landed merge whose tracker write is retrying", resp)
			}
			p, ok := queue.rows["s1"]
			if !ok || p.AttemptCount != 1 || p.LastReason == "" || p.NextAttemptAt.Before(before.Add(trackerRetryDelays[0])) {
				t.Fatalf("queue = %+v, want s1 after one attempt, due no sooner than %v", queue.rows, trackerRetryDelays[0])
			}
			if p.NextAttemptAt.After(time.Now().Add(trackerRetryDelays[0])) {
				t.Fatalf("queue = %+v, want the retry due %v after the failure, not after the merge's budget", queue.rows, trackerRetryDelays[0])
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
		queue := newPendingTrackerMergesFake()
		notes := &notesFake{}
		h.srv.pendingTrackerMerges, h.srv.notes = queue, notes
		h.verbs.markMerged = r

		resp, err := h.merge(false)
		if err != nil {
			t.Fatalf("OperatorMergeSession: %v", err)
		}
		if resp.GetTrackerWrites() != r.TrackerWrites || len(queue.rows) != 0 || len(notes.created) != 0 {
			t.Fatalf("%+v: tracker writes = %q, queue = %+v, notes = %+v; want the outcome reported as is, nothing queued and nothing noted", r, resp.GetTrackerWrites(), queue.rows, notes.created)
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
	wantTicketNote(t, notes, "PR #42 merged, but its ticket could not be moved")
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
	wantTicketNote(t, notes, "PR #42 merged, but its ticket MAD-1764 could not be moved")
}
