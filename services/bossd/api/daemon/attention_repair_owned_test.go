package server

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// repairOwnedFixture is one session with a PR, its display entry and a repair
// lease manager on a clock the test moves.
type repairOwnedFixture struct {
	srv     *Server
	row     *models.Session
	tracker *status.DisplayTracker
	clock   time.Time
}

func newRepairOwnedFixture(t *testing.T, info vcs.DisplayInfo, canAutoRepair bool, edit func(*models.Session)) *repairOwnedFixture {
	t.Helper()
	n := 7
	row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if edit != nil {
		edit(row)
	}
	tracker := status.NewDisplayTracker()
	info.HeadSHA = timelineHead
	tracker.Set(row.ID, info)
	f := &repairOwnedFixture{row: row, tracker: tracker, clock: time.Unix(2_000_000, 0)}
	f.srv = newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	f.srv.repairLease = status.NewRepairLeaseManager().WithClock(0, func() time.Time { return f.clock })
	f.srv.repos = &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"r": {ID: "r", CanAutoRepair: canAutoRepair}}}
	return f
}

func (f *repairOwnedFixture) attention(t *testing.T) *pb.SessionAttention {
	t.Helper()
	resp, err := f.srv.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions).GetAttention()
}

var blockedPark = vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "cannot-assess"}

// TestAttentionRepairOwned_Sessions_AC233502_LeaseAndDisplayMoveABlockedParkInProgress pins
// two RepairOwned sources: a live repair lease and the repair the display
// reports each read a blocked verify park in progress "repairing", and the
// park is the operator's FIX again once neither holds.
func TestAttentionRepairOwned_Sessions_AC233502_LeaseAndDisplayMoveABlockedParkInProgress(t *testing.T) {
	for _, source := range []string{"lease", "display"} {
		t.Run(source, func(t *testing.T) {
			f := newRepairOwnedFixture(t, blockedPark, false, nil)
			if got := f.attention(t); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetStatusPhrase() != "fix needed" {
				t.Fatalf("before repair: %v, want FIX %q", got, "fix needed")
			}
			if source == "lease" {
				if err := f.srv.repairLease.Acquire(f.row.ID, "repairer"); err != nil {
					t.Fatal(err)
				}
			} else {
				f.tracker.SetRepairing(f.row.ID, true)
			}
			got := f.attention(t)
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetProgressLabel() != attention.RepairingLabel || got.GetStatusPhrase() != "repairing" {
				t.Fatalf("repair owned: %v, want in progress %q", got, attention.RepairingLabel)
			}
			if source == "lease" {
				f.srv.repairLease.Release(f.row.ID, "repairer")
			} else {
				f.tracker.SetRepairing(f.row.ID, false)
			}
			if got := f.attention(t); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
				t.Fatalf("repair ended: %v, want FIX", got)
			}
		})
	}
}

// TestAttentionRepairOwned_Sessions_AC233502_ArmedRepair pins the third source, repair the
// plugin will start on a failing or changes-requested PR, and the facts that
// keep it from arming: a verify-only session, automatic repair off, a state
// the plugin skips, exhausted attempts, a recorded refusal, a stalled lease or
// a finished attempt on this head and status, which the plugin skips as a
// repeated input.
func TestAttentionRepairOwned_Sessions_AC233502_ArmedRepair(t *testing.T) {
	failing := vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true}
	refused := time.Now()
	for _, tc := range []struct {
		name     string
		info     vcs.DisplayInfo
		repair   bool
		edit     func(*models.Session)
		stall    bool
		repaired bool
	}{
		{"failing PR", failing, true, nil, false, true},
		{"changes requested", vcs.DisplayInfo{Status: vcs.DisplayStatusRejected, HasChangesRequested: true}, true, nil, false, true},
		{"verify only", failing, true, func(r *models.Session) { r.IsVerifyOnly = true }, false, false},
		{"automatic repair off", failing, false, nil, false, false},
		{"finalizing", failing, true, func(r *models.Session) { r.State = machine.Finalizing }, false, false},
		{"implementing", failing, true, func(r *models.Session) { r.State = machine.ImplementingPlan }, false, false},
		{"attempts exhausted", failing, true, func(r *models.Session) { r.LastRepairAttemptCount = repairExhaustedAttempts }, false, false},
		{"refusal recorded", failing, true, func(r *models.Session) {
			r.LastRepairBlockedReason, r.LastRepairBlockedAt = "repair could not start", &refused
		}, false, false},
		{"stalled lease", failing, true, nil, true, false},
		{"finished attempt on this head", failing, true, func(r *models.Session) {
			r.LastRepairHeadSHA, r.LastRepairDisplayStatus = timelineHead, int32(pb.DisplayStatus_DISPLAY_STATUS_FAILING)
		}, false, false},
		{"finished attempt on another status", failing, true, func(r *models.Session) {
			r.LastRepairHeadSHA, r.LastRepairDisplayStatus = timelineHead, int32(pb.DisplayStatus_DISPLAY_STATUS_REJECTED)
		}, false, true},
		{"finished attempt on an older head", failing, true, func(r *models.Session) {
			r.LastRepairHeadSHA, r.LastRepairDisplayStatus = "older", int32(pb.DisplayStatus_DISPLAY_STATUS_FAILING)
		}, false, true},
		{"runner failure on this head", failing, true, func(r *models.Session) {
			r.LastRepairHeadSHA, r.LastRepairDisplayStatus = timelineHead, int32(pb.DisplayStatus_DISPLAY_STATUS_FAILING)
			r.LastRepairRunnerError = "runner exited"
		}, false, true},
		{"checks running", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking}, true, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairOwnedFixture(t, tc.info, tc.repair, tc.edit)
			if tc.stall {
				if err := f.srv.repairLease.Acquire(f.row.ID, "repairer"); err != nil {
					t.Fatal(err)
				}
				f.clock = f.clock.Add(status.RepairStallWindow + time.Minute)
				if _, stalled := f.srv.repairLease.StalledAt(f.row.ID); !stalled || f.srv.repairLease.Active(f.row.ID) {
					t.Fatal("lease did not stall")
				}
			}
			got := f.attention(t)
			if tc.edit != nil && f.row.LastRepairAttemptCount >= repairExhaustedAttempts {
				// Exhausted repair is the operator's: its FIX never reads in progress.
				if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetStatusPhrase() != "repair failed" {
					t.Fatalf("exhausted repair: %v, want FIX %q", got, "repair failed")
				}
				return
			}
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				t.Fatalf("kind = %v, want in progress: %v", got.GetKind(), got)
			}
			if repaired := got.GetStatusPhrase() == "repairing"; repaired != tc.repaired {
				t.Fatalf("status phrase = %q, want repairing=%v", got.GetStatusPhrase(), tc.repaired)
			}
		})
	}
}

// TestAttentionRepairOwned_Sessions_AC233501_AC233502_ServesRepairingTally is the acceptance
// criterion end to end: a blocked verify park whose repair lease is active
// and whose head has one P2 blocker serves in progress "repairing 1 P2".
func TestAttentionRepairOwned_Sessions_AC233501_AC233502_ServesRepairingTally(t *testing.T) {
	h := newBlockerHarness(t, written(oneP2))
	h.tracker.Set(h.row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "cannot-assess", HeadSHA: headPin})
	h.srv.repairLease = status.NewRepairLeaseManager()
	if err := h.srv.repairLease.Acquire(h.row.ID, "repairer"); err != nil {
		t.Fatal(err)
	}
	if got := h.list(t); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetStatusPhrase() != "repairing" {
		t.Fatalf("before the blockers read: %v, want in progress %q", got, "repairing")
	}
	h.awaitRefresh(t)
	got := h.list(t)
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetProgressLabel() != attention.RepairingLabel {
		t.Fatalf("attention = %v, want in progress %q", got, attention.RepairingLabel)
	}
	if got.GetStatusPhrase() != "repairing 1 P2" || got.GetStatusDetail() != "in services/a.go" {
		t.Fatalf("status = %q / %q, want %q / %q", got.GetStatusPhrase(), got.GetStatusDetail(), "repairing 1 P2", "in services/a.go")
	}
}
