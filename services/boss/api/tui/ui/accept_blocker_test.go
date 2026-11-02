package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/recurser/boss/internal/domain/prverdict"
	"github.com/recurser/boss/internal/domain/rpcerr"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

type blockerAcceptFake struct {
	listed  []string
	list    *pb.ListSessionPRBlockersResponse
	listErr error
	accepts []*pb.AcceptSessionPRBlockerRequest
	resp    *pb.AcceptSessionPRBlockerResponse
	respErr error
}

func (c *blockerAcceptFake) ListSessionPRBlockers(_ context.Context, id string) (*pb.ListSessionPRBlockersResponse, error) {
	c.listed = append(c.listed, id)
	return c.list, c.listErr
}

func (c *blockerAcceptFake) AcceptSessionPRBlocker(_ context.Context, req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	c.accepts = append(c.accepts, proto.CloneOf(req))
	return c.resp, c.respErr
}

const acceptTestHead = "abcdef0123456789abcdef0123456789abcdef01"

func twoBlockers() *pb.ListSessionPRBlockersResponse {
	return &pb.ListSessionPRBlockersResponse{HeadSha: acceptTestHead, Blockers: []*pb.VerifyBlocker{
		{Id: "F-1", Severity: "P1", FilePath: "services/boss/a.go", LineNumber: 12, Summary: "nil map write"},
		{Id: "F-2", Severity: "P2", FilePath: "docs/b.md", Summary: "stale link"},
	}}
}

func acceptedResp() *pb.AcceptSessionPRBlockerResponse {
	return &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, HeadSha: acceptTestHead, BlockerId: "F-2", TicketIdentifier: "BOS-9001", TicketUrl: "https://linear.test/BOS-9001", CommentUrl: "https://github.test/c/1"}
}

// run executes cmd and feeds a flow message back into the flow, returning the
// last message it produced.
func runFlow(t *testing.T, f AcceptBlockerFlow, cmd tea.Cmd) (AcceptBlockerFlow, tea.Msg) {
	t.Helper()
	var last tea.Msg
	for cmd != nil {
		msg := cmd()
		last = msg
		switch msg.(type) {
		case AcceptBlockersLoadedMsg, AcceptBlockerResultMsg:
			f, cmd = f.Update(msg)
		default:
			cmd = nil
		}
	}
	return f, last
}

func typeText(f AcceptBlockerFlow, text string) AcceptBlockerFlow {
	for _, r := range text {
		f, _ = f.Update(tea.KeyPressMsg{Code: r})
	}
	return f
}

var (
	enterKey = tea.KeyPressMsg{Code: tea.KeyEnter}
	escKey   = tea.KeyPressMsg{Code: tea.KeyEscape}
	downKey  = tea.KeyPressMsg{Code: tea.KeyDown}
)

// openAtReason drives a flow to the reason step on the second blocker.
func openAtReason(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
	t.Helper()
	f, cmd := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
	f, _ = runFlow(t, f, cmd)
	f, _ = f.Update(downKey)
	f, _ = f.Update(enterKey)
	if !f.TextEntryActive() {
		t.Fatal("enter on a blocker must open the reason step")
	}
	return f
}

// Proves chat-picker.AC213401.
func TestAcceptBlockerFlow_PicksReasonConfirmsPinnedToShownHead_ChatPicker_AC213401(t *testing.T) {
	c := &blockerAcceptFake{list: twoBlockers(), resp: acceptedResp()}
	f, cmd := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
	if !f.Active() || f.TextEntryActive() {
		t.Fatal("a started flow must be active and not in text entry")
	}
	f, _ = runFlow(t, f, cmd)
	if len(c.listed) != 1 || c.listed[0] != "s-1" {
		t.Fatalf("list calls = %v, want one for s-1", c.listed)
	}
	view := stripANSI(f.View(120, 40, ""))
	for _, want := range []string{"Accept a verify blocker · PR #42 at abcdef01", "SEVERITY", "LOCATION", "SUMMARY", "P1", "services/boss/a.go:12", "nil map write", "F-1", "docs/b.md", "stale link", "F-2", "[enter] select"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "docs/b.md:0") {
		t.Fatalf("a zero line must show the file alone:\n%s", view)
	}

	f, _ = f.Update(downKey)
	f, _ = f.Update(enterKey)
	if !f.TextEntryActive() {
		t.Fatal("enter must open the reason step")
	}
	f, _ = f.Update(enterKey)
	if view := stripANSI(f.View(120, 40, "")); !f.TextEntryActive() || !strings.Contains(view, "Reason cannot be empty") {
		t.Fatalf("an empty reason must keep the prompt open with its complaint:\n%s", view)
	}
	f = typeText(f, "  covered by BOS-1  ")
	if view := stripANSI(f.View(120, 40, "")); !strings.Contains(view, "covered by BOS-1") || strings.Contains(view, "Reason cannot be empty") {
		t.Fatalf("typing must fill the input and clear the complaint:\n%s", view)
	}
	f, _ = f.Update(enterKey)
	if view := stripANSI(f.View(120, 40, "")); f.TextEntryActive() || !strings.Contains(view, "Accept F-2 on PR #42 at abcdef01? Verify files a follow-up ticket") {
		t.Fatalf("enter must move to the confirmation:\n%s", view)
	}
	if len(c.accepts) != 0 {
		t.Fatal("nothing may be sent before the confirmation")
	}

	f, cmd = f.Update(keyPress('y'))
	if !f.Busy() || !strings.Contains(stripANSI(f.View(120, 40, "")), "Accepting F-2 on PR #42…") {
		t.Fatal("confirming must show the busy line")
	}
	f, last := runFlow(t, f, cmd)
	if len(c.accepts) != 1 {
		t.Fatalf("accept calls = %d, want 1", len(c.accepts))
	}
	got := c.accepts[0]
	if got.GetId() != "s-1" || got.GetBlockerId() != "F-2" || got.GetReason() != "covered by BOS-1" || got.GetExpectedHeadSha() != acceptTestHead || got.GetIsDryRun() {
		t.Fatalf("accept request = %v", got)
	}
	res, ok := last.(AcceptBlockerResultMsg)
	if !ok || res.Err != nil || res.SessionID != "s-1" || !strings.Contains(res.Text, "BOS-9001") {
		t.Fatalf("result = %#v, want the follow-up ticket", last)
	}
	if f.Active() {
		t.Fatal("the result must close the flow")
	}

	pasted, _ := openAtReason(t, &blockerAcceptFake{list: twoBlockers()}).Update(tea.PasteMsg{Content: "pasted why"})
	if !strings.Contains(stripANSI(pasted.View(120, 40, "")), "pasted why") {
		t.Fatal("a paste in the reason step must fill the input")
	}

	refusal := &pb.AcceptSessionPRBlockerResponse{Reason: "head-moved"}
	if _, err := AcceptOutcome(42, "F-2", refusal, nil); err == nil || err.Error() != prverdict.AcceptRefusal(42, "F-2", refusal) {
		t.Fatalf("refusal = %v", err)
	}
	if text, err := AcceptOutcome(42, "F-2", &pb.AcceptSessionPRBlockerResponse{IsAlreadyAccepted: true, TicketIdentifier: "BOS-9001"}, nil); err != nil || !strings.HasPrefix(text, "Already accepted") {
		t.Fatalf("already accepted = %q, %v", text, err)
	}
	unknown := rpcerr.New(rpcerr.DeadlineExceeded, errors.New("context deadline exceeded"))
	if _, err := AcceptOutcome(42, "F-2", nil, unknown); err == nil || !strings.Contains(err.Error(), "may already be recorded, and accepting it again is safe") {
		t.Fatalf("outcome unknown = %v", err)
	}
	old := rpcerr.New(rpcerr.Unimplemented, errors.New("unknown procedure"))
	if _, err := AcceptOutcome(42, "F-2", nil, old); err == nil || !strings.Contains(err.Error(), "the daemon is older than boss") {
		t.Fatalf("old daemon = %v", err)
	}
	hosted := rpcerr.New(rpcerr.Unimplemented, errors.New("AcceptSessionPRBlocker is only available on a local daemon"))
	if _, err := AcceptOutcome(42, "F-2", nil, hosted); err == nil || strings.Contains(err.Error(), "older") {
		t.Fatalf("hosted local-only = %v", err)
	}
}

// Proves chat-picker.AC213402.
func TestAcceptBlockerFlow_CancelAndEmptyListsSendNothing_ChatPicker_AC213402(t *testing.T) {
	cancels := []struct {
		name  string
		drive func(*testing.T, *blockerAcceptFake) AcceptBlockerFlow
	}{
		{"esc while loading", func(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
			f, _ := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
			f, _ = f.Update(escKey)
			return f
		}},
		{"esc while picking", func(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
			f, cmd := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
			f, _ = runFlow(t, f, cmd)
			f, _ = f.Update(escKey)
			return f
		}},
		{"esc in the reason step", func(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
			f := typeText(openAtReason(t, c), "why")
			f, _ = f.Update(escKey)
			return f
		}},
		{"esc at the confirmation", func(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
			f, _ := typeText(openAtReason(t, c), "why").Update(enterKey)
			f, _ = f.Update(escKey)
			return f
		}},
		{"n at the confirmation", func(t *testing.T, c *blockerAcceptFake) AcceptBlockerFlow {
			f, _ := typeText(openAtReason(t, c), "why").Update(enterKey)
			f, _ = f.Update(keyPress('n'))
			return f
		}},
	}
	for _, tc := range cancels {
		t.Run(tc.name, func(t *testing.T) {
			c := &blockerAcceptFake{list: twoBlockers()}
			if f := tc.drive(t, c); f.Active() || len(c.accepts) != 0 {
				t.Fatalf("active=%v accepts=%d, want idle and none", f.Active(), len(c.accepts))
			}
		})
	}

	t.Run("late reply after a cancel", func(t *testing.T) {
		c := &blockerAcceptFake{list: twoBlockers()}
		f, cmd := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
		f, _ = f.Update(escKey)
		if f, _ = f.Update(cmd()); f.Active() {
			t.Fatal("a late list reply reopened a cancelled picker")
		}
	})
	t.Run("stale token", func(t *testing.T) {
		c := &blockerAcceptFake{list: twoBlockers()}
		first, stale := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
		f, _ := first.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
		if f, _ = f.Update(stale()); f.TextEntryActive() || strings.Contains(stripANSI(f.View(120, 40, "")), "SEVERITY") {
			t.Fatal("a stale token's reply was taken")
		}
	})

	lists := []struct {
		name, want string
		resp       *pb.ListSessionPRBlockersResponse
		err        error
		failed     bool
	}{
		{name: "no-blockers", resp: &pb.ListSessionPRBlockersResponse{Reason: "no-blockers"}, want: "PR #42 has no standing verify blockers."},
		{name: "comments-unreadable", resp: &pb.ListSessionPRBlockersResponse{Reason: "comments-unreadable"}, want: "Couldn't read PR #42's verify comments.", failed: true},
		{name: "no-acceptable-blockers", resp: &pb.ListSessionPRBlockersResponse{Reason: "no-acceptable-blockers"}, want: "PR #42's blockers are checks, conflicts or notes, which cannot be accepted."},
		{name: "head-unknown", resp: &pb.ListSessionPRBlockersResponse{Reason: "head-unknown"}, want: "PR #42's latest commit is not known yet.", failed: true},
		{name: "unknown reason", resp: &pb.ListSessionPRBlockersResponse{Reason: "brand-new"}, want: "brand-new", failed: true},
		{name: "empty list", resp: &pb.ListSessionPRBlockersResponse{HeadSha: acceptTestHead}, want: "PR #42 has no standing verify blockers."},
		{name: "list error", err: errors.New("unavailable: daemon down"), want: "unavailable: daemon down", failed: true},
	}
	for _, tc := range lists {
		t.Run(tc.name, func(t *testing.T) {
			c := &blockerAcceptFake{list: tc.resp, listErr: tc.err}
			f, cmd := AcceptBlockerFlow{}.Start(t.Context(), c, AcceptBlockerTarget{SessionID: "s-1", PR: 42})
			f, last := runFlow(t, f, cmd)
			res, ok := last.(AcceptBlockerResultMsg)
			if !ok || f.Active() || len(c.accepts) != 0 {
				t.Fatalf("last=%#v active=%v accepts=%d", last, f.Active(), len(c.accepts))
			}
			line := res.Text
			if tc.failed {
				if res.Err == nil || res.Text != "" {
					t.Fatalf("want a failure line, got %#v", res)
				}
				line = res.Err.Error()
			}
			if line != tc.want {
				t.Fatalf("line = %q, want %q", line, tc.want)
			}
		})
	}
}

// Proves chat-picker.AC213402.
func TestCanAcceptBlocker_ChatPicker_AC213402(t *testing.T) {
	blocked := func(edit func(*pb.Session)) *pb.Session {
		s := &pb.Session{PrNumber: proto.Int32(42), PrDisplayHeadSha: acceptTestHead, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"repair-exhausted"}}}
		edit(s)
		return s
	}
	for _, tc := range []struct {
		name string
		s    *pb.Session
		want bool
	}{
		{"fix with park codes", blocked(func(*pb.Session) {}), true},
		{"failing with a head", &pb.Session{PrNumber: proto.Int32(42), PrDisplayHeadSha: acceptTestHead, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING}, true},
		{"no PR", blocked(func(s *pb.Session) { s.PrNumber = nil }), false},
		{"no head", blocked(func(s *pb.Session) { s.PrDisplayHeadSha = "" }), false},
		{"merged", blocked(func(s *pb.Session) { s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED }), false},
		{"closed", blocked(func(s *pb.Session) { s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CLOSED }), false},
		{"repair active", blocked(func(s *pb.Session) { s.RepairActive = true }), false},
		{"passing", &pb.Session{PrNumber: proto.Int32(42), PrDisplayHeadSha: acceptTestHead, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, false},
		{"sign-off review", blocked(func(s *pb.Session) { s.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_REVIEW }), false},
		{"fix without park codes", blocked(func(s *pb.Session) { s.Attention.ParkCodes = nil }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAcceptBlocker(tc.s); got != tc.want {
				t.Fatalf("CanAcceptBlocker = %v, want %v", got, tc.want)
			}
		})
	}
}
