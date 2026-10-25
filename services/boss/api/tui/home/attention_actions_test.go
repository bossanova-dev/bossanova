package home

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/vcs"
)

func attentionSession(actions ...pb.AttentionAction) *pb.Session {
	n := int32(42)
	return &pb.Session{Id: "session-1", Title: "Needs review", PrNumber: &n, PrDisplayHeadSha: "rendered-head", Attention: &pb.SessionAttention{Actions: actions, ParkCodes: []string{"ledger-open"}, AlertKey: "resume:auth", TargetChatId: "target-chat"}}
}

func attentionHome(c *attentionStub, s *pb.Session) Model {
	h := NewModel(c, context.Background(), nil)
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

func TestAttentionActionConfirm(t *testing.T) {
	for _, tc := range []struct {
		key    rune
		action pb.AttentionAction
		call   string
	}{
		{'m', pb.AttentionAction_ATTENTION_ACTION_MERGE, "merge:session-1"},
		{'x', pb.AttentionAction_ATTENTION_ACTION_DISMISS, "dismiss:session-1:resume:auth"},
		{'r', pb.AttentionAction_ATTENTION_ACTION_RESUME, "resume:session-1"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			c := &attentionStub{}
			h := attentionHome(c, attentionSession(tc.action))
			model, _ := h.Update(keyPress(tc.key))
			h = model.(Model)
			if len(c.calls) != 0 {
				t.Fatal("write before confirmation")
			}
			if !strings.Contains(h.View().Content, "[y/enter]") {
				t.Fatal("missing confirmation")
			}
			model, cmd := h.Update(keyPress('n'))
			h = model.(Model)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel dispatched write")
			}
			model, _ = h.Update(keyPress(tc.key))
			h = model.(Model)
			// Poll data may change while confirming; merge still binds the rendered head.
			h.Sessions = []*pb.Session{attentionSession(tc.action)}
			h.Sessions[0].PrDisplayHeadSha = "new-head"
			model, cmd = h.Update(keyPress('y'))
			h = model.(Model)
			if cmd == nil {
				t.Fatal("confirmation did not send request")
			}
			model, _ = h.Update(cmd())
			h = model.(Model)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls = %v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("merge head = %q", c.head)
			}
			if h.Status == "" {
				t.Fatal("missing result status")
			}
		})
	}
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

func TestAttentionActionMissingMergeHeadRefuses(t *testing.T) {
	c := &attentionStub{}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	s.PrDisplayHeadSha = ""
	h := attentionHome(c, s)
	model, _ := h.Update(keyPress('m'))
	h = model.(Model)
	model, cmd := h.Update(keyPress('y'))
	h = model.(Model)
	model, _ = h.Update(cmd())
	h = model.(Model)
	if len(c.calls) != 0 || !h.StatusErr() || h.Status != "Couldn't merge PR #42: its latest commit is not known yet" {
		t.Fatalf("calls=%v status=%s", c.calls, h.Status)
	}
}

func TestAttentionActionNavigationResultDoesNotUnlockResume(t *testing.T) {
	original := OpenURLFunc
	OpenURLFunc = func(string) error { return nil }
	t.Cleanup(func() { OpenURLFunc = original })
	c := &attentionStub{}
	h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_RESUME))
	model, open := h.Update(keyPress('g'))
	h = model.(Model)
	model, _ = h.Update(keyPress('r'))
	h = model.(Model)
	model, resume := h.Update(keyPress('y'))
	h = model.(Model)
	model, _ = h.Update(open())
	h = model.(Model)
	_, duplicate := h.Update(keyPress('r'))
	if duplicate != nil {
		t.Fatal("navigation result unlocked in-flight resume")
	}
	if resume == nil {
		t.Fatal("resume not dispatched")
	}
}

func TestAttentionActionFooterShowsThePlainEnter(t *testing.T) {
	for _, action := range []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, pb.AttentionAction_ATTENTION_ACTION_ANSWER} {
		h := attentionHome(&attentionStub{}, attentionSession(action))
		left, _, _ := h.SessionTableFooterActions()
		if want := []string{"[n]ew session", "[enter] select", "[/] search"}; !slices.Equal(left, want) {
			t.Fatalf("%s footer=%v, want %v", action, left, want)
		}
	}
}

// TestAttentionEnterOpensDetailsWithTheTargetChatSelected walks a needs-you
// row through the app: Home's enter opens the session's chat list, not a chat,
// with the cursor on the chat the attention names (the failed chat on a fix
// row, the asking chat on a question row), and a second enter attaches it.
// The target is neither the newest chat nor the asking one the picker would
// otherwise land on.
func TestHomeEnterWithoutATargetChatLeavesTheDefaultSelection(t *testing.T) {
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	s.Attention.TargetChatId = ""
	h := attentionHome(&attentionStub{}, s)
	_, cmd := h.Update(attentionKey("enter"))
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	if nav, ok := cmd().(ui.SwitchViewMsg); !ok || nav.View != ui.ViewChatPicker || nav.ChatID != "" {
		t.Fatalf("enter navigated to %#v", nav)
	}
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

// TestAttentionReviewMergeRecordsTheApprovalAndMergesOnce drives [m] on a
// sign-off park's review row, on Home and on the details view: the footer
// offers merge and neither approve nor send back, the confirmation is a plain
// ok-to-merge with no mechanism, codes or repeated reason, and confirming sends
// exactly one approving merge pinned to the rendered head and waiving exactly
// the park.
func TestAttentionMergeRefusesAMovedHead(t *testing.T) {
	for name, c := range map[string]*attentionStub{
		"approval":   {mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved", HeadSha: "ffffffffffffffffffffffffffffffffffffffff"}}},
		"merge gate": {mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: expected head %s, live head fff; nothing was approved or merged", vcs.ErrHeadMismatch, renderedHead)))},
	} {
		t.Run(name, func(t *testing.T) {
			h := attentionHome(c, parkSession("human-review:ui"))
			model, _ := h.Update(keyPress('m'))
			model, cmd := model.Update(keyPress('y'))
			model, _ = model.Update(cmd())
			h = model.(Model)
			if !h.StatusErr() || h.Status != "PR #42 changed since you opened it, so nothing was merged" {
				t.Fatalf("status = %q (error %v), want the moved-head refusal", h.Status, h.StatusErr())
			}
			if len(c.merges) != 1 {
				t.Fatalf("merges = %d, want the one refused request", len(c.merges))
			}
		})
	}
}

// TestAttentionMergeRowMergesWithoutApproving checks a MERGE row asks plainly
// and records no approval, whether or not verify passed its head: an
// unverified MERGE is a repo with no running verify stage, where nothing asked
// for an approval record.
func TestAttentionMergeRowMergesWithoutApproving(t *testing.T) {
	mergeable := true
	n := int32(42)
	for name, verified := range map[string]bool{"verified": true, "unverified": false} {
		t.Run(name, func(t *testing.T) {
			s := &pb.Session{Id: "session-1", PrNumber: &n, PrDisplayHeadSha: renderedHead, Attention: attention.Compute(attention.Input{
				HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable, Verified: verified,
			})}
			if s.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE {
				t.Fatalf("kind = %v, want MERGE", s.GetAttention().GetKind())
			}
			c := &attentionStub{}
			model, _ := attentionHome(c, s).Update(keyPress('m'))
			assertMergeConfirm(t, model.(Model).AttentionAction)
			model, cmd := model.Update(keyPress('y'))
			model.Update(cmd())
			if len(c.merges) != 1 || c.merges[0].approve || len(c.merges[0].waives) != 0 || c.merges[0].head != renderedHead {
				t.Fatalf("merges = %+v, want one plain merge of the rendered head", c.merges)
			}
		})
	}
}

// TestAttentionFixRowsOfferNoMerge checks that no fix row, a blocked park or a
// fault, offers [m] on Home or the details view, even when its PR reads green.

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

// TestAttentionMergeAlwaysEndsOnAResultLine runs [m] on a review row through
// every way the merge can end and checks the spinner gives way to one
// persistent result line: "Merged PR #42" in the success colour, or a plain
// reason in the danger colour. A merge that lands also re-reads the session
// list, so the merged row leaves NEEDS YOU.
func TestAttentionMergeAlwaysEndsOnAResultLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stub    *attentionStub
		want    string
		wantErr bool
	}{
		{"merged", &attentionStub{}, "Merged PR #42", false},
		{"merged without bookkeeping", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "unavailable", TrackerWhy: "no-adapter"}},
			"Merged PR #42; the tracker was not updated because no tracker is set up", false},
		{"merged, tracker write retrying", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "retrying", TrackerWhy: "pr-unreadable"}},
			"Merged PR #42; the ticket update will be retried", false},
		{"approval refused, head moved", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved"}}},
			"PR #42 changed since you opened it, so nothing was merged", true},
		{"approval refused, park changed", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "park-changed"}}},
			"Couldn't merge PR #42: what needs your sign-off changed since you opened it, so nothing was merged", true},
		{"gate: head moved", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: expected head a, live head b; nothing was approved or merged", vcs.ErrHeadMismatch)))},
			"PR #42 changed since you opened it, so nothing was merged", true},
		{"gate: checks failing", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; 2 required checks are still failing; gated at head abc; nothing was approved or merged")))},
			"Couldn't merge PR #42: 2 required checks are still failing", true},
		{"gate after the approval landed", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=conflict; the PR has a merge conflict; gated at head abc; the approval of abc (https://x/9) stays recorded")))},
			"Couldn't merge PR #42: the PR has a merge conflict; your approval stays recorded, so verify may merge it once it passes", true},
		{"rpc error", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeInternal, errors.New("the tracker refused the write")))},
			"Couldn't merge PR #42: the tracker refused the write", true},
		{"uncoded refusal relayed by bosso", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewWireError(connect.CodeAborted, errors.New("merge PR: 405 At least 1 approving review is required")))},
			"Couldn't merge PR #42: merge PR: 405 At least 1 approving review is required", true},
		{"daemon too old, relayed by bosso", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewWireError(connect.CodeUnimplemented, errors.New("boss on mac-mini is too old to do this from here")))},
			"Couldn't merge PR #42: boss on mac-mini is too old to do this from here", true},
		{"no answer from bossd", &attentionStub{mergeErr: rpcerror.WrapError(connect.NewError(connect.CodeUnavailable, errors.New("dial unix: connection refused")))},
			"Waiting to hear whether PR #42 merged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := attentionHome(tc.stub, parkSession("human-review:ui"))
			model, _ := h.Update(keyPress('m'))
			model, cmd := model.Update(keyPress('y'))
			model, refresh := model.Update(cmd())
			h = model.(Model)
			if h.Status != tc.want || h.StatusErr() != tc.wantErr {
				t.Fatalf("result line = %q (error %v), want %q (error %v)", h.Status, h.StatusErr(), tc.want, tc.wantErr)
			}
			if h.AttentionAction.Active() {
				t.Fatal("the merge is still showing its spinner after the result landed")
			}
			screen := ansi.Strip(h.View().Content)
			if !strings.Contains(screen, tc.want[:min(len(tc.want), 40)]) || strings.Contains(screen, "Merging PR #42…") {
				t.Fatalf("the result line is not on screen:\n%s", screen)
			}
			if (refresh != nil) != !tc.wantErr {
				t.Fatalf("session re-read after the merge = %v, want %v", refresh != nil, !tc.wantErr)
			}
		})
	}
}

// mergeLeftRunning confirms [m]erge on the details view and presses Esc before
// the merge answers. It returns the app, now on Home, and the merge command
// the operator walked away from.
func mergeRowReadsMerging(h Model) bool {
	return strings.Contains(ansi.Strip(h.RenderSessionStatus(sessionByID(h.Sessions, "session-1"))), "merging")
}

// homeMergeRunning confirms [m]erge on Home's first row, with a second,
// quiet session below it, and returns Home and the merge command.
func homeMergeRunning(t *testing.T, c *attentionStub) (Model, tea.Cmd) {
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
	h = model.(Model)
	if !mergeRowReadsMerging(h) {
		t.Fatalf("Home row = %q, want it to read merging while the merge runs", ansi.Strip(h.RenderSessionStatus(h.Sessions[0])))
	}
	return h, merge
}

func TestAttentionMergeFromHomeLeavesHomeUsable(t *testing.T) {
	c := &attentionStub{}
	h, merge := homeMergeRunning(t, c)

	model, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	h = model.(Model)
	if got := h.SelectedSession().GetId(); got != "session-2" {
		t.Fatalf("selected session after down mid-merge = %q, want session-2", got)
	}
	_, open := h.Update(attentionKey("enter"))
	if open == nil {
		t.Fatal("enter mid-merge opened nothing")
	}
	if msg, ok := open().(ui.SwitchViewMsg); !ok || msg.View != ui.ViewChatPicker || msg.SessionID != "session-2" {
		t.Fatalf("enter mid-merge sent %#v, want the details view of session-2", msg)
	}
	if _, quit := h.Update(keyPress('q')); quit == nil {
		t.Fatal("q mid-merge did not quit")
	} else if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("q mid-merge did not quit")
	}

	model, _ = h.Update(merge())
	h = model.(Model)
	if mergeRowReadsMerging(h) || !strings.Contains(h.Status, "Merged PR #42") || len(c.merges) != 1 {
		t.Fatalf("after the merge answered: row merging %v, status %q, merges %d; want the outcome and one merge", mergeRowReadsMerging(h), h.Status, len(c.merges))
	}
}

func TestOngoingStatusIsOnlyAnUnresolvedMergesLine(t *testing.T) {
	merge, dismiss := pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_DISMISS
	for _, tc := range []struct {
		name  string
		state ui.AttentionActionState
		want  string
	}{
		{"merge running", ui.AttentionActionState{Action: merge, Busy: true, Progress: "Merging PR #42…"}, "Merging PR #42…"},
		{"merge unanswered", ui.AwaitingMerge("session-1", "Waiting to hear whether PR #42 merged"), "Waiting to hear whether PR #42 merged"},
		{"merge answered", ui.AttentionActionState{Action: merge, Progress: "Merging PR #42…"}, ""},
		{"dismiss running", ui.AttentionActionState{Action: dismiss, Busy: true, Progress: "Dismissing…"}, ""},
	} {
		if got := tc.state.OngoingStatus(); got != tc.want {
			t.Errorf("%s: ongoing status %q, want %q", tc.name, got, tc.want)
		}
	}
}

// noAnswerErrors are the ways a merge call ends without bossd's answer: the
// client's own deadline, as the deadline interceptor words it, and a lost
// daemon.
var noAnswerErrors = map[string]error{
	"client deadline": fmt.Errorf("daemon at /tmp/bossd.sock accepted the connection but did not answer in time; check it with `boss daemon status` and recover it with `boss daemon restart`: %w",
		rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded))),
	"daemon lost": rpcerror.WrapError(connect.NewError(connect.CodeUnavailable, errors.New("dial unix /tmp/bossd.sock: connect: connection refused"))),
}

func TestAttentionMergeWithNoAnswerReadsMergingUntilAPollSettlesIt(t *testing.T) {
	for errName, noAnswer := range noAnswerErrors {
		for _, tc := range []struct {
			name    string
			settled func(*pb.Session)
			want    string
			wantErr bool
		}{
			{"merged", func(s *pb.Session) { s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED }, "Merged PR #42", false},
			{"closed", func(s *pb.Session) { s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CLOSED }, "PR #42 was closed without merging", true},
			{"no longer merging", func(*pb.Session) {}, "No answer came back about merging PR #42, and it has not merged", true},
		} {
			t.Run(errName+"/"+tc.name, func(t *testing.T) {
				c := &attentionStub{mergeErr: noAnswer}
				h, merge := homeMergeRunning(t, c)
				model, refresh := h.Update(merge())
				h = model.(Model)
				if h.Status != "Waiting to hear whether PR #42 merged" || h.StatusErr() || refresh == nil {
					t.Fatalf("status %q (err %v), re-read %v: want the waiting line and a session re-read", h.Status, h.StatusErr(), refresh != nil)
				}
				for _, banned := range []string{"Couldn't merge", "deadline", "daemon", "connection"} {
					if screen := ansi.Strip(h.View().Content); strings.Contains(screen, banned) {
						t.Fatalf("screen says %q after a merge with no answer:\n%s", banned, screen)
					}
				}

				stillMerging := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
				stillMerging.DisplayMerging = true
				model, _ = h.Update(SessionListMsg{Sessions: []*pb.Session{stillMerging}})
				h = model.(Model)
				if !mergeRowReadsMerging(h) || h.Status != "Waiting to hear whether PR #42 merged" {
					t.Fatalf("row merging %v, status %q while bossd still merges: want it still merging", mergeRowReadsMerging(h), h.Status)
				}

				settled := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
				tc.settled(settled)
				model, _ = h.Update(SessionListMsg{Sessions: []*pb.Session{settled}})
				h = model.(Model)
				if mergeRowReadsMerging(h) || h.Status != tc.want || h.StatusErr() != tc.wantErr {
					t.Fatalf("row merging %v, status %q (err %v); want %q (err %v)", mergeRowReadsMerging(h), h.Status, h.StatusErr(), tc.want, tc.wantErr)
				}
			})
		}
	}
}

func TestMergeOutcomeUnknownTrustsOnlyAnAnswerBossdSent(t *testing.T) {
	release := make(chan struct{})
	_, handler := bossanovav1connect.NewDaemonServiceHandler(operatorMergeServer{release: release})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer close(release)
	rpc := bossanovav1connect.NewDaemonServiceClient(srv.Client(), srv.URL)

	_, answered := rpc.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "answer"}))
	if connect.CodeOf(answered) != connect.CodeDeadlineExceeded || ui.MergeOutcomeUnknown(rpcerror.WrapError(answered)) {
		t.Fatalf("bossd's own deadline answer (%v) classified as unknown = %v, want an outcome", answered, ui.MergeOutcomeUnknown(rpcerror.WrapError(answered)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, gaveUp := rpc.OperatorMergeSession(ctx, connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "hang"}))
	if !ui.MergeOutcomeUnknown(rpcerror.WrapError(gaveUp)) {
		t.Fatalf("the client giving up (%v) classified as an outcome, want unknown", gaveUp)
	}
}

// TestAttentionMergeWithNoAnswerOffersNoActionOnTheDetailsView pins the details
// view to Home's rule: while a merge's outcome is not known it neither offers
// nor accepts an attention action or a second merge, whether the unanswered
// result landed on it or it was reopened from Home. A poll that still shows
// bossd merging keeps it so, and one that settles the merge gives them back.
func TestAttentionMergeWithNoAnswerEndsWhenTheSessionLeavesTheList(t *testing.T) {
	for name, tc := range map[string]struct{ newer, want string }{
		"waiting line still shown": {want: ""},
		"newer status shown":       {newer: "Alert dismissed", want: "Alert dismissed"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{mergeErr: noAnswerErrors["daemon lost"]}
			h, merge := homeMergeRunning(t, c)
			model, _ := h.Update(merge())
			h = model.(Model)
			if h.Status != "Waiting to hear whether PR #42 merged" || !h.isMerging("session-1") {
				t.Fatalf("premise: status %q, merging %v; want the unanswered merge held", h.Status, h.isMerging("session-1"))
			}
			if tc.newer != "" {
				h.Status = tc.newer
			}
			model, _ = h.Update(SessionListMsg{Sessions: []*pb.Session{{Id: "session-2", Title: "Quiet"}}})
			h = model.(Model)
			if h.Status != tc.want || h.StatusErr() || h.isMerging("session-1") || len(h.MergesInFlight) != 0 {
				t.Fatalf("after the poll without session-1: status %q (err %v), merging %v, held %v; want status %q and nothing held", h.Status, h.StatusErr(), h.isMerging("session-1"), h.MergesInFlight, tc.want)
			}
			if screen := ansi.Strip(h.View().Content); strings.Contains(screen, "Waiting to hear") {
				t.Fatalf("Home still shows the waiting line:\n%s", screen)
			}
		})
	}
}

type operatorMergeServer struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	release chan struct{}
}

func (s operatorMergeServer) OperatorMergeSession(_ context.Context, req *connect.Request[pb.OperatorMergeSessionRequest]) (*connect.Response[pb.OperatorMergeSessionResponse], error) {
	if req.Msg.GetId() == "answer" {
		return nil, rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, errors.New("merge queue wait expired")))
	}
	<-s.release
	return connect.NewResponse(&pb.OperatorMergeSessionResponse{IsMerged: true}), nil
}
