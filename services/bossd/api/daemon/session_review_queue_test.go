package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/proto"
)

// repoScopedSessionStore answers list reads with only the requested repo's rows.
type repoScopedSessionStore struct {
	*listSessionsSessionStoreFake
}

func (f repoScopedSessionStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	rows, err := f.listSessionsSessionStoreFake.ListActiveWithRepo(ctx, repoID)
	out := rows[:0]
	for _, row := range rows {
		if row.ArchivedAt == nil && (repoID == "" || row.RepoID == repoID) {
			out = append(out, row)
		}
	}
	return out, err
}

func TestListReviewQueueRequiresRepoID_Sessions_AC209001(t *testing.T) {
	s := newListSessionsDisplayStatusTestServer(nil, nil, status.NewDisplayTracker(), nil)
	_, err := s.ListReviewQueue(t.Context(), connect.NewRequest(&pb.ListReviewQueueRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
}

func TestListReviewQueueServesHydratedParks_Sessions_AC209001(t *testing.T) {
	n := 7
	parked := &models.Session{ID: "parked", RepoID: "r", Title: "Add widgets", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
	other := &models.Session{ID: "other-repo", RepoID: "elsewhere", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
	working := &models.Session{ID: "working", RepoID: "r", State: machine.ImplementingPlan, PRNumber: &n, UpdatedAt: time.Now()}
	tracker := status.NewDisplayTracker()
	for _, id := range []string{parked.ID, other.ID} {
		tracker.Set(id, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "human-review:database", VerifyLabel: "moderate: Migrations", HeadSHA: timelineHead})
	}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{parked, other, working}, nil, tracker, nil)
	s.sessions = repoScopedSessionStore{s.sessions.(*listSessionsSessionStoreFake)}

	repoID := "r"
	listed, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{RepoId: &repoID}))
	if err != nil {
		t.Fatal(err)
	}
	var want *pb.Session
	for _, sess := range listed.Msg.GetSessions() {
		if sess.GetId() == parked.ID {
			want = sess
		}
	}
	if want == nil {
		t.Fatal("ListSessions did not serve the parked session")
	}

	got, err := s.ListReviewQueue(t.Context(), connect.NewRequest(&pb.ListReviewQueueRequest{RepoId: repoID}))
	if err != nil {
		t.Fatal(err)
	}
	rows := got.Msg.GetRows()
	if len(rows) != 1 || rows[0].GetSession().GetId() != parked.ID {
		t.Fatalf("rows = %v, want only the parked session in repo r", rows)
	}
	if !proto.Equal(rows[0].GetSession(), want) {
		t.Fatalf("row session = %v\nwant ListSessions' %v", rows[0].GetSession(), want)
	}
	if rows[0].GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
		t.Fatalf("row level = %v, want MODERATE", rows[0].GetConfidenceLevel())
	}
	for _, c := range got.Msg.GetLevelCounts() {
		wantCount := int32(0)
		if c.GetConfidenceLevel() == pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
			wantCount = 1
		}
		if c.GetSessionCount() != wantCount {
			t.Fatalf("count %v = %d, want %d", c.GetConfidenceLevel(), c.GetSessionCount(), wantCount)
		}
	}
	if len(got.Msg.GetLevelCounts()) != len(pb.ConfidenceLevel_name) {
		t.Fatalf("level counts = %v, want one per level", got.Msg.GetLevelCounts())
	}
}

// archivedReadForbidden fails the test if the queue reads archived sessions.
type archivedReadForbidden struct {
	repoScopedSessionStore
	t *testing.T
}

func (f archivedReadForbidden) ListWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	f.t.Error("ListReviewQueue read archived sessions")
	return f.repoScopedSessionStore.ListWithRepo(ctx, repoID)
}

// Merged PRs are never queue rows, whatever their merged proof level and
// whether or not they are archived, and they add nothing to the counts.
func TestListReviewQueueListsNoMergedPR(t *testing.T) {
	now := time.Now()
	recent := now.Add(-time.Hour)
	var rows []*models.Session
	for i := range 20 {
		n := i + 1
		rows = append(rows,
			&models.Session{ID: fmt.Sprintf("merged-%d", n), RepoID: "r", State: machine.Merged, PRNumber: &n, StateEnteredAt: &recent, UpdatedAt: now},
			&models.Session{ID: fmt.Sprintf("archived-%d", n), RepoID: "r", State: machine.Merged, PRNumber: &n, StateEnteredAt: &recent, ArchivedAt: &now, UpdatedAt: now},
		)
	}
	tracker := status.NewDisplayTracker()
	for _, row := range rows {
		tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusMerged, HeadSHA: timelineHead})
	}
	s := newListSessionsDisplayStatusTestServer(rows, nil, tracker, nil)
	s.sessions = archivedReadForbidden{repoScopedSessionStore{s.sessions.(*listSessionsSessionStoreFake)}, t}
	s.evidence = attentionProofEvidence{record: evidence.ProofRecord{Head: timelineHead, Level: "high"}}
	got, err := s.ListReviewQueue(t.Context(), connect.NewRequest(&pb.ListReviewQueueRequest{RepoId: "r"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Msg.GetRows()) != 0 {
		t.Fatalf("rows = %v, want none", got.Msg.GetRows())
	}
	for _, c := range got.Msg.GetLevelCounts() {
		if c.GetSessionCount() != 0 {
			t.Fatalf("count %v = %d, want 0", c.GetConfidenceLevel(), c.GetSessionCount())
		}
	}
}

// A chat asking a question on a parked PR outranks the park in its attention,
// but the head is still parked for a person, so the PR stays in the queue at
// its park's level, with its park on the row's session.
func TestListReviewQueueKeepsParkWhileChatAsks_Sessions_AC209001(t *testing.T) {
	n := 7
	parked := &models.Session{ID: "parked", RepoID: "r", Title: "Add widgets", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
	tracker := status.NewDisplayTracker()
	tracker.Set(parked.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "human-review:database", VerifyLabel: "moderate: Migrations", HeadSHA: timelineHead})
	chat := &domain.AgentChat{ID: "c", SessionID: parked.ID, AgentSessionID: "asking", AgentName: "claude"}
	chats := status.NewTracker()
	asked := time.Now()
	chats.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_QUESTION, asked)
	s := newListSessionsDisplayStatusTestServer([]*models.Session{parked}, map[string][]*domain.AgentChat{parked.ID: {chat}}, tracker, chats)
	s.attentionCache.questions = map[string]questionAttention{chat.AgentSessionID: {text: "Which base?", askedAt: asked, at: asked}}
	s.sessions = repoScopedSessionStore{s.sessions.(*listSessionsSessionStoreFake)}

	got, err := s.ListReviewQueue(t.Context(), connect.NewRequest(&pb.ListReviewQueueRequest{RepoId: "r"}))
	if err != nil {
		t.Fatal(err)
	}
	rows := got.Msg.GetRows()
	if len(rows) != 1 || rows[0].GetSession().GetId() != parked.ID {
		t.Fatalf("rows = %v, want the parked session", rows)
	}
	sess := rows[0].GetSession()
	if kind := sess.GetAttention().GetKind(); kind != pb.AttentionKind_ATTENTION_KIND_QUESTION {
		t.Fatalf("attention kind = %v, want QUESTION", kind)
	}
	if park := sess.GetVerifyPark(); park.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || park.GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
		t.Fatalf("verify park = %v, want a Moderate REVIEW", park)
	}
	if rows[0].GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE {
		t.Fatalf("row level = %v, want MODERATE", rows[0].GetConfidenceLevel())
	}
}
