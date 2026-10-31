package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"github.com/recurser/boss/api/tui/home"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAttentionEnterOpensDetailsWithTheTargetChatSelected(t *testing.T) {
	for _, action := range []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, pb.AttentionAction_ATTENTION_ACTION_ANSWER} {
		t.Run(action.String(), func(t *testing.T) {
			c := &attentionStub{}
			h := attentionHome(c, attentionSession(action, pb.AttentionAction_ATTENTION_ACTION_RESUME))
			_, cmd := h.Update(attentionKey("enter"))
			if cmd == nil {
				t.Fatal("enter did nothing")
			}
			nav, ok := cmd().(ui.SwitchViewMsg)
			if !ok || nav.View != ui.ViewChatPicker || nav.SessionID != "session-1" {
				t.Fatalf("enter navigated to %#v, want the session's chat list", nav)
			}
			if len(c.calls) != 0 {
				t.Fatalf("enter dispatched %v", c.calls)
			}

			a := NewApp(c, nil)
			a.home = h
			model, _ := a.Update(nav)
			a = model.(App)
			if a.activeView != ui.ViewChatPicker {
				t.Fatalf("activeView=%v, want the chat list", a.activeView)
			}
			now := time.Now()
			chat := func(id string, age time.Duration) *pb.AgentChat {
				return &pb.AgentChat{SessionId: "session-1", AgentSessionId: id, Title: id, CreatedAt: timestamppb.New(now.Add(-age))}
			}
			model, _ = a.Update(chatpicker.ChatsListedMsg{
				Chats:          []*pb.AgentChat{chat("newest-chat", time.Minute), chat("target-chat", time.Hour), chat("asking-chat", 2*time.Hour)},
				DaemonStatuses: map[string]string{"newest-chat": ui.StatusWorking, "target-chat": ui.StatusStopped, "asking-chat": ui.StatusQuestion},
			})
			a = model.(App)
			if got := a.chatPicker.SelectedChatID(); got != "target-chat" {
				t.Fatalf("details selected %q, want target-chat", got)
			}

			_, cmd = a.Update(attentionKey("enter"))
			if cmd == nil {
				t.Fatal("second enter did nothing")
			}
			attach, ok := cmd().(ui.SwitchViewMsg)
			if !ok || attach.View != ui.ViewAttach || attach.SessionID != "session-1" || attach.ResumeID != "target-chat" {
				t.Fatalf("second enter navigated to %#v, want target-chat attached", attach)
			}
		})
	}
}

// TestHomeEnterWithoutATargetChatLeavesTheDefaultSelection keeps the details
// view's own cursor choice for a row whose attention names no chat.

func TestAttentionDismissLeftRunningIsNotShownAsMerging(t *testing.T) {
	c := &attentionStub{}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ui.ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('x'))
	model, dismiss := model.Update(keyPress('y'))
	if dismiss == nil {
		t.Fatal("confirming did not send the dismiss")
	}
	model, _ = model.Update(attentionKey("esc"))
	if got := model.(App); got.activeView != ui.ViewHome || mergeRowReadsMerging(got.home) {
		t.Fatalf("view %v, row merging %v after Esc mid-dismiss: want Home without a merging row", got.activeView, mergeRowReadsMerging(got.home))
	}
	model, _ = model.Update(dismiss())
	if got := model.(App); mergeRowReadsMerging(got.home) || len(got.home.MergesInFlight) != 0 {
		t.Fatalf("after the dismiss answered: row merging %v, merges held %v; want none", mergeRowReadsMerging(got.home), got.home.MergesInFlight)
	}
}

// TestOngoingStatusIsOnlyAnUnresolvedMergesLine: the line Home brings back
// beside a row reading merging belongs to a merge whose outcome is not known.
// A merge whose call has answered, or another action still running, has none,
// whatever progress line it kept.

func TestAttentionMergeWithNoAnswerOnTheDetailsViewSettlesOnHome(t *testing.T) {
	c := &attentionStub{mergeErr: noAnswerErrors["client deadline"]}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ui.ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	model, _ = model.Update(merge())
	if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, "Waiting to hear whether PR #42 merged") || strings.Contains(screen, "deadline") || strings.Contains(screen, "Couldn't merge") {
		t.Fatalf("details view after a merge with no answer:\n%s", screen)
	}
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ui.ViewHome || !mergeRowReadsMerging(got.home) {
		t.Fatalf("view %v, row merging %v: want Home showing the unanswered merge as merging", got.activeView, mergeRowReadsMerging(got.home))
	}
	merged := proto.CloneOf(s)
	merged.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	model, _ = model.Update(sessionListMsg{Sessions: []*pb.Session{merged}})
	if got := model.(App); mergeRowReadsMerging(got.home) || got.home.Status != "Merged PR #42" {
		t.Fatalf("after the poll: row merging %v, status %q; want the merge settled as merged", mergeRowReadsMerging(got.home), got.home.Status)
	}
}

// TestMergeOutcomeUnknownTrustsOnlyAnAnswerBossdSent runs a real connect
// round trip: bossd's own DeadlineExceeded is an outcome, while the client
// giving up first is not. The hanging handler ignores its own deadline, as the
// detached merge does, so only the client can end that call.

func TestAttentionMergeWithNoAnswerOffersNoActionOnTheDetailsView(t *testing.T) {
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
	unanswered := map[string]func(*testing.T, *attentionStub) tea.Model{
		"answer lands on the details view": func(t *testing.T, c *attentionStub) tea.Model {
			a := NewApp(c, nil)
			a.home = attentionHome(c, proto.CloneOf(s))
			a.chatPicker = attentionDetails(c, proto.CloneOf(s))
			a.activeView = ui.ViewChatPicker
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
			model, _ = model.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-1"})
			model, _ = model.Update(chatpicker.SessionMsg{Session: proto.CloneOf(s)})
			model, _ = model.Update(listedChats(1))
			if got := model.(App); got.activeView != ui.ViewChatPicker {
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
					if cmd != nil || got.AttentionAction().Active() || strings.Contains(got.View().Content, "[y/enter] confirm") {
						t.Fatalf("%s: %q started an action (cmd %v, attention %+v, screen %q)", when, key, cmd != nil, got.AttentionAction(), got.View().Content)
					}
				}
				if len(c.merges) != 1 {
					t.Fatalf("%s: merges = %+v, want only the unanswered one", when, c.merges)
				}
			}
			assertNoActions("after the unanswered merge")

			stillMerging := proto.CloneOf(s)
			stillMerging.DisplayMerging = true
			model, _ = model.Update(chatpicker.RefreshMsg{Session: stillMerging})
			assertNoActions("while bossd still merges")

			model, _ = model.Update(chatpicker.RefreshMsg{Session: proto.CloneOf(s)})
			screen := ansi.Strip(model.View().Content)
			if !strings.Contains(screen, "No answer came back about merging PR #42, and it has not merged") || strings.Contains(screen, "Waiting to hear") || !strings.Contains(screen, "[m]erge") {
				t.Fatalf("after a poll showing the merge ended: want the outcome and merge on offer again:\n%s", screen)
			}
			model, _ = model.Update(keyPress('m'))
			if got := model.(App).chatPicker.AttentionAction(); !got.Confirm.Active {
				t.Fatalf("[m] after the merge settled did not ask to merge: %+v", got)
			}
		})
	}
}

// TestAttentionMergeWithNoAnswerEndsWhenTheSessionLeavesTheList covers the poll
// that no longer lists the session, as when it archived after merging: the
// merge stops holding the session, and its waiting line goes with it unless a
// newer status has replaced it.

func attentionDetails(c *attentionStub, s *pb.Session) chatpicker.Model {
	m := seedChatPicker(c, ui.StatusStopped)
	updated, cmd := m.Update(chatpicker.RefreshMsg{Session: s})
	m = updated.(chatpicker.Model)
	var deliver func(tea.Msg)
	deliver = func(msg tea.Msg) {
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, command := range batch {
				deliver(command())
			}
			return
		}
		updated, _ := m.Update(msg)
		m = updated.(chatpicker.Model)
	}
	if cmd != nil {
		deliver(cmd())
	}
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return updated.(chatpicker.Model)
}

func TestAttentionActionResumeAndRename(t *testing.T) {
	for _, details := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "details"}[details], func(t *testing.T) {
			c := &attentionStub{}
			s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME)
			var model tea.Model = attentionHome(c, s)
			if details {
				m := attentionDetails(c, s)
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
				h.BuildTableRows()
				row, _ := h.TableCursorForSessionID(s.GetId())
				h.Table.SetCursor(row)
				model = h
			}
			model, _ = model.Update(keyPress('r'))
			switch m := model.(type) {
			case HomeModel:
				if !m.Rename.Active() {
					t.Fatal("home rename lost")
				}
			case chatpicker.Model:
				if !m.TextEntryActive() {
					t.Fatal("chat rename lost")
				}
			}
		})
	}
}

func TestAttentionActionNavigationAndAvailability(t *testing.T) {
	var opened []string
	original := home.OpenURLFunc
	home.OpenURLFunc = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { home.OpenURLFunc = original })
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
			m := attentionDetails(&attentionStub{}, s)
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
		m := attentionDetails(c, attentionSession())
		_, cmd = m.Update(keyPress(key))
		if cmd != nil || len(c.calls) > 0 {
			t.Fatalf("unavailable details %c dispatched", key)
		}
	}
}

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
// (skills-toolbox/verify/verify-gate.mjs HUMAN_REASONS).
var knownParkCodes = []string{
	"always-human-path", "human-review:database", "human-review:security", "human-review:ui",
	"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session",
	"worktree-diverged", "worktree-unsynced", "human-review:unproven",
}

const renderedHead = "0123456789abcdef0123456789abcdef01234567"

// parkSession is a session parked by verify on code, as bossd serves it: the
// attention Compute answers, plus the legacy display fields older views read.

func TestAttentionReviewMergeRecordsTheApprovalAndMergesOnce(t *testing.T) {
	s := parkSession("human-review:ui")
	if got, want := ui.AttentionFooter(s, false), []string{"[v] video", "[m] merge"}; !slices.Equal(got, want) {
		t.Fatalf("home footer = %q, want %q", got, want)
	}
	if got, want := ui.AttentionFooter(s, true), []string{"[g] github", "[v] video", "[m] merge"}; !slices.Equal(got, want) {
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
				assertMergeConfirm(t, m.AttentionAction)
			case chatpicker.Model:
				assertMergeConfirm(t, m.AttentionAction())
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
		"the PR checklist has 2 open items",
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

func mergeLeftRunning(t *testing.T, c *attentionStub) (tea.Model, tea.Cmd) {
	t.Helper()
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	a := NewApp(c, nil)
	a.home = attentionHome(c, proto.CloneOf(s))
	a.chatPicker = attentionDetails(c, proto.CloneOf(s))
	a.activeView = ui.ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	if merge == nil {
		t.Fatal("confirming did not send the merge")
	}
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ui.ViewHome {
		t.Fatalf("active view after Esc mid-merge = %v, want Home", got.activeView)
	}
	if row := ansi.Strip(got.home.RenderSessionStatus(got.home.Sessions[0])); !strings.Contains(row, "merging") {
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
		{name: "refused", err: rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; checks are still running; nothing was approved or merged"))), want: "Couldn't merge PR #42: checks are still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &attentionStub{mergeErr: tc.err}
			model, merge := mergeLeftRunning(t, c)

			model, _ = model.Update(merge())
			got := model.(App)
			if len(c.merges) != 1 {
				t.Fatalf("merges = %+v, want the one merge the operator confirmed", c.merges)
			}
			if !strings.Contains(got.home.Status, tc.want) || got.home.StatusErr() != (tc.err != nil) {
				t.Fatalf("Home status = %q (err %v), want %q", got.home.Status, got.home.StatusErr(), tc.want)
			}
			if row := ansi.Strip(got.home.RenderSessionStatus(got.home.Sessions[0])); strings.Contains(row, "merging") {
				t.Fatalf("Home row = %q after the merge answered, want it to stop reading merging", row)
			}
		})
	}
}

func TestAttentionMergeOutcomeWaitsForTheOperatorsReturnToHome(t *testing.T) {
	c := &attentionStub{}
	model, merge := mergeLeftRunning(t, c)

	model, _ = model.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-1"})
	if footer := model.(App).chatPicker.AttentionAction().Footer(80, ""); !strings.Contains(footer, "Merging PR #42…") {
		t.Fatalf("reopened details view footer = %q, want it to say the merge is still running", footer)
	}
	model, _ = model.Update(attentionKey("esc"))
	model, _ = model.Update(ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-2"})
	model, _ = model.Update(merge())
	model, _ = model.Update(attentionKey("esc"))
	got := model.(App)
	if got.activeView != ui.ViewHome || !strings.Contains(got.home.Status, "Merged PR #42") {
		t.Fatalf("view %v, Home status %q: want the outcome that landed off screen waiting on Home", got.activeView, got.home.Status)
	}
	if _, ok := got.home.MergeInFlight("session-1"); ok {
		t.Fatal("Home still holds the merge as running after it answered")
	}
}

// mergeRowReadsMerging reports whether Home's session-1 row reads merging.

func TestAttentionMergeIsNotOfferedTwiceWhileItRuns(t *testing.T) {
	for name, running := range map[string]func(*testing.T, *attentionStub) (HomeModel, tea.Cmd){
		"started on Home": homeMergeRunning,
		"left running on the details view": func(t *testing.T, c *attentionStub) (HomeModel, tea.Cmd) {
			model, merge := mergeLeftRunning(t, c)
			var home tea.Model = model.(App).home
			home, _ = home.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
			home, _ = home.Update(sessionListMsg{Sessions: []*pb.Session{attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)}})
			return home.(HomeModel), merge
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &attentionStub{}
			h, merge := running(t, c)
			h.Table.SetCursor(0)
			if got := h.SelectedSession().GetId(); got != "session-1" {
				t.Fatalf("selected session = %q, want the merging session-1", got)
			}
			if left, _, _ := h.SessionTableFooterActions(); slices.Contains(left, "[m] merge") {
				t.Fatalf("Home footer %q offers merge on a row that reads merging", left)
			}
			model, cmd := h.Update(keyPress('m'))
			model, confirm := model.Update(keyPress('y'))
			if cmd != nil || confirm != nil || model.(HomeModel).AttentionAction.Active() {
				t.Fatal("[m] on a row that reads merging started a second merge")
			}
			_ = merge()
			if len(c.merges) != 1 {
				t.Fatalf("merges = %+v, want only the merge already running", c.merges)
			}
		})
	}
}
