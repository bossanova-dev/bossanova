package session

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

func TestDisplayPollerPartialMarker(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "draft"}[draft], func(t *testing.T) {
			sessions, repos, vp := newMockSessionStore(), newMockRepoStore(), newMockVCSProvider()
			tr := status.NewDisplayTracker()
			n := 42
			repos.repos["r"] = &models.Repo{ID: "r", OriginURL: "owner/repo"}
			sessions.sessions["s"] = &models.Session{ID: "s", RepoID: "r", PRNumber: &n}
			vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: draft, Mergeable: boolPtr(true), HeadSHA: "head", Body: "## Partial\ndo not merge — partial: 1/2"}
			poller := NewDisplayPoller(sessions, repos, vp, tr, time.Minute, zerolog.Nop())
			snapshots := newMockCheckSnapshotStore()
			poller.SetSnapshotStore(snapshots)
			published := 0
			tr.SetOnAttentionChange(func(sessionID string) {
				published++
				entry := tr.Get(sessionID)
				snaps, err := snapshots.ListBySession(t.Context(), sessionID, 1)
				if err != nil || len(snaps) != 1 || snaps[0].HeadSHA != entry.HeadSHA || snaps[0].ComputedStatus != int(entry.Status) {
					t.Fatalf("attention published before current checks: entry=%+v snapshots=%+v err=%v", entry, snaps, err)
				}
			})
			if !poller.pollSession(t.Context(), repos.repos["r"], sessions.sessions["s"], n) {
				t.Fatal("poll failed")
			}
			if e := tr.Get("s"); e == nil || !e.IsPartial {
				t.Fatalf("entry=%+v", e)
			}
			vp.nextPRStatus.Body = ""
			vp.nextPRStatus.HeadSHA = "next-head"
			if !poller.pollSession(t.Context(), repos.repos["r"], sessions.sessions["s"], n) {
				t.Fatal("poll failed")
			}
			if tr.Get("s").IsPartial {
				t.Fatal("marker retained after removal")
			}
			if published != 2 {
				t.Fatalf("attention publications=%d, want 2", published)
			}
		})
	}
}
