package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
)

type attentionProofEvidence struct {
	stubEvidence
	record evidence.ProofRecord
}

// Deliberately return even a mismatched record: hydration checks its pin too.
func (e attentionProofEvidence) Proof(string, string) (evidence.ProofRecord, bool) {
	return e.record, true
}

func TestProof_Sessions_AC208702_HydratesCurrentProof(t *testing.T) {
	for _, head := range []string{strings.ToUpper(timelineHead), strings.Repeat("b", 40)} {
		for _, hasEntry := range []bool{true, false} {
			n := 1
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
			tracker := status.NewDisplayTracker()
			tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "human-review:database", VerifyLabel: "moderate: Migrations", HeadSHA: timelineHead})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
			entries := stubEvidence{}
			if hasEntry {
				entries["s"] = evidence.Entry{Summary: evidence.Summary{Head: timelineHead, VideoURL: "https://video.test/clip"}}
			}
			s.evidence = attentionProofEvidence{stubEvidence: entries, record: evidence.ProofRecord{Head: head, Page: "https://proof.test/page", Level: "high", Asks: []string{"database"}, LoweredBy: []string{"Needs review"}, Media: []evidence.Media{{Kind: "video", URL: "https://video.test/clip"}}}}
			listed, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			s.sessions = &sessionStoreFake{sess: row}
			fetched, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: row.ID}))
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range []*pb.SessionAttention{onlySession(t, listed.Msg.Sessions).Attention, fetched.Msg.Session.Attention} {
				current := strings.EqualFold(head, timelineHead)
				if slices.Contains(got.Actions, pb.AttentionAction_ATTENTION_ACTION_PROOF) != current || slices.Contains(got.Actions, pb.AttentionAction_ATTENTION_ACTION_VIDEO) != (!current && hasEntry) {
					t.Fatalf("head %s: %v", head, got)
				}
				if current && (got.ProofUrl != "https://proof.test/page" || !slices.Equal(got.LoweredBy, []string{"Needs review"}) || got.EvidenceUrl != "https://video.test/clip" || got.ConfidenceLevel != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE || !slices.Equal(got.Asks, []string{"Migrations"})) {
					t.Fatal(got)
				}
				if !current && (got.ProofUrl != "" || len(got.LoweredBy) > 0) {
					t.Fatal(got)
				}
			}
		}
	}
}

func TestMergedProofConfidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       machine.State
		display     bool
		head, level string
		reader      bool
		want        pb.ConfidenceLevel
	}{
		{"display head", machine.Merged, true, timelineHead, "high", true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH},
		{"snapshot head", machine.Merged, false, timelineHead, "high", true, pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH},
		{"open", machine.ReadyForReview, true, timelineHead, "high", true, 0},
		{"different head", machine.Merged, true, strings.Repeat("b", 40), "high", true, 0},
		{"legacy", machine.Merged, true, timelineHead, "", true, 0},
		{"no reader", machine.Merged, true, timelineHead, "high", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			row := &models.Session{ID: "s", RepoID: "r", State: tc.state, PRNumber: &n, UpdatedAt: time.Now()}
			tracker := status.NewDisplayTracker()
			if tc.display {
				tracker.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged, HeadSHA: timelineHead})
			}
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
			s.sessions = &sessionStoreFake{sess: row}
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{HeadSHA: timelineHead}}}
			if tc.reader {
				s.evidence = attentionProofEvidence{record: evidence.ProofRecord{Head: tc.head, Level: tc.level}}
			}
			resp, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: "s"}))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Msg.Session.GetMergedConfidenceLevel(); got != tc.want {
				t.Fatalf("level=%v want %v", got, tc.want)
			}
		})
	}
}
