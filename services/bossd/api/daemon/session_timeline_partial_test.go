package server

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

func TestTimelinePartialMatchesSessionAttention_ChatPicker_AC14(t *testing.T) {
	for _, tc := range []struct {
		name, displayHead, snapshotHead string
		partial                         bool
		status                          vcs.DisplayStatus
		want                            pb.AttentionKind
	}{
		{"current", timelineHead, timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"case insensitive", strings.ToUpper(timelineHead), timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"stale marker", "older-head", timelineHead, true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"empty head", "", "", true, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{"nonpartial", timelineHead, timelineHead, false, vcs.DisplayStatusPassing, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"required review", timelineHead, timelineHead, true, vcs.DisplayStatusReview, pb.AttentionKind_ATTENTION_KIND_FIX},
		{"stale required review", "older-head", timelineHead, true, vcs.DisplayStatusReview, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, mergeable := 1, true
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n}
			tr := status.NewDisplayTracker()
			tr.Set(row.ID, vcs.DisplayInfo{Status: tc.status, HeadSHA: tc.displayHead, IsPartial: tc.partial, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{SessionID: row.ID, HeadSHA: tc.snapshotHead, PolledAt: at(10), ComputedStatus: int(tc.status), RawJSON: checksJSON(t, greenCI(2, ""))}}}
			facts, err := s.timelineFacts(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			steps, kind := assembleTimeline(facts)
			if kind != tc.want {
				t.Fatalf("kind=%v want=%v", kind, tc.want)
			}
			if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
				if facts.IsPartial || (tc.partial && facts.Mergeable != nil) {
					t.Fatal("live PR facts leaked across heads")
				}
				return
			}
			resp, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if got := onlySession(t, resp.Msg.Sessions).GetAttention().GetKind(); tc.displayHead == tc.snapshotHead && got != kind {
				t.Fatalf("board=%v timeline=%v", got, kind)
			}
			assertSteps(t, steps, []wantStep{{at(10), "CI passed, 2 checks", "", done}, {at(10), "Parked for you · partial build", "", current}})
		})
	}
}

func TestTimelinePartialExistingVerifyParkIsNotDuplicated(t *testing.T) {
	f := timelineFacts{HasPR: true, IsPartial: true, Snapshots: []db.CheckSnapshot{snap(t, at(30), vcs.DisplayStatusNeedsHuman, greenCI(2, "needs human: no-receipt,partial-build"))}}
	steps, kind := assembleTimeline(f)
	if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind=%v", kind)
	}
	parks := 0
	for _, st := range steps {
		if strings.HasPrefix(st.GetTitle(), "Parked for you") {
			parks++
		}
	}
	if parks != 1 {
		t.Fatalf("park count=%d", parks)
	}
	af := attentionFacts(f, &f.Snapshots[0])
	if !af.IsPartial {
		t.Fatal("attention facts lost partial marker")
	}
	if got := attention.Compute(sessionattention.Derive(af).Input); got.GetKind() != kind {
		t.Fatalf("attention=%v", got)
	}
}
