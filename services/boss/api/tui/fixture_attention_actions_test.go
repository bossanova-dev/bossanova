package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func attentionSession(actions ...pb.AttentionAction) *pb.Session {
	n := int32(42)
	return &pb.Session{Id: "session-1", Title: "Needs review", PrNumber: &n, PrDisplayHeadSha: "rendered-head", Attention: &pb.SessionAttention{Actions: actions, ParkCodes: []string{"ledger-open"}, AlertKey: "resume:auth", TargetChatId: "target-chat"}}
}
func attentionHome(c *attentionStub, s *pb.Session) HomeModel {
	h := NewHomeModel(c, context.Background(), nil)
	h.Loading = false
	h.RepoCount = 1
	h.Width = 120
	h.Height = 30
	h.Sessions = []*pb.Session{s}
	h.BuildTableRows()
	return h
}
func (*attentionStub) GetSessionTimeline(context.Context, string) ([]*pb.TimelineStep, error) {
	return []*pb.TimelineStep{{Title: "Opened", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}, nil
}

// operatorMergeCall is one recorded OperatorMergeSession request.
type operatorMergeCall struct {
	id, head string
	approve  bool
	waives   []string
}
type attentionStub struct {
	routerClientFixture
	calls  []string
	head   string
	merges []operatorMergeCall
	// mergeResp / mergeErr answer OperatorMergeSession; a nil mergeResp
	// reports a landed merge with its bookkeeping done.
	mergeResp *pb.OperatorMergeSessionResponse
	mergeErr  error
}

func (s *attentionStub) OperatorMergeSession(_ context.Context, id, head string, approve bool, waives []string) (*pb.OperatorMergeSessionResponse, error) {
	s.calls = append(s.calls, "merge:"+id)
	s.head = head
	s.merges = append(s.merges, operatorMergeCall{id: id, head: head, approve: approve, waives: slices.Clone(waives)})
	if s.mergeErr != nil {
		return nil, s.mergeErr
	}
	if s.mergeResp != nil {
		return s.mergeResp, nil
	}
	return &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "ok"}, nil
}
func (s *attentionStub) DismissSessionAlert(_ context.Context, id, key string) (*pb.Session, error) {
	s.calls = append(s.calls, "dismiss:"+id+":"+key)
	return nil, nil
}
func (s *attentionStub) ResumeSessionRun(_ context.Context, id string) (*pb.Session, error) {
	s.calls = append(s.calls, "resume:"+id)
	return nil, nil
}
func attentionKey(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	if s == "esc" {
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return keyPress([]rune(s)[0])
}

// TestAttentionResumeAndDismissWorkUnadvertised: a blocked run's FIX offers
// resume and dismiss, and neither Home's footer nor the details action bar
// names them, yet [r] still asks to resume and [x] still asks to dismiss.
func parkSession(code string, openItems ...string) *pb.Session {
	n := int32(42)
	a := attention.Compute(attention.Input{ParkCodes: []string{code}, OpenItems: openItems, Evidence: &attention.Evidence{URL: "https://example.com/proof"}})
	return &pb.Session{
		Id: "session-1", Title: "Parked change", PrNumber: &n, PrDisplayHeadSha: renderedHead, Attention: a,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "needs human: " + code},
	}
}

// TestParkRowsShowNoCodesOrJargon renders every known park on Home, on the
// details view and, for a review row, in its merge confirmation: no raw park
// code and no "ledger" may reach the operator.
func assertMergeConfirm(t *testing.T, a ui.AttentionActionState) {
	t.Helper()
	if !a.Confirm.Active || a.Confirm.Prompt != "Merge PR #42?" {
		t.Fatalf("merge confirmation prompt = %q (active %v), want exactly %q", a.Confirm.Prompt, a.Confirm.Active, "Merge PR #42?")
	}
	var lines []string
	for _, line := range strings.Split(ansi.Strip(a.Footer(120, "")), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if want := []string{"Merge PR #42?", "[y/enter] confirm  [n/esc] cancel"}; !slices.Equal(lines, want) {
		t.Fatalf("merge confirmation renders %q, want %q", lines, want)
	}
}

// mergeLeftRunning confirms [m]erge on the details view and presses Esc before
// the merge answers. It returns the app, now on Home, and the merge command
// the operator walked away from.
func mergeRowReadsMerging(h HomeModel) bool {
	return strings.Contains(ansi.Strip(h.RenderSessionStatus(sessionByID(h.Sessions, "session-1"))), "merging")
}

// homeMergeRunning confirms [m]erge on Home's first row, with a second,
// quiet session below it, and returns Home and the merge command.
func homeMergeRunning(t *testing.T, c *attentionStub) (HomeModel, tea.Cmd) {
	t.Helper()
	h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE))
	other := &pb.Session{Id: "session-2", Title: "Quiet"}
	h.Sessions = append(h.Sessions, other)
	h.BuildTableRows()
	model, _ := h.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	if merge == nil {
		t.Fatal("confirming did not send the merge")
	}
	h = model.(HomeModel)
	if !mergeRowReadsMerging(h) {
		t.Fatalf("Home row = %q, want it to read merging while the merge runs", ansi.Strip(h.RenderSessionStatus(h.Sessions[0])))
	}
	return h, merge
}

// noAnswerErrors are the ways a merge call ends without bossd's answer: the
// client's own deadline, as the deadline interceptor words it, and a lost
// daemon.
var noAnswerErrors = map[string]error{
	"client deadline": fmt.Errorf("daemon at /tmp/bossd.sock accepted the connection but did not answer in time; check it with `boss daemon status` and recover it with `boss daemon restart`: %w",
		rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded))),
	"daemon lost": rpcerror.WrapError(connect.NewError(connect.CodeUnavailable, errors.New("dial unix /tmp/bossd.sock: connect: connection refused"))),
}

func sessionByID(sessions []*pb.Session, id string) *pb.Session {
	for _, s := range sessions {
		if s.GetId() == id {
			return s
		}
	}
	return nil
}
