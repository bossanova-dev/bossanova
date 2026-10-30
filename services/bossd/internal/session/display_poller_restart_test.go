package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// TestDisplayPollerRestartServesLastKnownStatus reproduces the operator's
// restart: 31 sessions whose PRs were green and mergeable, a daemon restart,
// and GitHub not yet answered. Every session must keep the status the daemon
// last saw rather than none, which every reader takes for "in progress".
func TestDisplayPollerRestartServesLastKnownStatus(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	ids := make([]string, 31)
	for i := range ids {
		pr := 11700 + i
		ids[i] = fmt.Sprintf("ready-%02d", i)
		sessions.sessions[ids[i]] = &models.Session{ID: ids[i], RepoID: "repo-1", PRNumber: &pr}
	}
	snapshots := newMockCheckSnapshotStore()
	success := vcs.CheckConclusionSuccess
	vp := newMockVCSProvider()
	vp.nextCheckResults = []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &success}}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1", Mergeable: boolPtr(true)}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	before := NewDisplayPoller(sessions, repos, vp, status.NewDisplayTracker(), time.Hour, zerolog.Nop())
	before.SetSnapshotStore(snapshots)
	before.Run(ctx)
	runOnePollCycle(t, before, cancel)

	tracker := status.NewDisplayTracker()
	var mu sync.Mutex
	var changed []string
	tracker.SetOnChange(func(id string, _, _ *status.DisplayEntry) {
		mu.Lock()
		defer mu.Unlock()
		changed = append(changed, id)
	})
	vp.prStatusErr = errors.New("GitHub has not answered yet")
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	after := NewDisplayPoller(sessions, repos, vp, tracker, time.Hour, zerolog.Nop())
	after.SetSnapshotStore(snapshots)
	after.Run(ctx)
	runOnePollCycle(t, after, cancel)

	for _, id := range ids {
		e := tracker.Get(id)
		if e == nil {
			t.Fatalf("%s has no status after the restart, so it reads as in progress", id)
		}
		if e.Status != vcs.DisplayStatusPassing || e.HeadSHA != "head-1" || e.Mergeable == nil || !*e.Mergeable {
			t.Fatalf("%s restored as %+v, want passing and mergeable on head-1", id, e)
		}
	}
	mu.Lock()
	if len(changed) != 0 {
		t.Fatalf("restoring reported %d status changes to plugins, want none", len(changed))
	}
	mu.Unlock()

	vp.prStatusErr = nil
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	after.lastPoll = nil
	after.poll(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(changed)
		mu.Unlock()
		if n == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first fresh poll reported %d status changes to plugins, want %d, as on an empty tracker", n, len(ids))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDisplayPollerRestoresWhatTheSnapshotRecorded covers the statuses a
// restart restores beyond a green PR, and the ended PR it leaves to the poll,
// whose terminal reconcile a restart exists to retry.
func TestDisplayPollerRestoresWhatTheSnapshotRecorded(t *testing.T) {
	pending := vcs.CheckStatusInProgress
	success := vcs.CheckConclusionSuccess
	failure := vcs.CheckConclusionFailure
	parked := []vcs.CheckResult{
		{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &success},
		{Name: vcs.VerifyStatusContext, Status: pending, Description: "needs human: ledger-open"},
	}
	failing := []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &failure}}
	for _, tc := range []struct {
		name   string
		status vcs.DisplayStatus
		checks []vcs.CheckResult
		want   *status.DisplayEntry
	}{
		{"verify park", vcs.DisplayStatusNeedsHuman, parked, &status.DisplayEntry{Status: vcs.DisplayStatusNeedsHuman, HeadSHA: "h", VerifyReason: "ledger-open"}},
		{"failing checks", vcs.DisplayStatusFailing, failing, &status.DisplayEntry{Status: vcs.DisplayStatusFailing, HeadSHA: "h", HasFailures: true}},
		{"changes requested", vcs.DisplayStatusRejected, nil, &status.DisplayEntry{Status: vcs.DisplayStatusRejected, HeadSHA: "h", HasChangesRequested: true}},
		{"conflict", vcs.DisplayStatusConflict, nil, &status.DisplayEntry{Status: vcs.DisplayStatusConflict, HeadSHA: "h"}},
		{"merged", vcs.DisplayStatusMerged, nil, nil},
		{"closed", vcs.DisplayStatusClosed, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			pr := 7
			sessions.sessions["s"] = &models.Session{ID: "s", RepoID: "repo-1", PRNumber: &pr}
			snapshots := newMockCheckSnapshotStore()
			raw, err := json.Marshal(tc.checks)
			if err != nil {
				t.Fatal(err)
			}
			polled := time.Now().Add(-time.Hour)
			if err := snapshots.Create(context.Background(), db.CheckSnapshot{SessionID: "s", PolledAt: polled, HeadSHA: "h", RawJSON: string(raw), ComputedStatus: int(tc.status)}); err != nil {
				t.Fatal(err)
			}
			vp := newMockVCSProvider()
			vp.prStatusErr = errors.New("GitHub has not answered yet")
			tracker := status.NewDisplayTracker()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p := NewDisplayPoller(sessions, repos, vp, tracker, time.Hour, zerolog.Nop())
			p.SetSnapshotStore(snapshots)
			p.Run(ctx)
			runOnePollCycle(t, p, cancel)

			got := tracker.Get("s")
			if tc.want == nil {
				if got != nil {
					t.Fatalf("restored %+v, want the ended PR left to the poll", got)
				}
				return
			}
			if got == nil {
				t.Fatal("nothing restored")
			}
			if got.Status != tc.want.Status || got.HeadSHA != tc.want.HeadSHA || got.VerifyReason != tc.want.VerifyReason ||
				got.HasFailures != tc.want.HasFailures || got.HasChangesRequested != tc.want.HasChangesRequested || got.Mergeable != nil {
				t.Fatalf("restored %+v, want %+v", got, tc.want)
			}
			if !got.UpdatedAt.Equal(polled) {
				t.Fatalf("restored entry dated %v, want the snapshot's %v", got.UpdatedAt, polled)
			}
		})
	}
}

// TestDisplayPollerRestoresTheBuildReceiptItsLabelShowed restores a green PR
// whose row last read "✓ ready". The snapshot does not record the build
// receipt, so without it the row would read "✓ passing" until GitHub answers.
// A row that read anything else restores no receipt.
func TestDisplayPollerRestoresTheBuildReceiptItsLabelShowed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		label   string
		status  vcs.DisplayStatus
		receipt bool
	}{
		{"ready and passing", "✓ ready", vcs.DisplayStatusPassing, true},
		{"ready and approved", "✓ ready", vcs.DisplayStatusApproved, true},
		{"passing", "✓ passing", vcs.DisplayStatusPassing, false},
		{"ready label on a failing snapshot", "✓ ready", vcs.DisplayStatusFailing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
			pr := 7
			sessions.sessions["s"] = &models.Session{ID: "s", RepoID: "repo-1", PRNumber: &pr, DisplayLabel: tc.label}
			snapshots := newMockCheckSnapshotStore()
			if err := snapshots.Create(context.Background(), db.CheckSnapshot{SessionID: "s", PolledAt: time.Now().Add(-time.Hour), HeadSHA: "h", RawJSON: "[]", ComputedStatus: int(tc.status)}); err != nil {
				t.Fatal(err)
			}
			vp := newMockVCSProvider()
			vp.prStatusErr = errors.New("GitHub has not answered yet")
			tracker := status.NewDisplayTracker()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p := NewDisplayPoller(sessions, repos, vp, tracker, time.Hour, zerolog.Nop())
			p.SetSnapshotStore(snapshots)
			p.Run(ctx)
			runOnePollCycle(t, p, cancel)

			got := tracker.Get("s")
			if got == nil || got.Status != tc.status {
				t.Fatalf("restored %+v, want status %d", got, tc.status)
			}
			if got.HasBuildReceipt != tc.receipt {
				t.Fatalf("a row that read %q restored the build receipt %v, want %v", tc.label, got.HasBuildReceipt, tc.receipt)
			}
			if !got.Restored {
				t.Fatal("the entry is not marked restored")
			}
		})
	}
}

func TestRestoredDisplayInfo_ParkStateDoesNotChangeReasonAndLabel_Sessions_AC147(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	for _, status := range []vcs.CheckStatus{vcs.CheckStatusQueued, vcs.CheckStatusCompleted} {
		t.Run(map[bool]string{true: "success", false: "pending"}[status == vcs.CheckStatusCompleted], func(t *testing.T) {
			raw, err := json.Marshal([]vcs.CheckResult{{Name: vcs.VerifyStatusContext, Status: status, Conclusion: &success, Description: "needs human: ledger-open · outstanding review"}})
			if err != nil {
				t.Fatal(err)
			}
			info, ok := restoredDisplayInfo(db.CheckSnapshot{ComputedStatus: int(vcs.DisplayStatusNeedsHuman), RawJSON: string(raw)}, "")
			if !ok || info.VerifyReason != "ledger-open" || info.VerifyLabel != "outstanding review" {
				t.Fatalf("restored = %+v, ok = %v", info, ok)
			}
		})
	}
}
