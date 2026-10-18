package views

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/vcs"
)

func attentionSession(actions ...pb.AttentionAction) *pb.Session {
	n := int32(42)
	return &pb.Session{Id: "session-1", Title: "Needs review", PrNumber: &n, PrDisplayHeadSha: "rendered-head", Attention: &pb.SessionAttention{Actions: actions, ParkCodes: []string{"ledger-open"}, AlertKey: "resume:auth", TargetChatId: "target-chat"}}
}

func attentionHome(c *attentionStub, s *pb.Session) HomeModel {
	h := NewHomeModel(c, context.Background(), nil)
	h.loading = false
	h.repoCount = 1
	h.width = 120
	h.height = 30
	h.sessions = []*pb.Session{s}
	h.buildTableRows()
	return h
}

// operatorMergeCall is one recorded OperatorMergeSession request.
type operatorMergeCall struct {
	id, head string
	approve  bool
	waives   []string
}

type attentionStub struct {
	chatPickerStub
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
			h = model.(HomeModel)
			if len(c.calls) != 0 {
				t.Fatal("write before confirmation")
			}
			if !strings.Contains(h.View().Content, "[y/enter]") {
				t.Fatal("missing confirmation")
			}
			model, cmd := h.Update(keyPress('n'))
			h = model.(HomeModel)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel dispatched write")
			}
			model, _ = h.Update(keyPress(tc.key))
			h = model.(HomeModel)
			// Poll data may change while confirming; merge still binds the rendered head.
			h.sessions = []*pb.Session{attentionSession(tc.action)}
			h.sessions[0].PrDisplayHeadSha = "new-head"
			model, cmd = h.Update(keyPress('y'))
			h = model.(HomeModel)
			if cmd == nil {
				t.Fatal("confirmation did not send request")
			}
			model, _ = h.Update(cmd())
			h = model.(HomeModel)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls = %v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("merge head = %q", c.head)
			}
			if h.status == "" {
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

func TestAttentionActionResumeAndRename(t *testing.T) {
	for _, details := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "details"}[details], func(t *testing.T) {
			c := &attentionStub{}
			s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME)
			var model tea.Model = attentionHome(c, s)
			if details {
				m := seedChatPicker(&c.chatPickerStub, statusStopped)
				m.client = c
				m.session = s
				model = m
			}
			model, cmd := model.Update(keyPress('r'))
			if cmd != nil || !strings.Contains(ansi.Strip(model.View().Content), "Resume this run?") {
				t.Fatal("[r] did not ask to resume")
			}
			model, cmd = model.Update(keyPress('y'))
			if cmd == nil {
				t.Fatal("resume not dispatched")
			}
			model, duplicate := model.Update(keyPress('r'))
			if duplicate != nil {
				t.Fatal("duplicate resume")
			}
			model, _ = model.Update(cmd())
			if len(c.calls) != 1 || c.calls[0] != "resume:session-1" {
				t.Fatalf("calls=%v", c.calls)
			}
			if !strings.Contains(model.View().Content, "Run resumed") {
				t.Fatal("missing resume status")
			}
			s.Attention = nil
			if h, ok := model.(HomeModel); ok {
				h.buildTableRows()
				row, _ := h.tableCursorForSessionID(s.GetId())
				h.table.SetCursor(row)
				model = h
			}
			model, _ = model.Update(keyPress('r'))
			switch m := model.(type) {
			case HomeModel:
				if !m.rename.Active() {
					t.Fatal("home rename lost")
				}
			case ChatPickerModel:
				if !m.renaming {
					t.Fatal("chat rename lost")
				}
			}
		})
	}
}

func TestAttentionActionDetailsConfirm(t *testing.T) {
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
			m := seedChatPicker(&c.chatPickerStub, statusStopped)
			m.client = c
			m.session = attentionSession(tc.action)
			model, _ := m.Update(keyPress(tc.key))
			m = model.(ChatPickerModel)
			if !strings.Contains(m.View().Content, "[y/enter]") {
				t.Fatal("missing confirmation")
			}
			model, cmd := m.Update(attentionKey("esc"))
			m = model.(ChatPickerModel)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel wrote")
			}
			model, _ = m.Update(keyPress(tc.key))
			m = model.(ChatPickerModel)
			model, cmd = m.Update(attentionKey("enter"))
			m = model.(ChatPickerModel)
			model, duplicate := m.Update(attentionKey("enter"))
			m = model.(ChatPickerModel)
			if duplicate != nil {
				t.Fatal("duplicate confirm")
			}
			if cmd == nil {
				t.Fatal("confirm did not dispatch")
			}
			model, _ = m.Update(cmd())
			m = model.(ChatPickerModel)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls=%v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("head=%q", c.head)
			}
			if m.statusNote == "" {
				t.Fatal("missing status")
			}
		})
	}
}

func TestAttentionActionNavigationAndAvailability(t *testing.T) {
	var opened []string
	original := openURLFunc
	openURLFunc = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { openURLFunc = original })
	for _, tc := range []struct {
		key        string
		detailsKey string
		action     pb.AttentionAction
		url        string
	}{
		{"g", "g", pb.AttentionAction_ATTENTION_ACTION_GITHUB, "https://github.com/org/repo/pull/42"},
		{"v", "v", pb.AttentionAction_ATTENTION_ACTION_VIDEO, "https://example.com/proof"},
		// Details keeps t for [t]erminal and opens the tracker from its own [l]inear.
		{"t", "l", pb.AttentionAction_ATTENTION_ACTION_TRACKER, "https://example.com/issue"},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			s := attentionSession(tc.action)
			prURL := "https://github.com/org/repo/pull/42"
			s.PrUrl = &prURL
			s.Attention.EvidenceUrl = "https://example.com/proof"
			issueURL := "https://example.com/issue"
			s.TrackerUrl = &issueURL
			h := attentionHome(&attentionStub{}, s)
			_, cmd := h.Update(attentionKey(tc.key))
			if cmd == nil {
				t.Fatal("missing navigation")
			}
			cmd()
			if len(opened) == 0 || opened[len(opened)-1] != tc.url {
				t.Fatalf("opened=%v", opened)
			}
			m := seedChatPicker(&chatPickerStub{}, statusStopped)
			m.session = s
			_, cmd = m.Update(attentionKey(tc.detailsKey))
			if cmd == nil {
				t.Fatal("missing details navigation")
			}
		})
	}
	for _, key := range []rune{'y', 'b', 'x', 'v'} {
		c := &attentionStub{}
		h := attentionHome(c, attentionSession())
		_, cmd := h.Update(keyPress(key))
		if cmd != nil || len(c.calls) > 0 {
			t.Fatalf("unavailable %c dispatched", key)
		}
		m := seedChatPicker(&c.chatPickerStub, statusStopped)
		m.client = c
		m.session = attentionSession()
		_, cmd = m.Update(keyPress(key))
		if cmd != nil || len(c.calls) > 0 {
			t.Fatalf("unavailable details %c dispatched", key)
		}
	}
}

func TestAttentionActionMissingMergeHeadRefuses(t *testing.T) {
	c := &attentionStub{}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	s.PrDisplayHeadSha = ""
	h := attentionHome(c, s)
	model, _ := h.Update(keyPress('m'))
	h = model.(HomeModel)
	model, cmd := h.Update(keyPress('y'))
	h = model.(HomeModel)
	model, _ = h.Update(cmd())
	h = model.(HomeModel)
	if len(c.calls) != 0 || !h.statusErr || h.status != "Couldn't merge PR #42: its latest commit is not known yet" {
		t.Fatalf("calls=%v status=%s", c.calls, h.status)
	}
}

func TestAttentionActionNavigationResultDoesNotUnlockResume(t *testing.T) {
	original := openURLFunc
	openURLFunc = func(string) error { return nil }
	t.Cleanup(func() { openURLFunc = original })
	c := &attentionStub{}
	h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_RESUME))
	model, open := h.Update(keyPress('g'))
	h = model.(HomeModel)
	model, _ = h.Update(keyPress('r'))
	h = model.(HomeModel)
	model, resume := h.Update(keyPress('y'))
	h = model.(HomeModel)
	model, _ = h.Update(open())
	h = model.(HomeModel)
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
		left, _, _ := h.sessionTableFooterActions()
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
func TestAttentionEnterOpensDetailsWithTheTargetChatSelected(t *testing.T) {
	for _, action := range []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, pb.AttentionAction_ATTENTION_ACTION_ANSWER} {
		t.Run(action.String(), func(t *testing.T) {
			c := &attentionStub{}
			h := attentionHome(c, attentionSession(action, pb.AttentionAction_ATTENTION_ACTION_RESUME))
			_, cmd := h.Update(attentionKey("enter"))
			if cmd == nil {
				t.Fatal("enter did nothing")
			}
			nav, ok := cmd().(switchViewMsg)
			if !ok || nav.view != ViewChatPicker || nav.sessionID != "session-1" {
				t.Fatalf("enter navigated to %#v, want the session's chat list", nav)
			}
			if len(c.calls) != 0 {
				t.Fatalf("enter dispatched %v", c.calls)
			}

			a := NewApp(c, nil)
			a.home = h
			model, _ := a.Update(nav)
			a = model.(App)
			if a.activeView != ViewChatPicker {
				t.Fatalf("activeView=%v, want the chat list", a.activeView)
			}
			now := time.Now()
			chat := func(id string, age time.Duration) *pb.ClaudeChat {
				return &pb.ClaudeChat{SessionId: "session-1", AgentSessionId: id, Title: id, CreatedAt: timestamppb.New(now.Add(-age))}
			}
			model, _ = a.Update(chatsListedMsg{
				chats:          []*pb.ClaudeChat{chat("newest-chat", time.Minute), chat("target-chat", time.Hour), chat("asking-chat", 2*time.Hour)},
				daemonStatuses: map[string]string{"newest-chat": statusWorking, "target-chat": statusStopped, "asking-chat": statusQuestion},
			})
			a = model.(App)
			if got := a.chatPicker.selectedChat().GetAgentSessionId(); got != "target-chat" {
				t.Fatalf("details selected %q, want target-chat", got)
			}

			_, cmd = a.Update(attentionKey("enter"))
			if cmd == nil {
				t.Fatal("second enter did nothing")
			}
			attach, ok := cmd().(switchViewMsg)
			if !ok || attach.view != ViewAttach || attach.sessionID != "session-1" || attach.resumeID != "target-chat" {
				t.Fatalf("second enter navigated to %#v, want target-chat attached", attach)
			}
		})
	}
}

// TestHomeEnterWithoutATargetChatLeavesTheDefaultSelection keeps the details
// view's own cursor choice for a row whose attention names no chat.
func TestHomeEnterWithoutATargetChatLeavesTheDefaultSelection(t *testing.T) {
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	s.Attention.TargetChatId = ""
	h := attentionHome(&attentionStub{}, s)
	_, cmd := h.Update(attentionKey("enter"))
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	if nav, ok := cmd().(switchViewMsg); !ok || nav.view != ViewChatPicker || nav.chatID != "" {
		t.Fatalf("enter navigated to %#v", nav)
	}
}

// TestAttentionResumeAndDismissWorkUnadvertised: a blocked run's FIX offers
// resume and dismiss, and neither Home's footer nor the details action bar
// names them, yet [r] still asks to resume and [x] still asks to dismiss.
func TestAttentionResumeAndDismissWorkUnadvertised(t *testing.T) {
	fix := func() *pb.Session {
		s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT)
		s.Attention.Kind, s.Attention.ParkCodes, s.Attention.Sentence = pb.AttentionKind_ATTENTION_KIND_FIX, nil, "Run blocked: MAD-1751 implemented"
		return s
	}
	screens := map[string]func(*attentionStub) tea.Model{
		"home":    func(c *attentionStub) tea.Model { return attentionHome(c, fix()) },
		"details": func(c *attentionStub) tea.Model { return attentionDetails(c, fix()) },
	}
	for name, open := range screens {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{}
			screen := ansi.Strip(open(c).View().Content)
			for _, hint := range []string{"[r]", "resume", "[x]", "dismiss"} {
				if strings.Contains(screen, hint) {
					t.Fatalf("%s advertises %q:\n%s", name, hint, screen)
				}
			}
			model, cmd := open(c).Update(keyPress('r'))
			if cmd != nil || !strings.Contains(ansi.Strip(model.View().Content), "Resume this run?") {
				t.Fatal("[r] did not ask to resume")
			}
			model, cmd = model.Update(keyPress('y'))
			if cmd == nil {
				t.Fatal("confirming did not resume")
			}
			model.Update(cmd())
			if !slices.Equal(c.calls, []string{"resume:session-1"}) {
				t.Fatalf("calls = %v", c.calls)
			}
			model, _ = open(c).Update(keyPress('x'))
			if !strings.Contains(ansi.Strip(model.View().Content), "[y/enter]") {
				t.Fatal("[x] did not ask to dismiss")
			}
		})
	}
}

// knownParkCodes is every operator park code verify can raise
// (skills-toolbox/verify-gate.mjs HUMAN_REASONS).
var knownParkCodes = []string{
	"always-human-path", "human-review:database", "human-review:security", "human-review:ui",
	"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session",
	"worktree-diverged", "worktree-unsynced", "human-review:unproven",
}

const renderedHead = "0123456789abcdef0123456789abcdef01234567"

// parkSession is a session parked by verify on code, as bossd serves it: the
// attention Compute answers, plus the legacy display fields older views read.
func parkSession(code string, openItems ...string) *pb.Session {
	n := int32(42)
	a := attention.Compute(attention.Input{ParkCodes: []string{code}, OpenItems: openItems, Evidence: &attention.Evidence{URL: "https://example.com/proof"}})
	return &pb.Session{
		Id: "session-1", Title: "Parked change", PrNumber: &n, PrDisplayHeadSha: renderedHead, Attention: a,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "needs human: " + code},
	}
}

func attentionDetails(c *attentionStub, s *pb.Session) ChatPickerModel {
	m := seedChatPicker(&c.chatPickerStub, statusStopped)
	m.client = c
	m.session = s
	m.timeline = []*pb.TimelineStep{{Title: "Opened", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return updated.(ChatPickerModel)
}

// TestAttentionReviewMergeRecordsTheApprovalAndMergesOnce drives [m] on a
// sign-off park's review row, on Home and on the details view: the footer
// offers merge and neither approve nor send back, the confirmation is a plain
// ok-to-merge with no mechanism, codes or repeated reason, and confirming sends
// exactly one approving merge pinned to the rendered head and waiving exactly
// the park.
func TestAttentionReviewMergeRecordsTheApprovalAndMergesOnce(t *testing.T) {
	s := parkSession("human-review:ui")
	if got, want := attentionFooter(s, false), []string{"[v] video", "[m] merge"}; !slices.Equal(got, want) {
		t.Fatalf("home footer = %q, want %q", got, want)
	}
	if got, want := attentionFooter(s, true), []string{"[g] github", "[v] video", "[m] merge"}; !slices.Equal(got, want) {
		t.Fatalf("details footer = %q, want %q", got, want)
	}
	for _, details := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "details"}[details], func(t *testing.T) {
			c := &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{
				IsMerged: true, TrackerWrites: "ok",
				Approval: &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:ui", HeadSha: renderedHead, ApprovalUrl: "https://x/9", TrackerWrites: "ok"},
			}}
			var model tea.Model = attentionHome(c, proto.CloneOf(s))
			if details {
				model = attentionDetails(c, proto.CloneOf(s))
			}
			for _, retired := range []rune{'y', 'b'} {
				if _, cmd := model.Update(keyPress(retired)); cmd != nil || len(c.calls) != 0 {
					t.Fatalf("[%c] dispatched on a review row: %v", retired, c.calls)
				}
			}
			model, _ = model.Update(keyPress('m'))
			switch m := model.(type) {
			case HomeModel:
				assertMergeConfirm(t, m.attentionAction)
			case ChatPickerModel:
				assertMergeConfirm(t, m.attentionAction)
			}
			model, cmd := model.Update(keyPress('y'))
			if cmd == nil {
				t.Fatal("confirmation did not send the merge")
			}
			if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, "Merging PR #42…") || strings.Contains(screen, "Requesting merge") {
				t.Fatalf("missing the merge progress line:\n%s", screen)
			}
			model, duplicate := model.Update(keyPress('y'))
			if duplicate != nil {
				t.Fatal("a second confirm sent a second merge")
			}
			model, _ = model.Update(cmd())
			want := operatorMergeCall{id: "session-1", head: renderedHead, approve: true, waives: []string{"human-review:ui"}}
			if len(c.merges) != 1 || c.merges[0].id != want.id || c.merges[0].head != want.head || !c.merges[0].approve || !slices.Equal(c.merges[0].waives, want.waives) {
				t.Fatalf("merges = %+v, want exactly %+v", c.merges, want)
			}
			if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, "Merged PR #42") || strings.Contains(screen, "Merging PR #42…") {
				t.Fatalf("missing the merge result line:\n%s", screen)
			}
		})
	}
}

// TestAttentionMergeRefusesAMovedHead lands both ways the daemon refuses a
// head the operator did not see: the approval refusing before it records, and
// the merge gate refusing a moved head. Each says so and merges nothing.
func TestAttentionMergeRefusesAMovedHead(t *testing.T) {
	for name, c := range map[string]*attentionStub{
		"approval":   {mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved", HeadSha: "ffffffffffffffffffffffffffffffffffffffff"}}},
		"merge gate": {mergeErr: connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: expected head %s, live head fff; nothing was approved or merged", vcs.ErrHeadMismatch, renderedHead))},
	} {
		t.Run(name, func(t *testing.T) {
			h := attentionHome(c, parkSession("human-review:ui"))
			model, _ := h.Update(keyPress('m'))
			model, cmd := model.Update(keyPress('y'))
			model, _ = model.Update(cmd())
			h = model.(HomeModel)
			if !h.statusErr || h.status != "PR #42 changed since you opened it, so nothing was merged" {
				t.Fatalf("status = %q (error %v), want the moved-head refusal", h.status, h.statusErr)
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
			assertMergeConfirm(t, model.(HomeModel).attentionAction)
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
func TestAttentionFixRowsOfferNoMerge(t *testing.T) {
	fault := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	fault.Attention.Kind, fault.Attention.ParkCodes = pb.AttentionKind_ATTENTION_KIND_FIX, nil
	rows := map[string]*pb.Session{"fault": fault}
	for _, code := range knownParkCodes {
		if s := parkSession(code); s.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
			rows[code] = s
		}
	}
	for name, s := range rows {
		t.Run(name, func(t *testing.T) {
			s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
			if slices.Contains(attentionFooter(s, true), "[m] merge") {
				t.Fatalf("footer offers merge: %q", attentionFooter(s, true))
			}
			c := &attentionStub{}
			if _, cmd := attentionHome(c, proto.CloneOf(s)).Update(keyPress('m')); cmd != nil {
				t.Fatal("[m] dispatched on a Home fix row")
			}
			m := attentionDetails(c, proto.CloneOf(s))
			updated, cmd := m.Update(keyPress('m'))
			m = updated.(ChatPickerModel)
			if cmd != nil || m.confirm != confirmNone || m.attentionAction.active() {
				t.Fatal("[m] raised a merge on a details fix row")
			}
			if screen := ansi.Strip(m.View().Content); strings.Contains(screen, "[m]erge") {
				t.Fatalf("details footer offers merge:\n%s", screen)
			}
			if len(c.calls) != 0 {
				t.Fatalf("calls = %v", c.calls)
			}
		})
	}
}

// TestParkRowsShowNoCodesOrJargon renders every known park on Home, on the
// details view and, for a review row, in its merge confirmation: no raw park
// code and no "ledger" may reach the operator.
func TestParkRowsShowNoCodesOrJargon(t *testing.T) {
	for _, code := range knownParkCodes {
		t.Run(code, func(t *testing.T) {
			var items []string
			if code == "ledger-open" {
				items = []string{"Open human follow-up: watch the rollout"}
			}
			s := parkSession(code, items...)
			c := &attentionStub{}
			home := attentionHome(c, proto.CloneOf(s))
			screens := map[string]string{"home": home.View().Content, "details": attentionDetails(c, proto.CloneOf(s)).View().Content}
			if s.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW {
				confirm, _ := home.Update(keyPress('m'))
				screens["merge confirmation"] = confirm.View().Content
			}
			for where, screen := range screens {
				plain := ansi.Strip(screen)
				if strings.Contains(strings.ToLower(plain), "ledger") {
					t.Errorf("%s shows \"ledger\":\n%s", where, plain)
				}
				for _, known := range knownParkCodes {
					if strings.Contains(plain, known) {
						t.Errorf("%s shows the park code %q:\n%s", where, known, plain)
					}
				}
			}
		})
	}
}

// TestBlockedParkDetailsListOpenItems opens the details view on an
// open-checklist park: it lists each open item its park note recorded and
// gives no advice.
func TestBlockedParkDetailsListOpenItems(t *testing.T) {
	m := attentionDetails(&attentionStub{}, parkSession("ledger-open", "Open human follow-up: watch the rollout", "Open question: which region first?"))
	screen := ansi.Strip(m.View().Content)
	for _, want := range []string{
		"The PR checklist has 2 open items",
		"Open in the PR checklist:",
		"• Open human follow-up: watch the rollout",
		"• Open question: which region first?",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("details view lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "boss pr approve") || strings.Contains(screen, "Open a chat") {
		t.Errorf("details view gives advice:\n%s", screen)
	}
	unread := attentionDetails(&attentionStub{}, parkSession("repair-exhausted"))
	if screen := ansi.Strip(unread.View().Content); !strings.Contains(screen, "Verify is blocked: repair exhausted.") {
		t.Errorf("a park with no open items does not say what blocks it:\n%s", screen)
	}
}

// assertMergeConfirm pins the [m]erge confirmation every row shows: exactly the
// question, then the shared confirm/cancel hints, with nothing about how the
// merge works or why the row is parked.
func assertMergeConfirm(t *testing.T, a attentionActionState) {
	t.Helper()
	if !a.confirm.active || a.confirm.prompt != "Merge PR #42?" {
		t.Fatalf("merge confirmation prompt = %q (active %v), want exactly %q", a.confirm.prompt, a.confirm.active, "Merge PR #42?")
	}
	var lines []string
	for _, line := range strings.Split(ansi.Strip(a.footer(120, "")), "\n") {
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
		{"gate: head moved", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: expected head a, live head b; nothing was approved or merged", vcs.ErrHeadMismatch))},
			"PR #42 changed since you opened it, so nothing was merged", true},
		{"gate: checks failing", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; 2 required checks are still failing; gated at head abc; nothing was approved or merged"))},
			"Couldn't merge PR #42: 2 required checks are still failing", true},
		{"gate after the approval landed", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=conflict; the PR has a merge conflict; gated at head abc; the approval of abc (https://x/9) stays recorded"))},
			"Couldn't merge PR #42: the PR has a merge conflict; your approval stays recorded, so verify may merge it once it passes", true},
		{"rpc error", &attentionStub{mergeErr: connect.NewError(connect.CodeInternal, errors.New("the tracker refused the write"))},
			"Couldn't merge PR #42: the tracker refused the write", true},
		{"uncoded refusal relayed by bosso", &attentionStub{mergeErr: connect.NewWireError(connect.CodeAborted, errors.New("merge PR: 405 At least 1 approving review is required"))},
			"Couldn't merge PR #42: merge PR: 405 At least 1 approving review is required", true},
		{"no answer from bossd", &attentionStub{mergeErr: connect.NewError(connect.CodeUnavailable, errors.New("dial unix: connection refused"))},
			"Waiting to hear whether PR #42 merged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := attentionHome(tc.stub, parkSession("human-review:ui"))
			model, _ := h.Update(keyPress('m'))
			model, cmd := model.Update(keyPress('y'))
			model, refresh := model.Update(cmd())
			h = model.(HomeModel)
			if h.status != tc.want || h.statusErr != tc.wantErr {
				t.Fatalf("result line = %q (error %v), want %q (error %v)", h.status, h.statusErr, tc.want, tc.wantErr)
			}
			if h.attentionAction.active() {
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
func mergeLeftRunning(t *testing.T, c *attentionStub) (tea.Model, tea.Cmd) {
	t.Helper()
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	if merge == nil {
		t.Fatal("confirming did not send the merge")
	}
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ViewHome {
		t.Fatalf("active view after Esc mid-merge = %v, want Home", got.activeView)
	}
	if row := ansi.Strip(got.home.renderSessionStatus(got.home.sessions[0])); !strings.Contains(row, "merging") {
		t.Fatalf("Home row = %q, want it to read merging while the merge runs", row)
	}
	return model, merge
}

func TestAttentionMergeEscLeavesAndHomeShowsTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "merged", want: "Merged PR #42"},
		{name: "refused", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; checks are still running; nothing was approved or merged")), want: "Couldn't merge PR #42: checks are still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &attentionStub{mergeErr: tc.err}
			model, merge := mergeLeftRunning(t, c)

			model, _ = model.Update(merge())
			got := model.(App)
			if len(c.merges) != 1 {
				t.Fatalf("merges = %+v, want the one merge the operator confirmed", c.merges)
			}
			if !strings.Contains(got.home.status, tc.want) || got.home.statusErr != (tc.err != nil) {
				t.Fatalf("Home status = %q (err %v), want %q", got.home.status, got.home.statusErr, tc.want)
			}
			if row := ansi.Strip(got.home.renderSessionStatus(got.home.sessions[0])); strings.Contains(row, "merging") {
				t.Fatalf("Home row = %q after the merge answered, want it to stop reading merging", row)
			}
		})
	}
}

func TestAttentionMergeOutcomeWaitsForTheOperatorsReturnToHome(t *testing.T) {
	c := &attentionStub{}
	model, merge := mergeLeftRunning(t, c)

	model, _ = model.Update(switchViewMsg{view: ViewChatPicker, sessionID: "session-1"})
	if footer := model.(App).chatPicker.attentionAction.footer(80, ""); !strings.Contains(footer, "Merging PR #42…") {
		t.Fatalf("reopened details view footer = %q, want it to say the merge is still running", footer)
	}
	model, _ = model.Update(attentionKey("esc"))
	model, _ = model.Update(switchViewMsg{view: ViewChatPicker, sessionID: "session-2"})
	model, _ = model.Update(merge())
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ViewHome || !strings.Contains(got.home.status, "Merged PR #42") {
		t.Fatalf("view %v, Home status %q: want the outcome that landed off screen waiting on Home", got.activeView, got.home.status)
	}
	if _, ok := got.home.mergeInFlight("session-1"); ok {
		t.Fatal("Home still holds the merge as running after it answered")
	}
}

// mergeRowReadsMerging reports whether Home's session-1 row reads merging.
func mergeRowReadsMerging(h HomeModel) bool {
	return strings.Contains(ansi.Strip(h.renderSessionStatus(sessionByID(h.sessions, "session-1"))), "merging")
}

// homeMergeRunning confirms [m]erge on Home's first row, with a second,
// quiet session below it, and returns Home and the merge command.
func homeMergeRunning(t *testing.T, c *attentionStub) (HomeModel, tea.Cmd) {
	t.Helper()
	h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE))
	other := &pb.Session{Id: "session-2", Title: "Quiet"}
	h.sessions = append(h.sessions, other)
	h.buildTableRows()
	model, _ := h.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	if merge == nil {
		t.Fatal("confirming did not send the merge")
	}
	h = model.(HomeModel)
	if !mergeRowReadsMerging(h) {
		t.Fatalf("Home row = %q, want it to read merging while the merge runs", ansi.Strip(h.renderSessionStatus(h.sessions[0])))
	}
	return h, merge
}

func TestAttentionMergeFromHomeLeavesHomeUsable(t *testing.T) {
	c := &attentionStub{}
	h, merge := homeMergeRunning(t, c)

	model, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	h = model.(HomeModel)
	if got := h.selectedSession().GetId(); got != "session-2" {
		t.Fatalf("selected session after down mid-merge = %q, want session-2", got)
	}
	_, open := h.Update(attentionKey("enter"))
	if open == nil {
		t.Fatal("enter mid-merge opened nothing")
	}
	if msg, ok := open().(switchViewMsg); !ok || msg.view != ViewChatPicker || msg.sessionID != "session-2" {
		t.Fatalf("enter mid-merge sent %#v, want the details view of session-2", msg)
	}
	if _, quit := h.Update(keyPress('q')); quit == nil {
		t.Fatal("q mid-merge did not quit")
	} else if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("q mid-merge did not quit")
	}

	model, _ = h.Update(merge())
	h = model.(HomeModel)
	if mergeRowReadsMerging(h) || !strings.Contains(h.status, "Merged PR #42") || len(c.merges) != 1 {
		t.Fatalf("after the merge answered: row merging %v, status %q, merges %d; want the outcome and one merge", mergeRowReadsMerging(h), h.status, len(c.merges))
	}
}

func TestAttentionMergeIsNotOfferedTwiceWhileItRuns(t *testing.T) {
	for name, running := range map[string]func(*testing.T, *attentionStub) (HomeModel, tea.Cmd){
		"started on Home": homeMergeRunning,
		"left running on the details view": func(t *testing.T, c *attentionStub) (HomeModel, tea.Cmd) {
			model, merge := mergeLeftRunning(t, c)
			var home tea.Model = model.(App).home
			home, _ = home.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
			home, _ = home.Update(sessionListMsg{sessions: []*pb.Session{attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)}})
			return home.(HomeModel), merge
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{}
			h, merge := running(t, c)
			h.table.SetCursor(0)
			if got := h.selectedSession().GetId(); got != "session-1" {
				t.Fatalf("selected session = %q, want the merging session-1", got)
			}
			if left, _, _ := h.sessionTableFooterActions(); slices.Contains(left, "[m] merge") {
				t.Fatalf("Home footer %q offers merge on a row that reads merging", left)
			}
			model, cmd := h.Update(keyPress('m'))
			model, confirm := model.Update(keyPress('y'))
			if cmd != nil || confirm != nil || model.(HomeModel).attentionAction.active() {
				t.Fatal("[m] on a row that reads merging started a second merge")
			}
			_ = merge()
			if len(c.merges) != 1 {
				t.Fatalf("merges = %+v, want only the merge already running", c.merges)
			}
		})
	}
}

func TestAttentionDismissLeftRunningIsNotShownAsMerging(t *testing.T) {
	c := &attentionStub{}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('x'))
	model, dismiss := model.Update(keyPress('y'))
	if dismiss == nil {
		t.Fatal("confirming did not send the dismiss")
	}
	model, _ = model.Update(attentionKey("esc"))
	if got := model.(App); got.activeView != ViewHome || mergeRowReadsMerging(got.home) {
		t.Fatalf("view %v, row merging %v after Esc mid-dismiss: want Home without a merging row", got.activeView, mergeRowReadsMerging(got.home))
	}
	model, _ = model.Update(dismiss())
	if got := model.(App); mergeRowReadsMerging(got.home) || len(got.home.mergesInFlight) != 0 {
		t.Fatalf("after the dismiss answered: row merging %v, merges held %v; want none", mergeRowReadsMerging(got.home), got.home.mergesInFlight)
	}
}

// noAnswerErrors are the ways a merge call ends without bossd's answer: the
// client's own deadline, as the deadline interceptor words it, and a lost
// daemon.
var noAnswerErrors = map[string]error{
	"client deadline": fmt.Errorf("daemon at /tmp/bossd.sock accepted the connection but did not answer in time; check it with `boss daemon status` and recover it with `boss daemon restart`: %w",
		connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)),
	"daemon lost": connect.NewError(connect.CodeUnavailable, errors.New("dial unix /tmp/bossd.sock: connect: connection refused")),
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
				h = model.(HomeModel)
				if h.status != "Waiting to hear whether PR #42 merged" || h.statusErr || refresh == nil {
					t.Fatalf("status %q (err %v), re-read %v: want the waiting line and a session re-read", h.status, h.statusErr, refresh != nil)
				}
				for _, banned := range []string{"Couldn't merge", "deadline", "daemon", "connection"} {
					if screen := ansi.Strip(h.View().Content); strings.Contains(screen, banned) {
						t.Fatalf("screen says %q after a merge with no answer:\n%s", banned, screen)
					}
				}

				stillMerging := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
				stillMerging.DisplayMerging = true
				model, _ = h.Update(sessionListMsg{sessions: []*pb.Session{stillMerging}})
				h = model.(HomeModel)
				if !mergeRowReadsMerging(h) || h.status != "Waiting to hear whether PR #42 merged" {
					t.Fatalf("row merging %v, status %q while bossd still merges: want it still merging", mergeRowReadsMerging(h), h.status)
				}

				settled := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
				tc.settled(settled)
				model, _ = h.Update(sessionListMsg{sessions: []*pb.Session{settled}})
				h = model.(HomeModel)
				if mergeRowReadsMerging(h) || h.status != tc.want || h.statusErr != tc.wantErr {
					t.Fatalf("row merging %v, status %q (err %v); want %q (err %v)", mergeRowReadsMerging(h), h.status, h.statusErr, tc.want, tc.wantErr)
				}
			})
		}
	}
}

func TestAttentionMergeWithNoAnswerOnTheDetailsViewSettlesOnHome(t *testing.T) {
	c := &attentionStub{mergeErr: noAnswerErrors["client deadline"]}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	model, _ = model.Update(merge())
	if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, "Waiting to hear whether PR #42 merged") || strings.Contains(screen, "deadline") || strings.Contains(screen, "Couldn't merge") {
		t.Fatalf("details view after a merge with no answer:\n%s", screen)
	}
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ViewHome || !mergeRowReadsMerging(got.home) {
		t.Fatalf("view %v, row merging %v: want Home showing the unanswered merge as merging", got.activeView, mergeRowReadsMerging(got.home))
	}
	merged := proto.CloneOf(s)
	merged.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	model, _ = model.Update(sessionListMsg{sessions: []*pb.Session{merged}})
	if got := model.(App); mergeRowReadsMerging(got.home) || got.home.status != "Merged PR #42" {
		t.Fatalf("after the poll: row merging %v, status %q; want the merge settled as merged", mergeRowReadsMerging(got.home), got.home.status)
	}
}

// TestMergeOutcomeUnknownTrustsOnlyAnAnswerBossdSent runs a real connect
// round trip: bossd's own DeadlineExceeded is an outcome, while the client
// giving up first is not. The hanging handler ignores its own deadline, as the
// detached merge does, so only the client can end that call.
func TestMergeOutcomeUnknownTrustsOnlyAnAnswerBossdSent(t *testing.T) {
	release := make(chan struct{})
	_, handler := bossanovav1connect.NewDaemonServiceHandler(operatorMergeServer{release: release})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer close(release)
	rpc := bossanovav1connect.NewDaemonServiceClient(srv.Client(), srv.URL)

	_, answered := rpc.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "answer"}))
	if connect.CodeOf(answered) != connect.CodeDeadlineExceeded || mergeOutcomeUnknown(answered) {
		t.Fatalf("bossd's own deadline answer (%v) classified as unknown = %v, want an outcome", answered, mergeOutcomeUnknown(answered))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, gaveUp := rpc.OperatorMergeSession(ctx, connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "hang"}))
	if !mergeOutcomeUnknown(gaveUp) {
		t.Fatalf("the client giving up (%v) classified as an outcome, want unknown", gaveUp)
	}
}

// TestAttentionMergeWithNoAnswerOffersNoActionOnTheDetailsView pins the details
// view to Home's rule: while a merge's outcome is not known it neither offers
// nor accepts an attention action or a second merge, whether the unanswered
// result landed on it or it was reopened from Home. A poll that still shows
// bossd merging keeps it so, and one that settles the merge gives them back.
func TestAttentionMergeWithNoAnswerOffersNoActionOnTheDetailsView(t *testing.T) {
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
	unanswered := map[string]func(*testing.T, *attentionStub) tea.Model{
		"answer lands on the details view": func(t *testing.T, c *attentionStub) tea.Model {
			a := NewApp(c, nil)
			a.home = attentionHome(c, proto.CloneOf(s))
			a.chatPicker = attentionDetails(c, proto.CloneOf(s))
			a.activeView = ViewChatPicker
			var model tea.Model = a
			if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, "[m]erge") {
				t.Fatalf("premise: details view offers no merge before the merge:\n%s", screen)
			}
			model, _ = model.Update(keyPress('m'))
			model, merge := model.Update(keyPress('y'))
			model, _ = model.Update(merge())
			return model
		},
		"reopened from Home": func(t *testing.T, c *attentionStub) tea.Model {
			a := NewApp(c, nil)
			a.home = attentionHome(c, proto.CloneOf(s))
			var model tea.Model = a
			model, _ = model.Update(keyPress('m'))
			model, merge := model.Update(keyPress('y'))
			model, _ = model.Update(merge())
			model, _ = model.Update(switchViewMsg{view: ViewChatPicker, sessionID: "session-1"})
			model, _ = model.Update(chatPickerSessionMsg{session: proto.CloneOf(s)})
			model, _ = model.Update(listedChats(1))
			if got := model.(App); got.activeView != ViewChatPicker {
				t.Fatalf("active view = %v, want the reopened details view", got.activeView)
			}
			return model
		},
	}
	for name, setup := range unanswered {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{mergeErr: noAnswerErrors["daemon lost"]}
			model := setup(t, c)
			assertNoActions := func(when string) {
				t.Helper()
				screen := ansi.Strip(model.View().Content)
				if !strings.Contains(screen, "Waiting to hear whether PR #42 merged") || strings.Contains(screen, "[m]erge") || strings.Contains(screen, "dismiss") {
					t.Fatalf("%s: want the waiting line and no merge or dismiss on offer:\n%s", when, screen)
				}
				for _, key := range []rune{'m', 'y', 'x', 'y'} {
					var cmd tea.Cmd
					model, cmd = model.Update(keyPress(key))
					got := model.(App).chatPicker
					if cmd != nil || got.attentionAction.active() || got.confirm != confirmNone {
						t.Fatalf("%s: %q started an action (cmd %v, attention %+v, confirm %v)", when, key, cmd != nil, got.attentionAction, got.confirm)
					}
				}
				if len(c.merges) != 1 {
					t.Fatalf("%s: merges = %+v, want only the unanswered one", when, c.merges)
				}
			}
			assertNoActions("after the unanswered merge")

			stillMerging := proto.CloneOf(s)
			stillMerging.DisplayMerging = true
			model, _ = model.Update(chatPickerRefreshMsg{session: stillMerging})
			assertNoActions("while bossd still merges")

			model, _ = model.Update(chatPickerRefreshMsg{session: proto.CloneOf(s)})
			screen := ansi.Strip(model.View().Content)
			if !strings.Contains(screen, "No answer came back about merging PR #42, and it has not merged") || strings.Contains(screen, "Waiting to hear") || !strings.Contains(screen, "[m]erge") {
				t.Fatalf("after a poll showing the merge ended: want the outcome and merge on offer again:\n%s", screen)
			}
			model, _ = model.Update(keyPress('m'))
			if got := model.(App).chatPicker.attentionAction; !got.confirm.active {
				t.Fatalf("[m] after the merge settled did not ask to merge: %+v", got)
			}
		})
	}
}

// TestAttentionMergeWithNoAnswerEndsWhenTheSessionLeavesTheList covers the poll
// that no longer lists the session, as when it archived after merging: the
// merge stops holding the session, and its waiting line goes with it unless a
// newer status has replaced it.
func TestAttentionMergeWithNoAnswerEndsWhenTheSessionLeavesTheList(t *testing.T) {
	for name, tc := range map[string]struct{ newer, want string }{
		"waiting line still shown": {want: ""},
		"newer status shown":       {newer: "Alert dismissed", want: "Alert dismissed"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{mergeErr: noAnswerErrors["daemon lost"]}
			h, merge := homeMergeRunning(t, c)
			model, _ := h.Update(merge())
			h = model.(HomeModel)
			if h.status != "Waiting to hear whether PR #42 merged" || !h.isMerging("session-1") {
				t.Fatalf("premise: status %q, merging %v; want the unanswered merge held", h.status, h.isMerging("session-1"))
			}
			if tc.newer != "" {
				h.status = tc.newer
			}
			model, _ = h.Update(sessionListMsg{sessions: []*pb.Session{{Id: "session-2", Title: "Quiet"}}})
			h = model.(HomeModel)
			if h.status != tc.want || h.statusErr || h.isMerging("session-1") || len(h.mergesInFlight) != 0 {
				t.Fatalf("after the poll without session-1: status %q (err %v), merging %v, held %v; want status %q and nothing held", h.status, h.statusErr, h.isMerging("session-1"), h.mergesInFlight, tc.want)
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
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("merge queue wait expired"))
	}
	<-s.release
	return connect.NewResponse(&pb.OperatorMergeSessionResponse{IsMerged: true}), nil
}
