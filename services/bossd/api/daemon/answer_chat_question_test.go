package server

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// fakeChatPane is a chat's terminal: whether it is live, what it shows, and
// what was sent to it.
type fakeChatPane struct {
	ChatTmuxSpawner
	live    bool
	pane    string
	sent    [][]domain.PaneInput
	sendErr error
}

func (p *fakeChatPane) HasSession(context.Context, string) bool { return p.live }
func (p *fakeChatPane) CapturePane(context.Context, string) (string, error) {
	return p.pane, nil
}
func (p *fakeChatPane) SendPaneInputs(_ context.Context, _ string, inputs []domain.PaneInput) error {
	p.sent = append(p.sent, inputs)
	return p.sendErr
}

// fakePlanner answers PlanQuestionAnswer from plans, one per call, repeating
// the last; it records the requests it was asked.
type fakePlanner struct {
	agent.RunnerClient
	plans []*pb.PlanQuestionAnswerResponse
	err   error
	asked []*pb.PlanQuestionAnswerRequest
}

func (f *fakePlanner) PlanQuestionAnswer(_ context.Context, req *pb.PlanQuestionAnswerRequest) (*pb.PlanQuestionAnswerResponse, error) {
	f.asked = append(f.asked, req)
	if f.err != nil {
		return nil, f.err
	}
	plan := f.plans[min(len(f.asked), len(f.plans))-1]
	return plan, nil
}

var answerKeyTwo = []*pb.PaneInput{{Input: &pb.PaneInput_Key{Key: "2"}}}

func pendingPlan(inputs ...*pb.PaneInput) *pb.PlanQuestionAnswerResponse {
	return &pb.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs}
}

func answeredPlan() *pb.PlanQuestionAnswerResponse {
	return &pb.PlanQuestionAnswerResponse{Reason: "the agent is no longer waiting on this question"}
}

func newAnswerTestServer(t *testing.T, planner agent.RunnerClient, pane *fakeChatPane) *Server {
	t.Helper()
	chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: "/wt/s1"}
	return &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.RunnerClient{"claude": planner},
		chatTmux:     pane,
	}
}

func optionAnswer(i uint32) *pb.AnswerChatQuestionCommand {
	return &pb.AnswerChatQuestionCommand{
		AgentSessionId: "agent-1", QuestionId: "toolu_1",
		Answer: &pb.AnswerChatQuestionCommand_OptionIndex{OptionIndex: i},
	}
}

func TestAnswerChatQuestion_SendsThePlannedKeysAndConfirmsTheAgentTookThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		planner := &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...), answeredPlan()}}
		pane := &fakeChatPane{live: true, pane: "the menu"}
		s := newAnswerTestServer(t, planner, pane)

		got, err := s.AnswerChatQuestion(t.Context(), optionAnswer(1))
		if err != nil {
			t.Fatalf("AnswerChatQuestion: %v", err)
		}
		if !got.GetIsAnswered() {
			t.Fatal("is_answered = false, want true once the plugin stops finding the question")
		}
		if len(pane.sent) != 1 || len(pane.sent[0]) != 1 || pane.sent[0][0] != (domain.PaneInput{Key: "2"}) {
			t.Fatalf("sent %v, want the planned key 2 once", pane.sent)
		}
		first := planner.asked[0]
		if first.GetQuestionId() != "toolu_1" || first.GetWorkDir() != "/wt/s1" || string(first.GetPaneContent()) != "the menu" || first.GetOptionIndex() != 1 {
			t.Fatalf("planned against %v, want the question, worktree, captured pane and option", first)
		}
	})
}

func TestAnswerChatQuestion_UnconfirmedWhenTheMenuStays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		planner := &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}
		pane := &fakeChatPane{live: true}
		s := newAnswerTestServer(t, planner, pane)

		got, err := s.AnswerChatQuestion(t.Context(), optionAnswer(1))
		if err != nil {
			t.Fatalf("AnswerChatQuestion: %v", err)
		}
		if got.GetIsAnswered() {
			t.Fatal("is_answered = true, want false while the menu is still there")
		}
		if len(pane.sent) != 1 {
			t.Fatalf("sent %d times, want once: an unconfirmed answer is never resent", len(pane.sent))
		}
	})
}

func TestAnswerChatQuestion_TypesNothingWhenRefused(t *testing.T) {
	cases := []struct {
		name    string
		planner *fakePlanner
		pane    *fakeChatPane
		cmd     *pb.AnswerChatQuestionCommand
		ctx     func(context.Context) context.Context
		want    connect.Code
	}{
		{"the question is stale", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{answeredPlan()}}, &fakeChatPane{live: true}, optionAnswer(0), nil, connect.CodeFailedPrecondition},
		{"the menu cannot take the answer", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{{IsQuestionPending: true, Reason: "several parts"}}}, &fakeChatPane{live: true}, optionAnswer(0), nil, connect.CodeFailedPrecondition},
		{"the chat is not running", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}, &fakeChatPane{live: false}, optionAnswer(0), nil, connect.CodeFailedPrecondition},
		{"the agent has no question menu", &fakePlanner{err: grpcstatus.Error(codes.Unimplemented, "no")}, &fakeChatPane{live: true}, optionAnswer(0), nil, connect.CodeFailedPrecondition},
		{"two lines of words", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}, &fakeChatPane{live: true}, &pb.AnswerChatQuestionCommand{AgentSessionId: "agent-1", QuestionId: "toolu_1", Answer: &pb.AnswerChatQuestionCommand_Text{Text: "a\nb"}}, nil, connect.CodeInvalidArgument},
		{"an escape byte in the words", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}, &fakeChatPane{live: true}, &pb.AnswerChatQuestionCommand{AgentSessionId: "agent-1", QuestionId: "toolu_1", Answer: &pb.AnswerChatQuestionCommand_Text{Text: "teal\x1b[2J"}}, nil, connect.CodeInvalidArgument},
		{"no question named", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}, &fakeChatPane{live: true}, &pb.AnswerChatQuestionCommand{AgentSessionId: "agent-1", Answer: &pb.AnswerChatQuestionCommand_OptionIndex{}}, nil, connect.CodeInvalidArgument},
		{"cloud control refuses the repository", &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(answerKeyTwo...)}}, &fakeChatPane{live: true}, optionAnswer(0), func(ctx context.Context) context.Context {
			return domain.WithCloudRepoCheck(domain.WithCloudTarget(ctx, domain.CloudTarget{"repo:r1": "r1"}), func(context.Context, string) bool { return false })
		}, connect.CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAnswerTestServer(t, tc.planner, tc.pane)
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			_, err := s.AnswerChatQuestion(ctx, tc.cmd)
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.want, err)
			}
			if len(tc.pane.sent) != 0 {
				t.Fatalf("sent %v, want nothing typed", tc.pane.sent)
			}
		})
	}
}

func TestAnswerChatQuestion_StaleRefusalNamesTheQuestion(t *testing.T) {
	s := newAnswerTestServer(t, &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{answeredPlan()}}, &fakeChatPane{live: true})
	_, err := s.AnswerChatQuestion(t.Context(), optionAnswer(0))
	if !errors.Is(err, ErrQuestionNotPending) {
		t.Fatalf("err = %v, want ErrQuestionNotPending", err)
	}
}

func TestAnswerChatQuestion_PermissionPromptObeysCloudControlAndDispatchesDigit(t *testing.T) {
	permCmd := &pb.AnswerChatQuestionCommand{
		AgentSessionId: "agent-1",
		QuestionId:     "perm-deadbeef01234567",
		Answer:         &pb.AnswerChatQuestionCommand_OptionIndex{OptionIndex: 0},
	}

	t.Run("dispatches planned digit when allowed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			keyOne := []*pb.PaneInput{{Input: &pb.PaneInput_Key{Key: "1"}}}
			planner := &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(keyOne...), answeredPlan()}}
			pane := &fakeChatPane{live: true, pane: "permission dialog"}
			s := newAnswerTestServer(t, planner, pane)

			got, err := s.AnswerChatQuestion(t.Context(), permCmd)
			if err != nil {
				t.Fatalf("AnswerChatQuestion: %v", err)
			}
			if !got.GetIsAnswered() {
				t.Fatal("is_answered = false, want true")
			}
			if len(pane.sent) != 1 || len(pane.sent[0]) != 1 || pane.sent[0][0] != (domain.PaneInput{Key: "1"}) {
				t.Fatalf("sent %v, want key 1 once", pane.sent)
			}
		})
	})

	t.Run("refuses when cloud control denies the repository", func(t *testing.T) {
		keyOne := []*pb.PaneInput{{Input: &pb.PaneInput_Key{Key: "1"}}}
		planner := &fakePlanner{plans: []*pb.PlanQuestionAnswerResponse{pendingPlan(keyOne...)}}
		pane := &fakeChatPane{live: true, pane: "permission dialog"}
		s := newAnswerTestServer(t, planner, pane)
		ctx := domain.WithCloudRepoCheck(
			domain.WithCloudTarget(t.Context(), domain.CloudTarget{"repo:r1": "r1"}),
			func(context.Context, string) bool { return false },
		)
		_, err := s.AnswerChatQuestion(ctx, permCmd)
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, err)
		}
		if len(pane.sent) != 0 {
			t.Fatalf("sent %v, want nothing typed", pane.sent)
		}
	})
}

func TestStopChat_EndsOnlyTheNamedChat(t *testing.T) {
	chat := &domain.AgentChat{ID: "c2", AgentSessionID: "agent-2", SessionID: "s1"}
	var stopped []string
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: &models.Session{ID: "s1", RepoID: "r1"}},
		wakeHook: wakeHook{stopChat: func(_ context.Context, sessionID, agentSessionID string) error {
			stopped = append(stopped, sessionID+"/"+agentSessionID)
			return nil
		}},
	}
	if err := s.StopChat(t.Context(), &pb.StopChatCommand{SessionId: "s1", AgentSessionId: "agent-2"}); err != nil {
		t.Fatalf("StopChat: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != "s1/agent-2" {
		t.Fatalf("stopped %v, want s1/agent-2 once", stopped)
	}
}

func TestStopChat_RefusesWithoutStopping(t *testing.T) {
	cases := []struct {
		name    string
		session string
		ctx     func(context.Context) context.Context
		want    connect.Code
	}{
		{"a chat of another session", "s9", nil, connect.CodeNotFound},
		{"cloud control refuses the repository", "s1", func(ctx context.Context) context.Context {
			return domain.WithCloudRepoCheck(domain.WithCloudTarget(ctx, domain.CloudTarget{"repo:r1": "r1"}), func(context.Context, string) bool { return false })
		}, connect.CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stops := 0
			s := &Server{
				agentChats: &chatStoreFake{chat: &domain.AgentChat{ID: "c2", AgentSessionID: "agent-2", SessionID: "s1"}},
				sessions:   &sessionStoreFake{sess: &models.Session{ID: "s1", RepoID: "r1"}},
				wakeHook:   wakeHook{stopChat: func(context.Context, string, string) error { stops++; return nil }},
			}
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			err := s.StopChat(ctx, &pb.StopChatCommand{SessionId: tc.session, AgentSessionId: "agent-2"})
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.want, err)
			}
			if stops != 0 {
				t.Fatalf("stopped %d times, want none", stops)
			}
		})
	}
}
