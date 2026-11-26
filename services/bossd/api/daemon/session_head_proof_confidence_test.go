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

type headProofConfidenceCase struct {
	name       string
	state      machine.State
	display    *vcs.DisplayInfo
	record     *evidence.ProofRecord
	want       *pb.ProofConfidence
	wantParked pb.ConfidenceLevel
}

var (
	headProofRecord = evidence.ProofRecord{Head: timelineHead, Level: "high", LoweredBy: []string{"Needs review"}, Asks: []string{"database"}}
	headProofWant   = &pb.ProofConfidence{HeadSha: timelineHead, Level: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, LoweredBy: []string{"Needs review"}, Asks: []string{"database"}}
)

// TestHeadProofConfidence proves every session read (list, get and the
// stream bosso serves) carries the proof record for the head bossd tracks, in
// any state, and nothing for another head, a legacy record or no reader.
func TestHeadProofConfidence(t *testing.T) {
	record := headProofRecord
	other := record
	other.Head = strings.Repeat("b", 40)
	legacy := record
	legacy.Level, legacy.Legacy = "", true
	want := headProofWant
	for _, tc := range []headProofConfidenceCase{
		{name: "open with display head", state: machine.ReadyForReview, display: &vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead}, record: &record, want: want},
		{name: "record for another head", state: machine.ReadyForReview, display: &vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead}, record: &other},
		{name: "legacy record", state: machine.ReadyForReview, display: &vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead}, record: &legacy},
		{name: "no evidence reader", state: machine.ReadyForReview, display: &vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead}},
		{name: "merged with snapshot head only", state: machine.Merged, record: &record, want: want},
	} {
		t.Run(tc.name, func(t *testing.T) { checkHeadProofConfidence(t, tc) })
	}
}

// TestHeadProofConfidence_Sessions_AC276901_ReportsRecordOverPark proves the
// field reports the record's level while a sign-off park's label shows another.
func TestHeadProofConfidence_Sessions_AC276901_ReportsRecordOverPark(t *testing.T) {
	record := headProofRecord
	checkHeadProofConfidence(t, headProofConfidenceCase{
		state:   machine.ReadyForReview,
		display: &vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "human-review:database", VerifyLabel: "moderate: Migrations", HeadSHA: timelineHead},
		record:  &record, want: headProofWant, wantParked: pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE,
	})
}

func checkHeadProofConfidence(t *testing.T, tc headProofConfidenceCase) {
	t.Helper()
	n := 1
	row := &models.Session{ID: "s", RepoID: "r", State: tc.state, PRNumber: &n, UpdatedAt: time.Now()}
	tracker := status.NewDisplayTracker()
	if tc.display != nil {
		tracker.Set(row.ID, *tc.display)
	}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{HeadSHA: timelineHead}}}
	if tc.record != nil {
		s.evidence = attentionProofEvidence{record: *tc.record}
	}
	listed, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	s.sessions = &sessionStoreFake{sess: row}
	fetched, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: row.ID}))
	if err != nil {
		t.Fatal(err)
	}
	streamed := &pb.Session{Id: row.ID}
	s.HydrateAttentionForStream(t.Context(), streamed)
	for path, got := range map[string]*pb.Session{"list": onlySession(t, listed.Msg.Sessions), "get": fetched.Msg.Session, "stream": streamed} {
		assertProofConfidence(t, path, got.GetPrHeadProofConfidence(), tc.want)
		if tc.wantParked != 0 && got.GetAttention().GetConfidenceLevel() != tc.wantParked {
			t.Fatalf("%s: attention level %v, want %v", path, got.GetAttention().GetConfidenceLevel(), tc.wantParked)
		}
	}
}

func assertProofConfidence(t *testing.T, path string, got, want *pb.ProofConfidence) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s: pr_head_proof_confidence = %v, want unset", path, got)
		}
		return
	}
	if got == nil || got.GetHeadSha() != want.GetHeadSha() || got.GetLevel() != want.GetLevel() ||
		!slices.Equal(got.GetLoweredBy(), want.GetLoweredBy()) || !slices.Equal(got.GetAsks(), want.GetAsks()) {
		t.Fatalf("%s: pr_head_proof_confidence = %v, want %v", path, got, want)
	}
}

func TestHeadProofConfidenceClonesRecordSlices(t *testing.T) {
	n := 1
	row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
	tracker := status.NewDisplayTracker()
	tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	s.sessions = &sessionStoreFake{sess: row}
	asks := []string{"database"}
	s.evidence = attentionProofEvidence{record: evidence.ProofRecord{Head: timelineHead, Level: "low", Asks: asks}}
	resp, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: row.ID}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Msg.Session.PrHeadProofConfidence.Asks[0] = "mutated"
	if asks[0] != "database" {
		t.Fatal("served asks alias the record's slice")
	}
}
