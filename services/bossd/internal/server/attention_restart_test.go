package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// TestAttentionSurvivesADaemonRestart serves 31 ready PRs before a restart
// and again from the statuses the display poller restores, before GitHub has
// been read. Every session must answer the same MERGE, since and head, so no
// client sees it go in progress and come back.
func TestAttentionSurvivesADaemonRestart(t *testing.T) {
	entered := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	rows := make([]*models.Session, 31)
	for i := range rows {
		pr := 11700 + i
		since := entered.Add(time.Duration(i) * time.Minute)
		rows[i] = &models.Session{ID: fmt.Sprintf("ready-%02d", i), RepoID: "r", PRNumber: &pr, State: machine.ReadyForReview, StateEnteredAt: &since, UpdatedAt: since}
	}
	polled := entered.Add(time.Hour)
	snapshots := &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{snap(t, polled, vcs.DisplayStatusPassing, greenCI(2, ""))}}
	mergeable := true
	serve := func(tr *status.DisplayTracker) map[string]*pb.Session {
		s := newListSessionsDisplayStatusTestServer(rows, nil, tr, nil)
		s.checkSnapshots = snapshots
		resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]*pb.Session, len(resp.Msg.Sessions))
		for _, sess := range resp.Msg.Sessions {
			out[sess.GetId()] = sess
		}
		return out
	}

	live := status.NewDisplayTracker()
	restored := status.NewDisplayTracker()
	for _, row := range rows {
		live.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
		restored.Restore(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable}, polled)
	}
	before, after := serve(live), serve(restored)
	for _, row := range rows {
		b, a := before[row.ID], after[row.ID]
		if b.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE {
			t.Fatalf("%s before the restart: %v, want MERGE", row.ID, b.GetAttention())
		}
		if a.GetAttention().GetKind() != b.GetAttention().GetKind() ||
			!a.GetAttention().GetSince().AsTime().Equal(b.GetAttention().GetSince().AsTime()) ||
			a.GetPrDisplayHeadSha() != b.GetPrDisplayHeadSha() {
			t.Fatalf("%s after the restart: %v head %q, before: %v head %q", row.ID, a.GetAttention(), a.GetPrDisplayHeadSha(), b.GetAttention(), b.GetPrDisplayHeadSha())
		}
	}
}
