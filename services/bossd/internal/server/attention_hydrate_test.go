package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	libevidence "github.com/recurser/bossalib/evidence"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAttentionListReviewCases(t *testing.T) {
	for _, tc := range []struct{ id, code, label, sentence string }{
		{"2979", "human-review:ui", "", "Watch the evidence video (2:40), then approve · UI change"},
		{"2983", "human-review:security", "outbound requests", "Review the diff · security: outbound requests"},
		{"2987", "human-review:database", "", "Review the migration, then approve"},
		{"2967", "human-review:unproven", "", "Some checks have no evidence · watch the video, then approve"},
		{"2965", "no-receipt", "", "Built by hand, no factory review · review the diff, then approve"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			n := 1
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
			tracker := status.NewDisplayTracker()
			tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: tc.code, VerifyLabel: tc.label, HeadSHA: timelineHead})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
			s.evidence = stubEvidence{"s": evidence.Entry{Summary: libevidence.Summary{Head: timelineHead, VideoURL: "https://proof.example/video.mp4", VideoDuration: 160 * time.Second, Proven: 3, Total: 4}}}
			resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			got := onlySession(t, resp.Msg.Sessions).GetAttention()
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || got.GetSentence() != tc.sentence {
				t.Fatalf("attention = %v", got)
			}
		})
	}
}

type attentionCronStore struct {
	db.CronJobStore
	jobs  []*models.CronJob
	reads int
}

func (f *attentionCronStore) ListByRepo(context.Context, string) ([]*models.CronJob, error) {
	f.reads++
	return f.jobs, nil
}

type attentionSessionStore struct {
	db.SessionStore
	row *models.Session
}

func (f *attentionSessionStore) Get(context.Context, string) (*models.Session, error) {
	return f.row, nil
}

type attentionResumeStore struct {
	db.ResumeAttemptStore
	rows []*models.ResumeAttempt
	key  string
}

func (f *attentionResumeStore) ListForRun(_ context.Context, _, key string) ([]*models.ResumeAttempt, error) {
	f.key = key
	return f.rows, nil
}

func TestAttentionFactoryMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs []*models.CronJob
		kind pb.AttentionKind
	}{
		{"absent", nil, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"disabled", []*models.CronJob{{Prompt: "/boss-verify", IsEnabled: false}}, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"unrelated", []*models.CronJob{{Prompt: "/boss-build", IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"enabled", []*models.CronJob{{Prompt: "$boss-verify headless", IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			mergeable := true
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			crons := &attentionCronStore{jobs: tc.jobs}
			s.cronJobs = crons
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{HeadSHA: timelineHead, RawJSON: checksJSON(t, greenCI(2, ""))}}}
			// Use ordinary checks only: the fixture's pending verify status is not CI.
			for range 2 {
				resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
				if err != nil {
					t.Fatal(err)
				}
				if got := onlySession(t, resp.Msg.Sessions).GetAttention(); got.GetKind() != tc.kind {
					t.Fatalf("attention = %v", got)
				}
			}
			if crons.reads != 1 {
				t.Fatalf("factory reads=%d", crons.reads)
			}
		})
	}
}

func TestAttentionQuestionCache(t *testing.T) {
	row := &models.Session{ID: "s", RepoID: "r"}
	chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "a", AgentName: "claude"}
	tracker := status.NewTracker()
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_QUESTION, at(10))
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, map[string][]*models.AgentChat{"s": {chat}}, nil, tracker)
	s.sessions = &attentionSessionStore{row: row}
	client := &transcriptAgentClient{resp: &pb.ReadTranscriptResponse{Exists: true, FinalAssistantText: "Which option?"}}
	s.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	// Transcript resolution uses the chat row, just as the actual RPC does.
	s.agentChats = &chatStoreFake{chat: chat}
	for range 2 {
		p := SessionToProto(row)
		s.hydrateAttention(context.Background(), p, attentionDeps{row: row, chats: []*models.AgentChat{chat}})
		if got := p.GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_QUESTION || got.GetSentence() != "“Which option?”" || got.GetTargetChatId() != "a" {
			t.Fatalf("attention=%v", got)
		}
	}
	if client.calls != 1 {
		t.Fatalf("reads=%d", client.calls)
	}
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_WORKING, at(11))
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_QUESTION, at(12))
	client.resp.FinalAssistantText = "A new question?"
	p := SessionToProto(row)
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row, chats: []*models.AgentChat{chat}})
	if p.GetAttention().GetSentence() != "“A new question?”" || client.calls != 2 {
		t.Fatalf("new question=%v reads=%d", p.Attention, client.calls)
	}
}

func TestAttentionResumeDismissalAndRecovery(t *testing.T) {
	chatID := "run-a"
	row := &models.Session{ID: "s", RepoID: "r", AgentSessionID: &chatID}
	ledger := &attentionResumeStore{rows: []*models.ResumeAttempt{{AttemptedAt: at(10), FailureClass: "auth_expired"}, {AttemptedAt: at(12), FailureClass: "auth_expired"}}}
	s := &Server{resumeAttempts: ledger}
	p := SessionToProto(row)
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !strings.Contains(p.GetAttention().GetSentence(), "same 401") || ledger.key != chatID {
		t.Fatalf("attention=%v key=%s", p.Attention, ledger.key)
	}
	row.DismissedAlertKey = "resume:auth_expired"
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatal("dismissed alert still visible")
	}
	row.DismissedAlertKey = ""
	p.LastAgentActivityAt = timestamppb.New(at(15))
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatal("recovered run still needs fix")
	}
}

func TestAttention2986NoStaleAlert(t *testing.T) {
	n := 2986
	reason := "old repair failed"
	row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, BlockedReason: &reason, LastRepairAttemptCount: 5, LastRepairHeadSHA: "old", LastRepairStartedAt: timePtr(at(10))}
	tracker := status.NewDisplayTracker()
	tracker.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, HeadSHA: timelineHead, VerifyReason: "human-review:security"})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	chatID := "productive"
	chats := map[string][]*models.AgentChat{"s": {{SessionID: "s", AgentSessionID: chatID}}}
	s.agentChats = &listSessionsAgentChatStoreFake{chats: chats}
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update(chatID, pb.ChatStatus_CHAT_STATUS_WORKING, at(20))
	s.rotationEvents = fakeRotationEventStore{events: []db.RotationEvent{{SessionID: "s", CreatedAt: at(16), Trigger: pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(), Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String()}}}
	oldHead := "old"
	row.LastAttemptHeadSHA = &oldHead
	for _, state := range []machine.State{machine.ReadyForReview, machine.Blocked} {
		row.State = state
		resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := onlySession(t, resp.Msg.Sessions).GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
			t.Fatalf("state=%v attention=%v", state, got)
		}
	}
}
