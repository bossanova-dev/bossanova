package server

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

func (f *repairOwnedFixture) session(t *testing.T) *pb.Session {
	t.Helper()
	resp, err := f.srv.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions)
}

func TestAttentionQueuedRepairOwned_Sessions_AC268904(t *testing.T) {
	f := newRepairOwnedFixture(t, blockedPark, false, nil)
	if got := f.attention(t); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetStatusPhrase() != "⨯ not assessed" {
		t.Fatalf("before queued: %v, want FIX %q", got, "⨯ not assessed")
	}

	f.srv.repairLease.MarkQueued(f.row.ID)
	sess := f.session(t)
	if sess.GetRepairActive() {
		t.Fatalf("queued session unexpectedly has repair_active=true")
	}
	if sess.GetRepairQueuedAt() == nil {
		t.Fatalf("queued session missing repair_queued_at")
	}
	got := sess.GetAttention()
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetProgressLabel() != attention.RepairingLabel || got.GetStatusPhrase() != "repairing" {
		t.Fatalf("queued repair owned: %v, want in progress %q", got, attention.RepairingLabel)
	}

	f.clock = f.clock.Add(status.MinRepairQueueTTL + time.Second)
	sess = f.session(t)
	if sess.GetRepairQueuedAt() != nil {
		t.Fatalf("lapsed queued session still has repair_queued_at=%v", sess.GetRepairQueuedAt())
	}
	if got := sess.GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("after queue lapse: %v, want FIX", got)
	}
}

func TestAttentionQueuedRepairSuppressesStuckWaitFix_Sessions_AC268904(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name    string
		display vcs.DisplayInfo
	}{
		{
			name:    "conflict",
			display: vcs.DisplayInfo{Status: vcs.DisplayStatusConflict, Mergeable: &no, HasFailures: true, HeadSHA: "e52fc1ca"},
		},
		{
			name:    "failing",
			display: vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, Mergeable: &[]bool{true}[0], HasFailures: true, HeadSHA: "a1b2c3d4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stuckWaitServer(t, tc.display, "none", "checks_passed_ready")
			clock := time.Unix(2_000_000, 0)
			s.repairLease = status.NewRepairLeaseManager().WithClock(0, func() time.Time { return clock })

			before := listStuckAttention(t, s)
			if before.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !strings.Contains(before.GetSentence(), "no repair is running") {
				t.Fatalf("before queued: %v, want stuck-wait FIX mentioning no repair is running", before)
			}

			s.repairLease.MarkQueued("stuck-session")
			queued := listStuckAttention(t, s)
			if queued.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && strings.HasPrefix(queued.GetAlertKey(), "stuck-wait:") {
				t.Fatalf("queued session still got stuck-wait FIX: %v", queued)
			}
			if strings.Contains(queued.GetSentence(), "no repair is running") {
				t.Fatalf("queued session still mentions no repair is running: %v", queued)
			}

			clock = clock.Add(status.MinRepairQueueTTL + time.Second)
			after := listStuckAttention(t, s)
			if after.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !strings.Contains(after.GetSentence(), "no repair is running") {
				t.Fatalf("after queue lapse: %v, want stuck-wait FIX resuming", after)
			}
		})
	}
}

func TestAttentionQueuedRepairSuppressesFinishedAttemptMergeConflictFix_Sessions_AC268904(t *testing.T) {
	no := false
	conflict := vcs.DisplayInfo{Status: vcs.DisplayStatusConflict, Mergeable: &no}
	f := newRepairOwnedFixture(t, conflict, true, func(row *models.Session) {
		row.LastRepairHeadSHA = timelineHead
		row.LastRepairDisplayStatus = int32(pb.DisplayStatus_DISPLAY_STATUS_CONFLICT)
	})

	before := f.attention(t)
	if before.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || before.GetSentence() != "Merge conflict" {
		t.Fatalf("before queued: %v, want FIX %q", before, "Merge conflict")
	}

	f.srv.repairLease.MarkQueued(f.row.ID)
	queued := f.attention(t)
	if queued.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
		t.Fatalf("while queued: %v, want IN_PROGRESS (no finished-attempt Merge conflict FIX)", queued)
	}
	if queued.GetSentence() == "Merge conflict" {
		t.Fatalf("while queued: unexpectedly got Merge conflict FIX: %v", queued)
	}

	f.clock = f.clock.Add(status.MinRepairQueueTTL + time.Second)
	after := f.attention(t)
	if after.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || after.GetSentence() != "Merge conflict" {
		t.Fatalf("after queue lapse: %v, want FIX %q", after, "Merge conflict")
	}
}
