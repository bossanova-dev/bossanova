package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// ChatPane reads a chat's terminal and sends it keys, for answering the
// question menu an agent waits on. The chat tmux client is one; without it
// every answer is refused.
type ChatPane interface {
	HasSession(ctx context.Context, name string) bool
	CapturePane(ctx context.Context, sessionName string) (string, error)
	SendPaneInputs(ctx context.Context, sessionName string, inputs []domain.PaneInput) error
}

type questionAnswerPlanner interface {
	PlanQuestionAnswer(context.Context, *pb.PlanQuestionAnswerRequest) (*pb.PlanQuestionAnswerResponse, error)
}

// ErrQuestionNotPending refuses an answer for a question the agent is no
// longer waiting on, or whose menu its terminal no longer shows.
var ErrQuestionNotPending = errors.New("the agent is no longer waiting on this question")

const (
	// answerConfirmWait bounds how long AnswerChatQuestion watches for the
	// agent to take the answer before calling it unconfirmed.
	answerConfirmWait = 5 * time.Second
	// answerConfirmPoll is how often it looks.
	answerConfirmPoll = 250 * time.Millisecond
)

// AnswerChatQuestion answers the question a chat's agent waits on in its
// terminal menu, with one of its options or with typed words.
//
// It deliberately does not go through SendChatMessage's delivery gate, which
// refuses any pane showing a menu (BOS-600) because a blind Enter into an
// unknown menu has unbounded effects. This path is not blind: the agent's
// plugin checks, against the pane captured just before, that its transcript
// still ends on the question question_id names and that the pane shows that
// question's menu with its options untouched, and only then plans the keys.
// A stale answer, a question answered in the terminal meanwhile, or a pane
// showing anything else types nothing and fails FailedPrecondition.
//
// The answer counts as taken once the plugin no longer finds the question
// pending; is_answered is false when the menu is still there at the end of
// answerConfirmWait.
func (s *Server) AnswerChatQuestion(ctx context.Context, cmd *pb.AnswerChatQuestionCommand) (*pb.AnswerChatQuestionResult, error) {
	req, err := planRequestFor(cmd)
	if err != nil {
		return nil, err
	}
	chat, sess, err := s.chatMessageTarget(ctx, cmd.GetAgentSessionId())
	if err != nil {
		return nil, err
	}
	if !domain.CloudRepoAllowed(ctx, sess.RepoID) {
		return nil, connect.NewError(connect.CodePermissionDenied, domain.ErrCloudTarget)
	}
	agentName := accountAgentName(chat.AgentName)
	planner, ok := s.agentClients[agentName].(questionAnswerPlanner)
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent %q cannot take an answer to its question from here; answer it in the terminal", agentName))
	}
	pane, ok := s.chatTmux.(ChatPane)
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("tmux not available"))
	}
	tmuxName := chatTmuxName(chat, sess.RepoID)
	if !pane.HasSession(ctx, tmuxName) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: the chat is not running", ErrQuestionNotPending))
	}
	req.WorkDir = sess.WorktreePath
	plan, err := planAnswer(ctx, planner, pane, tmuxName, req)
	if err != nil {
		return nil, err
	}
	if !plan.GetIsQuestionPending() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: %s", ErrQuestionNotPending, plan.GetReason()))
	}
	if len(plan.GetInputs()) == 0 {
		reason := plan.GetReason()
		if reason == "" {
			reason = "the agent's menu cannot take this answer; answer it in the terminal"
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(reason))
	}
	if err := s.assertCloudChatMessage(ctx, chat, sess); err != nil {
		return nil, err
	}
	if err := pane.SendPaneInputs(ctx, tmuxName, paneInputs(plan.GetInputs())); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send the answer: %w", err))
	}
	answered := awaitAnswerTaken(ctx, planner, pane, tmuxName, req)
	s.logger.Info().
		Str("chat_id", chat.ID).
		Str("agent_session_id", chat.AgentSessionID).
		Str("session_id", chat.SessionID).
		Str("agent", agentName).
		Str("question_id", cmd.GetQuestionId()).
		Bool("is_answered", answered).
		Msg("chat question answer")
	return &pb.AnswerChatQuestionResult{IsAnswered: answered}, nil
}

// planRequestFor validates cmd and turns it into the plugin's planning
// request, still missing the work dir and pane.
func planRequestFor(cmd *pb.AnswerChatQuestionCommand) (*pb.PlanQuestionAnswerRequest, error) {
	if cmd.GetAgentSessionId() == "" || cmd.GetQuestionId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_session_id and question_id are required"))
	}
	req := &pb.PlanQuestionAnswerRequest{AgentSessionId: cmd.GetAgentSessionId(), QuestionId: cmd.GetQuestionId()}
	switch answer := cmd.GetAnswer().(type) {
	case *pb.AnswerChatQuestionCommand_OptionIndex:
		req.Answer = &pb.PlanQuestionAnswerRequest_OptionIndex{OptionIndex: answer.OptionIndex}
	case *pb.AnswerChatQuestionCommand_Text:
		if strings.TrimSpace(answer.Text) == "" || strings.ContainsFunc(answer.Text, unicode.IsControl) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a typed answer must be one line of words"))
		}
		req.Answer = &pb.PlanQuestionAnswerRequest_Text{Text: answer.Text}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("an option_index or text answer is required"))
	}
	return req, nil
}

// planAnswer captures the pane and asks the plugin to plan req against it.
func planAnswer(ctx context.Context, planner questionAnswerPlanner, chatPane ChatPane, tmuxName string, req *pb.PlanQuestionAnswerRequest) (*pb.PlanQuestionAnswerResponse, error) {
	pane, err := chatPane.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: read the terminal: %w", ErrQuestionNotPending, err))
	}
	req.PaneContent = []byte(pane)
	plan, err := planner.PlanQuestionAnswer(ctx, req)
	if status.Code(err) == codes.Unimplemented {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the agent cannot take an answer to its question from here; answer it in the terminal"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("plan the answer: %w", err))
	}
	return plan, nil
}

// awaitAnswerTaken reports whether the agent stopped waiting on the question
// within answerConfirmWait.
func awaitAnswerTaken(ctx context.Context, planner questionAnswerPlanner, pane ChatPane, tmuxName string, req *pb.PlanQuestionAnswerRequest) bool {
	deadline := time.Now().Add(answerConfirmWait)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(answerConfirmPoll):
		}
		plan, err := planAnswer(ctx, planner, pane, tmuxName, req)
		if err == nil && !plan.GetIsQuestionPending() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

func paneInputs(planned []*pb.PaneInput) []domain.PaneInput {
	out := make([]domain.PaneInput, 0, len(planned))
	for _, in := range planned {
		out = append(out, domain.PaneInput{Key: in.GetKey(), Text: in.GetText()})
	}
	return out
}

// chatTmuxName is the chat's live tmux session: the name persisted on its row,
// else the deterministic one (see SendChatMessage).
func chatTmuxName(chat *domain.AgentChat, repoID string) string {
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		return *chat.TmuxSessionName
	}
	return domain.ChatSessionName(repoID, chat.AgentSessionID)
}
