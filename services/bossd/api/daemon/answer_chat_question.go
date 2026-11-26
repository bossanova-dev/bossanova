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

// ErrAnswerPartlyEntered stops an answer of several steps part way, after
// some of its keys were sent: the menu changed underneath, stopped changing,
// or wanted more steps than an answer takes.
var ErrAnswerPartlyEntered = errors.New("the answer was partly entered; check the terminal")

const (
	// answerConfirmWait bounds how long AnswerChatQuestion watches for the
	// agent to take the answer before calling it unconfirmed.
	answerConfirmWait = 5 * time.Second
	// answerConfirmPoll is how often it looks.
	answerConfirmPoll = 250 * time.Millisecond
	// maxAnswerSteps bounds the steps of one answer. A several-choice tab
	// takes at most its option count plus 4 (Claude Code offers at most 4
	// options), and a row past the digit keys takes 2.
	maxAnswerSteps = 12
	// answerStepWait bounds how long the daemon waits for the pane to change
	// and settle after a step before planning the next one.
	answerStepWait = 1500 * time.Millisecond
	// answerStepPoll is how often it captures the pane meanwhile.
	answerStepPoll = 100 * time.Millisecond
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
// An answer of several keys (ticking boxes, typing words, moving to Submit)
// is entered one step at a time: after each step that does not finish it,
// the daemon captures the pane once it has changed and settled, and plans the
// next step against that capture. A menu that stops being the question part
// way, stops changing, or wants more than maxAnswerSteps steps ends the
// answer with ErrAnswerPartlyEntered, FailedPrecondition.
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
	steps, err := enterAnswer(ctx, planner, pane, tmuxName, req, plan)
	if err != nil {
		return nil, err
	}
	answered := awaitAnswerTaken(ctx, planner, pane, tmuxName, req)
	s.logger.Info().
		Str("chat_id", chat.ID).
		Str("agent_session_id", chat.AgentSessionID).
		Str("session_id", chat.SessionID).
		Str("agent", agentName).
		Str("question_id", cmd.GetQuestionId()).
		Bool("is_answered", answered).
		Int("step_count", steps).
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
		if !isOneLine(answer.Text) || strings.TrimSpace(answer.Text) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a typed answer must be one line of words"))
		}
		req.Answer = &pb.PlanQuestionAnswerRequest_Text{Text: answer.Text}
	case *pb.AnswerChatQuestionCommand_Choices:
		choices := answer.Choices
		if !isOneLine(choices.GetText()) || (choices.GetText() != "" && strings.TrimSpace(choices.GetText()) == "") {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a typed answer must be one line of words"))
		}
		if len(choices.GetOptionIndexes()) == 0 && strings.TrimSpace(choices.GetText()) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a several-choice answer needs an option or words"))
		}
		req.Answer = &pb.PlanQuestionAnswerRequest_Choices{Choices: &pb.QuestionChoices{OptionIndexes: choices.GetOptionIndexes(), Text: choices.GetText()}}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("an option_index or text answer is required"))
	}
	return req, nil
}

// isOneLine reports whether text holds no line break or other control
// character, as the agent's one-line free-text row needs.
func isOneLine(text string) bool {
	return !strings.ContainsFunc(text, unicode.IsControl)
}

// enterAnswer sends plan's inputs and, until a step finishes the answer,
// waits for the pane to change after each step and plans the next step
// against what it shows. It returns how many steps it sent.
func enterAnswer(ctx context.Context, planner questionAnswerPlanner, pane ChatPane, tmuxName string, req *pb.PlanQuestionAnswerRequest, plan *pb.PlanQuestionAnswerResponse) (int, error) {
	for steps := 1; ; steps++ {
		if err := pane.SendPaneInputs(ctx, tmuxName, paneInputs(plan.GetInputs())); err != nil {
			return steps, connect.NewError(connect.CodeInternal, fmt.Errorf("send the answer: %w", err))
		}
		if isFinalStep(plan) {
			return steps, nil
		}
		if steps >= maxAnswerSteps {
			return steps, partlyEntered(fmt.Sprintf("the menu still wanted more after %d steps", steps))
		}
		shown, ok := awaitPaneChange(ctx, pane, tmuxName, string(req.GetPaneContent()))
		if !ok {
			return steps, partlyEntered("the menu did not change after a step")
		}
		next, err := planAgainst(ctx, planner, req, shown)
		if err != nil {
			return steps, err
		}
		if !next.GetIsQuestionPending() {
			return steps, partlyEntered("the menu changed before the answer was finished")
		}
		if len(next.GetInputs()) == 0 {
			return steps, partlyEntered(next.GetReason())
		}
		plan = next
	}
}

// isFinalStep reports whether plan's inputs finish the answer. A plugin from
// before answers took several steps leaves is_final_step unset, and its plan
// is the whole answer.
func isFinalStep(plan *pb.PlanQuestionAnswerResponse) bool {
	return plan.IsFinalStep == nil || plan.GetIsFinalStep()
}

func partlyEntered(why string) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: %s", ErrAnswerPartlyEntered, why))
}

// awaitPaneChange captures the pane until it differs from before and two
// captures in a row agree, so the next step is planned from a menu that
// finished redrawing. Past answerStepWait it settles for the last changed
// capture, or reports false when the pane never changed.
func awaitPaneChange(ctx context.Context, chatPane ChatPane, tmuxName, before string) (string, bool) {
	deadline := time.Now().Add(answerStepWait)
	last := before
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(answerStepPoll):
		}
		shown, err := chatPane.CapturePane(ctx, tmuxName)
		if err != nil {
			return "", false
		}
		if shown != before && shown == last {
			return shown, true
		}
		last = shown
		if !time.Now().Before(deadline) {
			return last, last != before
		}
	}
}

// planAnswer captures the pane and asks the plugin to plan req against it.
func planAnswer(ctx context.Context, planner questionAnswerPlanner, chatPane ChatPane, tmuxName string, req *pb.PlanQuestionAnswerRequest) (*pb.PlanQuestionAnswerResponse, error) {
	pane, err := chatPane.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: read the terminal: %w", ErrQuestionNotPending, err))
	}
	return planAgainst(ctx, planner, req, pane)
}

// planAgainst asks the plugin to plan req against the captured pane.
func planAgainst(ctx context.Context, planner questionAnswerPlanner, req *pb.PlanQuestionAnswerRequest, pane string) (*pb.PlanQuestionAnswerResponse, error) {
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
