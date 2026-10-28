package status

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

func TestDisplayTrackerPartialRefresh(t *testing.T) {
	tr := NewDisplayTracker()
	info := vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, IsPartial: true}
	tr.Restore("restored", info, time.Now())
	tr.Set("live", info)
	for _, id := range []string{"restored", "live"} {
		if !tr.Get(id).IsPartial || !tr.GetBatch([]string{id})[id].IsPartial {
			t.Fatalf("%s lost partial marker", id)
		}
		tr.Set(id, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
		if tr.Get(id).IsPartial || tr.GetBatch([]string{id})[id].IsPartial {
			t.Fatalf("%s retained removed partial marker", id)
		}
	}
}

func TestDisplayPartialMarkerPublishesAttentionWithoutDisplayWrite(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	id := mustSession(t, sessions, mustRepo(t, repos))
	tr := NewDisplayTracker()
	mergeable := true
	info := vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: "current", Mergeable: &mergeable}
	tr.Set(id, info)
	computer := NewDisplayStatusComputer(sessions, tr, nil, chats, workflows, zerolog.Nop())
	if err := computer.Recompute(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	before, err := sessions.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	computer.SetOnUpdate(func(context.Context, string) { writes++ })
	tr.SetRecomputer(computer)
	var published []pb.AttentionKind
	tr.SetOnAttentionChange(func(sessionID string) {
		entry := tr.Get(sessionID)
		got := attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: entry.Mergeable, IsPartial: entry.IsPartial})
		published = append(published, got.GetKind())
	})
	for _, partial := range []bool{true, true, false, false} {
		info.IsPartial = partial
		tr.Set(id, info)
	}
	if len(published) != 2 || published[0] != pb.AttentionKind_ATTENTION_KIND_FIX || published[1] != pb.AttentionKind_ATTENTION_KIND_MERGE {
		t.Fatalf("published attention = %v", published)
	}
	after, err := sessions.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 || before.DisplayLabel != after.DisplayLabel || before.DisplayIntent != after.DisplayIntent || before.DisplaySpinner != after.DisplaySpinner {
		t.Fatal("marker-only changes rewrote the legacy display trio")
	}
}
