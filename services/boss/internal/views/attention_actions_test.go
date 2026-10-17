package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
		{"enter", "enter", pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, ""},
		{"enter", "enter", pb.AttentionAction_ATTENTION_ACTION_ANSWER, ""},
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
			msg := cmd()
			if tc.key == "enter" {
				nav, ok := msg.(switchViewMsg)
				if !ok || nav.sessionID != "session-1" || nav.resumeID != "target-chat" || nav.view != ViewAttach {
					t.Fatalf("navigation=%#v", msg)
				}
			} else if len(opened) == 0 || opened[len(opened)-1] != tc.url {
				t.Fatalf("opened=%v", opened)
			}
			// Details enter continues to attach the selected chat, not attention's target.
			m := seedChatPicker(&chatPickerStub{}, statusStopped)
			m.session = s
			_, cmd = m.Update(attentionKey(tc.detailsKey))
			if cmd == nil {
				t.Fatal("missing details navigation")
			}
			msg = cmd()
			if tc.key == "enter" {
				nav, ok := msg.(switchViewMsg)
				if !ok || nav.resumeID != m.selectedChat().GetAgentSessionId() {
					t.Fatalf("details navigation=%#v", msg)
				}
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
	if len(c.calls) != 0 || !h.statusErr || h.status != "Couldn't merge PR #42: its head is not known yet; refresh and try again" {
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
	model, resume := h.Update(keyPress('r'))
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

func TestAttentionActionFooterShowsOneEnterDestination(t *testing.T) {
	h := attentionHome(&attentionStub{}, attentionSession(pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT))
	left, _, _ := h.sessionTableFooterActions()
	labels := strings.Join(left, "  ")
	if strings.Count(labels, "[enter]") != 1 || !strings.Contains(labels, "open the failed chat") {
		t.Fatalf("ambiguous enter footer: %s", labels)
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
			if !h.statusErr || h.status != "PR #42 changed since you opened it; refresh and try again" {
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

// TestBlockedParkDetailsListOpenItemsAndTheEscapeHatch opens the details view
// on an open-checklist park: it lists each open item its park note recorded,
// and since the row offers no merge it names the two ways forward.
func TestBlockedParkDetailsListOpenItemsAndTheEscapeHatch(t *testing.T) {
	m := attentionDetails(&attentionStub{}, parkSession("ledger-open", "Open human follow-up: watch the rollout", "Open question: which region first?"))
	screen := ansi.Strip(m.View().Content)
	for _, want := range []string{
		"The PR checklist has 2 open items",
		"Open in the PR checklist:",
		"• Open human follow-up: watch the rollout",
		"• Open question: which region first?",
		"boss pr approve 42",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("details view lacks %q:\n%s", want, screen)
		}
	}
	unread := attentionDetails(&attentionStub{}, parkSession("repair-exhausted"))
	if screen := ansi.Strip(unread.View().Content); !strings.Contains(screen, "Verify is blocked: repair exhausted.") || !strings.Contains(screen, "boss pr approve 42") {
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
		{"approval refused, head moved", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved"}}},
			"PR #42 changed since you opened it; refresh and try again", true},
		{"approval refused, park changed", &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "park-changed"}}},
			"Couldn't merge PR #42: what verify asks you to sign off changed since you looked; review it again, then merge", true},
		{"gate: head moved", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: expected head a, live head b; nothing was approved or merged", vcs.ErrHeadMismatch))},
			"PR #42 changed since you opened it; refresh and try again", true},
		{"gate: checks failing", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; 2 required checks are still failing; gated at head abc; nothing was approved or merged"))},
			"Couldn't merge PR #42: 2 required checks are still failing", true},
		{"gate after the approval landed", &attentionStub{mergeErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=conflict; the PR has a merge conflict; gated at head abc; the approval of abc (https://x/9) stays recorded"))},
			"Couldn't merge PR #42: the PR has a merge conflict; your approval stays recorded, so verify may merge it once it passes", true},
		{"rpc error", &attentionStub{mergeErr: connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))},
			"Couldn't merge PR #42: daemon unreachable", true},
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
