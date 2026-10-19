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
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestAttentionSurvivesADaemonRestart serves 31 ready PRs before a restart
// and again from the statuses the display poller restores, before GitHub has
// been read. Every session must answer the same MERGE, since, head and label,
// so no client sees it go in progress and come back, or "✓ ready" turn
// "✓ passing" until its PR is read.
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
		live.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable, HasBuildReceipt: true})
		restored.Restore(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable, HasBuildReceipt: true}, polled)
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
		if b.GetDisplayLabel() != "✓ ready" || a.GetDisplayLabel() != b.GetDisplayLabel() {
			t.Fatalf("%s reads %q after the restart, %q before it", row.ID, a.GetDisplayLabel(), b.GetDisplayLabel())
		}
	}
}

// probingTranscriptClient is a claude runner whose agent last wrote at asked,
// in the transcript keyed by probeID ("a" when empty). Bookkeeping kept landing
// for five minutes after it, as pr-link and away_summary records do while a
// question waits. A zero asked answers that the runner cannot tell, as codex
// does.
type probingTranscriptClient struct {
	transcriptAgentClient
	asked   time.Time
	probeID string
}

func (c *probingTranscriptClient) ProbeProgressLiveness(_ context.Context, req *pb.ProbeProgressLivenessRequest) (*pb.ProbeProgressLivenessResponse, error) {
	id := c.probeID
	if id == "" {
		id = "a"
	}
	if c.asked.IsZero() || req.GetWorkDir() != "/wt" || req.GetAgentSessionId() != id {
		return &pb.ProbeProgressLivenessResponse{}, nil
	}
	return &pb.ProbeProgressLivenessResponse{
		LastProgressAt:         timestamppb.New(c.asked.Add(5 * time.Minute)),
		LastAssistantMessageAt: timestamppb.New(c.asked),
		IsKnown:                true,
	}, nil
}

// questionServer serves one session asking "Which option?" from chat through
// client, keeping one daemon's attention cache across serve calls.
type questionServer struct {
	t       *testing.T
	s       *Server
	row     *models.Session
	chat    *models.AgentChat
	entered time.Time
}

func newQuestionServer(t *testing.T, chat *models.AgentChat, client *probingTranscriptClient) *questionServer {
	entered := at(1)
	row := &models.Session{ID: "s", RepoID: "r", State: machine.ImplementingPlan, StateEnteredAt: &entered, UpdatedAt: entered, WorktreePath: "/wt"}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, map[string][]*models.AgentChat{"s": {chat}}, nil, nil)
	s.sessions = &attentionSessionStore{row: row}
	s.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	s.agentChats = &chatStoreFake{chat: chat}
	return &questionServer{t: t, s: s, row: row, chat: chat, entered: entered}
}

// serve answers the question's since while the pane last changed at
// paneChanged, which a restarted daemon re-seeds.
func (q *questionServer) serve(paneChanged time.Time) time.Time {
	q.t.Helper()
	tr := status.NewTracker()
	tr.Update(q.chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_QUESTION, paneChanged)
	q.s.chatStatus = tr
	p := SessionToProto(q.row)
	q.s.hydrateAttention(context.Background(), p, attentionDeps{row: q.row, chats: []*models.AgentChat{q.chat}})
	if p.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_QUESTION || p.GetAttention().GetSentence() != "“Which option?”" {
		q.t.Fatalf("attention = %v, want the question", p.GetAttention())
	}
	return p.GetAttention().GetSince().AsTime()
}

func askingClient(asked time.Time) *probingTranscriptClient {
	return &probingTranscriptClient{
		transcriptAgentClient: transcriptAgentClient{resp: &pb.ReadTranscriptResponse{Exists: true, FinalAssistantText: "Which option?"}},
		asked:                 asked,
	}
}

// TestQuestionAttentionIsDatedFromItsAsk asks the same words twice in one
// session state. Each question is dated from the agent's message that asked
// it, never from the bookkeeping written while it waits, so the two differ,
// while a restarted daemon, whose chat tracker re-seeds every pane time,
// serves the open question exactly as before.
func TestQuestionAttentionIsDatedFromItsAsk(t *testing.T) {
	chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "a", AgentName: "claude"}
	client := askingClient(at(10))

	if got := newQuestionServer(t, chat, client).serve(at(10)); !got.Equal(at(10)) {
		t.Fatalf("the question is dated %v, want its ask at %v, not the bookkeeping at %v", got, at(10), at(15))
	}
	if got := newQuestionServer(t, chat, client).serve(at(40)); !got.Equal(at(10)) {
		t.Fatalf("after a restart the open question is dated %v, before it %v", got, at(10))
	}

	client.asked = at(20)
	if got := newQuestionServer(t, chat, client).serve(at(20)); !got.Equal(at(20)) {
		t.Fatalf("the same words asked again are dated %v, want the second ask at %v", got, at(20))
	}

	client.asked = time.Time{}
	client.resp.Messages = []*pb.ChatMessage{{Role: "assistant", Text: "Which option?", Timestamp: at(30).Format(time.RFC3339Nano)}}
	if got := newQuestionServer(t, chat, client).serve(at(31)); !got.Equal(at(30)) {
		t.Fatalf("a runner that cannot probe dates the question %v, want its newest assistant message at %v", got, at(30))
	}

	client.resp.Messages = []*pb.ChatMessage{{Role: "user", Text: "pick one", Timestamp: at(33).Format(time.RFC3339Nano)}}
	q := newQuestionServer(t, chat, client)
	if got := q.serve(at(34)); !got.Equal(q.entered) {
		t.Fatalf("a question whose newest message is the operator's is dated %v, want the state's start %v", got, q.entered)
	}

	client.resp.Messages = nil
	q = newQuestionServer(t, chat, client)
	if got := q.serve(at(32)); !got.Equal(q.entered) {
		t.Fatalf("an undated question is dated %v, want the state's start %v", got, q.entered)
	}
}

// TestQuestionAttentionKeepsItsDateWhileCached serves an open question twice
// from one daemon with the pane unchanged. The second answer comes from the
// cache, so the date the first read found must come with it even though the
// transcript can no longer tell.
func TestQuestionAttentionKeepsItsDateWhileCached(t *testing.T) {
	chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "a", AgentName: "claude"}
	client := askingClient(at(10))
	q := newQuestionServer(t, chat, client)
	if got := q.serve(at(11)); !got.Equal(at(10)) {
		t.Fatalf("the question is dated %v, want its ask at %v", got, at(10))
	}
	client.asked = time.Time{}
	if got := q.serve(at(11)); !got.Equal(at(10)) {
		t.Fatalf("the cached question is dated %v, want the ask at %v it was read with", got, at(10))
	}
}

// TestQuestionAttentionProbesTheProviderSession dates a question from a chat
// whose runner keys its transcript by its own provider session id.
func TestQuestionAttentionProbesTheProviderSession(t *testing.T) {
	provider := "p"
	chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "a", AgentName: "claude", ProviderSessionID: &provider}
	client := askingClient(at(12))
	client.probeID = provider
	if got := newQuestionServer(t, chat, client).serve(at(13)); !got.Equal(at(12)) {
		t.Fatalf("the question is dated %v, want the ask at %v read from the provider session's transcript", got, at(12))
	}
}
