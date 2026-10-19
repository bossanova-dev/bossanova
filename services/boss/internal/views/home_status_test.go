package views

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// statusTimers records every expiry Home schedules, in order, instead of
// waiting the real homeStatusTTL for it.
type statusTimers struct {
	fired     []homeStatusExpiredMsg
	durations []time.Duration
	// cmdCode identifies the commands this fake returns, so scheduledBy can
	// find them inside a batch without running the batch's other commands.
	cmdCode uintptr
}

func captureStatusTimers(t *testing.T) *statusTimers {
	t.Helper()
	timers := &statusTimers{}
	prev := homeStatusTick
	homeStatusTick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		msg := fn(time.Time{}).(homeStatusExpiredMsg)
		timers.fired = append(timers.fired, msg)
		timers.durations = append(timers.durations, d)
		cmd := func() tea.Msg { return msg }
		timers.cmdCode = reflect.ValueOf(cmd).Pointer()
		return cmd
	}
	t.Cleanup(func() { homeStatusTick = prev })
	return timers
}

func (s *statusTimers) only(t *testing.T) homeStatusExpiredMsg {
	t.Helper()
	if len(s.fired) != 1 {
		t.Fatalf("scheduled %d status expiries, want 1", len(s.fired))
	}
	if s.durations[0] != 5*time.Second {
		t.Fatalf("expiry scheduled after %s, want 5s", s.durations[0])
	}
	return s.fired[0]
}

func (s *statusTimers) isExpiry(cmd tea.Cmd) bool {
	return s.cmdCode != 0 && reflect.ValueOf(cmd).Pointer() == s.cmdCode
}

// scheduledBy returns the status expiries cmd delivers to the program, so a
// handler that builds the timer and then drops it schedules nothing. A cmd that
// is not itself an expiry is run once to open its batch; the batch's other
// commands are left unrun, as they block on real ticks or reach the client.
func (s *statusTimers) scheduledBy(cmd tea.Cmd) []homeStatusExpiredMsg {
	if cmd == nil {
		return nil
	}
	if s.isExpiry(cmd) {
		return []homeStatusExpiredMsg{cmd().(homeStatusExpiredMsg)}
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		return nil
	}
	var expiries []homeStatusExpiredMsg
	for _, c := range batch {
		if c != nil && s.isExpiry(c) {
			expiries = append(expiries, c().(homeStatusExpiredMsg))
		}
	}
	return expiries
}

// expiryFrom requires cmd to deliver the one expiry scheduled so far.
func (s *statusTimers) expiryFrom(t *testing.T, cmd tea.Cmd) homeStatusExpiredMsg {
	t.Helper()
	want := s.only(t)
	if got := s.scheduledBy(cmd); len(got) != 1 || got[0] != want {
		t.Fatalf("the handler's command delivers expiries %#v, want only %#v", got, want)
	}
	return want
}

func homeStatusUpdate(t *testing.T, h HomeModel, msg tea.Msg) HomeModel {
	t.Helper()
	model, _ := h.Update(msg)
	return homeFromKey(t, model)
}

func homeScreen(h HomeModel) string {
	return ansi.Strip(h.View().Content)
}

func TestHomeMergedLineClearsAfterTheTimeout(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
	h = homeStatusUpdate(t, h, merge())
	merged := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	merged.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	model, cmd := h.Update(sessionListMsg{sessions: []*pb.Session{merged}})
	h = homeFromKey(t, model)
	if !strings.Contains(homeScreen(h), "Merged PR #42") {
		t.Fatalf("the settled merge is not reported:\n%s", homeScreen(h))
	}

	h = homeStatusUpdate(t, h, timers.expiryFrom(t, cmd))
	if h.status != "" || strings.Contains(homeScreen(h), "Merged PR #42") {
		t.Fatalf("status %q after its timeout, want the line gone:\n%s", h.status, homeScreen(h))
	}
}

func TestHomeRenamedLineClearsAfterTheTimeout(t *testing.T) {
	timers := captureStatusTimers(t)
	model, cmd := renameKeyHome(t).Update(sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "New name"}})
	h := homeFromKey(t, model)
	if !strings.Contains(homeScreen(h), "Renamed session to New name") {
		t.Fatalf("the rename is not reported:\n%s", homeScreen(h))
	}

	h = homeStatusUpdate(t, h, timers.expiryFrom(t, cmd))
	if strings.Contains(homeScreen(h), "Renamed session to") {
		t.Fatalf("the rename line outlived its timeout:\n%s", homeScreen(h))
	}
}

func TestHomeOlderTimerLeavesANewerStatus(t *testing.T) {
	timers := captureStatusTimers(t)
	h := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "First"}})
	h = homeStatusUpdate(t, h, sessionRenamedMsg{session: &pb.Session{Id: "sess-2", Title: "Second"}})
	if len(timers.fired) != 2 {
		t.Fatalf("scheduled %d expiries, want one per rename", len(timers.fired))
	}

	h = homeStatusUpdate(t, h, timers.fired[0])
	if h.status != "Renamed session to Second" {
		t.Fatalf("the first rename's timer left status %q, want the second rename still shown", h.status)
	}
	h = homeStatusUpdate(t, h, timers.fired[1])
	if h.status != "" {
		t.Fatalf("the second rename's own timer left status %q", h.status)
	}
}

func TestHomeTimerFromAnEarlierHomeLeavesTheRebuiltOne(t *testing.T) {
	timers := captureStatusTimers(t)
	before := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "First"}})
	stale := timers.only(t)

	after := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "Second"}})
	if after.generation == before.generation || after.statusSeq != stale.seq {
		t.Fatalf("fixture: generations %d/%d, seqs %d/%d; want different Homes at the same seq", before.generation, after.generation, stale.seq, after.statusSeq)
	}
	after = homeStatusUpdate(t, after, stale)
	if after.status != "Renamed session to Second" {
		t.Fatalf("an earlier Home's timer left status %q", after.status)
	}
}

func TestHomeWaitingMergeLineStaysUntilTheMergeSettles(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
	h = homeStatusUpdate(t, h, sessionRenamedMsg{session: &pb.Session{Id: "session-2", Title: "Quiet again"}})
	earlier := timers.only(t)
	h = homeStatusUpdate(t, h, merge())
	const waiting = "Waiting to hear whether PR #42 merged"
	if h.status != waiting || len(timers.fired) != 1 {
		t.Fatalf("status %q with %d expiries, want the waiting line and no new expiry", h.status, len(timers.fired))
	}

	h = homeStatusUpdate(t, h, earlier)
	h = homeStatusUpdate(t, h, tea.KeyPressMsg{Code: tea.KeyDown})
	stillMerging := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	stillMerging.DisplayMerging = true
	h = homeStatusUpdate(t, h, sessionListMsg{sessions: []*pb.Session{stillMerging, {Id: "session-2", Title: "Quiet"}}})
	if !strings.Contains(homeScreen(h), waiting) {
		t.Fatalf("the waiting line went before the merge settled:\n%s", homeScreen(h))
	}

	merged := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	merged.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	h = homeStatusUpdate(t, h, sessionListMsg{sessions: []*pb.Session{merged, {Id: "session-2", Title: "Quiet"}}})
	if h.status != "Merged PR #42" || len(timers.fired) != 2 {
		t.Fatalf("status %q with %d expiries after the merge settled, want the merged line on a timer", h.status, len(timers.fired))
	}
}

func TestHomeFailureStaysUntilAKeyOtherThanNavigation(t *testing.T) {
	timers := captureStatusTimers(t)
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{err: failed})
	if !h.statusErr() || len(timers.fired) != 0 {
		t.Fatalf("status %q (err %v) with %d expiries, want a failure with no timer", h.status, h.statusErr(), len(timers.fired))
	}
	h = homeStatusUpdate(t, h, tickMsg{})
	if !strings.Contains(homeScreen(h), "Rename failed") {
		t.Fatalf("the failure went without a key:\n%s", homeScreen(h))
	}

	navigation := []tea.KeyPressMsg{
		{Code: tea.KeyUp}, {Code: tea.KeyDown}, keyPress('j'), keyPress('k'),
		{Code: tea.KeyPgUp}, {Code: tea.KeyPgDown}, {Code: tea.KeyHome}, {Code: tea.KeyEnd},
		keyPress('G'), keyPress('b'), keyPress('f'), {Code: tea.KeySpace, Text: " "},
		keyPress('u'), {Code: 'u', Mod: tea.ModCtrl}, {Code: 'd', Mod: tea.ModCtrl},
	}
	for _, key := range navigation {
		h = homeStatusUpdate(t, h, key)
		if !strings.Contains(homeScreen(h), "Rename failed") {
			t.Fatalf("navigation key %q cleared the failure:\n%s", key.String(), homeScreen(h))
		}
	}

	for _, key := range []tea.KeyPressMsg{keyPress('z'), keyPress('g')} {
		h = homeStatusUpdate(t, h, sessionRenamedMsg{err: failed})
		h = homeStatusUpdate(t, h, key)
		if h.status != "" || strings.Contains(homeScreen(h), "Rename failed") {
			t.Fatalf("status %q after %q, want the failure gone", h.status, key.String())
		}
	}
}

// TestHomeTypingLeavesAFailure covers a merge that fails while the operator is
// typing into the search box or the rename editor: the letters belong to the
// field, so the failure stays on screen.
func TestHomeTypingLeavesAFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		open   rune
		typing func(HomeModel) bool
	}{
		{"search", '/', func(h HomeModel) bool { return h.filter.Active() && h.filter.Query() == "ab" }},
		{"rename", 'r', func(h HomeModel) bool { return h.rename.Active() && strings.HasSuffix(h.rename.Value(), "ab") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureStatusTimers(t)
			refused := connect.NewError(connect.CodeFailedPrecondition, errors.New("checks failing"))
			h, merge := homeMergeRunning(t, &attentionStub{mergeErr: refused})
			h = homeStatusUpdate(t, h, keyPress(tc.open))
			h = homeStatusUpdate(t, h, merge())
			if !h.statusErr() {
				t.Fatalf("status %q after the merge was refused, want a failure", h.status)
			}
			failure := h.status
			h = homeStatusUpdate(t, h, keyPress('a'))
			h = homeStatusUpdate(t, h, keyPress('b'))
			if !tc.typing(h) {
				t.Fatalf("the letters never reached the %s field", tc.name)
			}
			if h.status != failure || !strings.Contains(homeScreen(h), failure) {
				t.Fatalf("status %q after typing, want %q still on screen:\n%s", h.status, failure, homeScreen(h))
			}
		})
	}
}

// TestHomeOtherKeyClearsOnlyAFailure covers a key other than navigation pressed
// over an outcome or an unresolved merge: only a failure is dismissed by it.
func TestHomeOtherKeyClearsOnlyAFailure(t *testing.T) {
	captureStatusTimers(t)
	outcome := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "New name"}})
	outcome = homeStatusUpdate(t, outcome, keyPress('z'))
	if outcome.status != "Renamed session to New name" {
		t.Fatalf("a key other than navigation changed the outcome line to %q", outcome.status)
	}

	ongoing, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
	ongoing = homeStatusUpdate(t, ongoing, merge())
	const waiting = "Waiting to hear whether PR #42 merged"
	ongoing = homeStatusUpdate(t, ongoing, keyPress('z'))
	if ongoing.status != waiting || !strings.Contains(homeScreen(ongoing), waiting) {
		t.Fatalf("a key other than navigation changed the waiting line to %q", ongoing.status)
	}
}

// TestHomeDirectMergeResultClearsAfterTheTimeout covers a merge that answers at
// once: its "Merged PR" line rides out with the re-read and still clears.
func TestHomeDirectMergeResultClearsAfterTheTimeout(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{})
	model, cmd := h.Update(merge())
	h = homeFromKey(t, model)
	if h.status != "Merged PR #42" {
		t.Fatalf("status %q after the merge answered, want the merged line", h.status)
	}

	h = homeStatusUpdate(t, h, timers.expiryFrom(t, cmd))
	if h.status != "" || strings.Contains(homeScreen(h), "Merged PR #42") {
		t.Fatalf("status %q after its timeout, want the line gone:\n%s", h.status, homeScreen(h))
	}
}

func TestHomeOtherActionResultsClearAfterTheTimeout(t *testing.T) {
	for _, tc := range []struct {
		action pb.AttentionAction
		text   string
	}{
		{pb.AttentionAction_ATTENTION_ACTION_DISMISS, "Alert dismissed"},
		{pb.AttentionAction_ATTENTION_ACTION_RESUME, "Run resumed"},
		{pb.AttentionAction_ATTENTION_ACTION_GITHUB, "Opened GitHub"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			timers := captureStatusTimers(t)
			h := attentionHome(&attentionStub{}, attentionSession(tc.action))
			model, cmd := h.Update(attentionActionResultMsg{sessionID: "session-1", action: tc.action, text: tc.text})
			h = homeFromKey(t, model)
			if h.status != tc.text {
				t.Fatalf("status %q, want %q", h.status, tc.text)
			}
			h = homeStatusUpdate(t, h, timers.expiryFrom(t, cmd))
			if h.status != "" {
				t.Fatalf("status %q after its timeout, want the line gone", h.status)
			}
		})
	}
}

// TestHomeSettledMergeLeavesAnUndismissedFailure covers a poll that settles a
// waiting merge while an earlier failure is still on screen: the failure stays.
func TestHomeSettledMergeLeavesAnUndismissedFailure(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
	h = homeStatusUpdate(t, h, merge())
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h = homeStatusUpdate(t, h, sessionRenamedMsg{err: failed})

	merged := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	merged.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	h = homeStatusUpdate(t, h, sessionListMsg{sessions: []*pb.Session{merged, {Id: "session-2", Title: "Quiet"}}})
	if !h.statusErr() || !strings.Contains(homeScreen(h), "Rename failed") || len(timers.fired) != 0 {
		t.Fatalf("status %q with %d expiries after the merge settled, want the failure kept and no timer", h.status, len(timers.fired))
	}
	if h.isMerging("session-1") {
		t.Fatal("the settled merge still reads merging")
	}
}

// TestHomeDirectMergeResultLeavesAnUndismissedFailure covers a merge on one row
// that answers after a rename on another row failed: "Merged PR" would replace
// the failure and then time out, so the failure stays.
func TestHomeDirectMergeResultLeavesAnUndismissedFailure(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{})
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h = homeStatusUpdate(t, h, sessionRenamedMsg{err: failed})

	h = homeStatusUpdate(t, h, merge())
	if !h.statusErr() || !strings.Contains(homeScreen(h), "Rename failed") || strings.Contains(homeScreen(h), "Merged PR #42") {
		t.Fatalf("status %q after the merge answered, want the rename failure kept:\n%s", h.status, homeScreen(h))
	}
	if len(timers.fired) != 0 {
		t.Fatalf("scheduled %d expiries after the merge answered, want none over a failure", len(timers.fired))
	}
	if h.isMerging("session-1") {
		t.Fatal("the answered merge still reads merging")
	}
}

// TestHomeOngoingLineLeavesAnUndismissedFailure covers a merge that ends
// unanswered after a rename on another row failed: the waiting line would hide
// the failure until the merge settled, so the failure stays.
func TestHomeOngoingLineLeavesAnUndismissedFailure(t *testing.T) {
	timers := captureStatusTimers(t)
	h, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h = homeStatusUpdate(t, h, sessionRenamedMsg{err: failed})

	h = homeStatusUpdate(t, h, merge())
	if !h.statusErr() || !strings.Contains(homeScreen(h), "Rename failed") || strings.Contains(homeScreen(h), "Waiting to hear") {
		t.Fatalf("status %q after the merge went unanswered, want the rename failure kept:\n%s", h.status, homeScreen(h))
	}
	if len(timers.fired) != 0 || !h.isMerging("session-1") {
		t.Fatalf("%d expiries, merging %v; want no timer and the row still merging", len(timers.fired), h.isMerging("session-1"))
	}
}

// TestHomeDismissedFailureBringsBackTheMergeLine covers a failure dismissed
// while a merge is unresolved: the line the failure held back comes back, so
// the row that reads merging is not left beside an empty status line.
func TestHomeDismissedFailureBringsBackTheMergeLine(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		answer     func(HomeModel, tea.Cmd) HomeModel
	}{
		{"unanswered", "Waiting to hear whether PR #42 merged", func(h HomeModel, merge tea.Cmd) HomeModel {
			return homeStatusUpdate(t, h, merge())
		}},
		{"still running", "Merging PR #42…", func(h HomeModel, _ tea.Cmd) HomeModel { return h }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timers := captureStatusTimers(t)
			h, merge := homeMergeRunning(t, &attentionStub{mergeErr: noAnswerErrors["client deadline"]})
			failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
			h = homeStatusUpdate(t, h, sessionRenamedMsg{err: failed})
			h = tc.answer(h, merge)

			h = homeStatusUpdate(t, h, keyPress('d'))
			if h.status != tc.line || h.statusKind != statusOngoing || !strings.Contains(homeScreen(h), tc.line) {
				t.Fatalf("status %q (kind %d) after d, want the merge's line %q back:\n%s", h.status, h.statusKind, tc.line, homeScreen(h))
			}
			if len(timers.fired) != 0 || !h.isMerging("session-1") {
				t.Fatalf("%d expiries, merging %v; want no timer and the row still merging", len(timers.fired), h.isMerging("session-1"))
			}
		})
	}
}

// TestHomeDismissesAFailureOnD covers Home's own keymap: Home frees d from the
// table's half-page binding, so on Home d is not navigation and dismisses a
// failure. Reading the table's default keymap instead would keep the failure.
func TestHomeDismissesAFailureOnD(t *testing.T) {
	captureStatusTimers(t)
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{err: failed})
	h = homeStatusUpdate(t, h, keyPress('d'))
	if h.status != "" || strings.Contains(homeScreen(h), "Rename failed") {
		t.Fatalf("status %q after d, want the failure gone", h.status)
	}
}

// TestHomeKeyReleaseLeavesAFailure covers a terminal that reports key releases.
// A failure that lands while a key is held must survive that key's release:
// the operator has not pressed anything with the failure on screen.
func TestHomeKeyReleaseLeavesAFailure(t *testing.T) {
	captureStatusTimers(t)
	failed := connect.NewError(connect.CodeUnavailable, errors.New("daemon unreachable"))
	h := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{err: failed})
	h = homeStatusUpdate(t, h, tea.KeyReleaseMsg{Code: 'z', Text: "z"})
	if !h.statusErr() || !strings.Contains(homeScreen(h), "Rename failed") {
		t.Fatalf("status %q after a key release, want the failure kept", h.status)
	}
}

func TestHomeKeyLeavesAnOutcomeToItsTimer(t *testing.T) {
	captureStatusTimers(t)
	h := homeStatusUpdate(t, renameKeyHome(t), sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "New name"}})
	h = homeStatusUpdate(t, h, tea.KeyPressMsg{Code: tea.KeyDown})
	if h.status != "Renamed session to New name" {
		t.Fatalf("a navigation key changed the outcome line to %q", h.status)
	}
}

// TestHomeOutcomeSeenOnReturnStartsItsTimer covers a merge that finished while
// the operator was on another view: its line waits for them, and the 5 seconds
// start once Home is back on screen.
func TestHomeOutcomeSeenOnReturnStartsItsTimer(t *testing.T) {
	timers := captureStatusTimers(t)
	a := appWithHomeOffScreen()
	model, _ := a.Update(attentionActionResultMsg{sessionID: "session-1", action: pb.AttentionAction_ATTENTION_ACTION_MERGE, text: "Merged PR #42"})
	a = model.(App)
	offScreen := append([]homeStatusExpiredMsg(nil), timers.fired...)

	onReturn := timers.scheduledBy(a.switchToHome())
	if a.home.status != "Merged PR #42" || len(onReturn) != 1 || len(timers.fired) != len(offScreen)+1 || onReturn[0] != timers.fired[len(timers.fired)-1] {
		t.Fatalf("returning to Home showed %q and Init delivers expiries %#v, want the merge outcome on one new timer", a.home.status, onReturn)
	}
	for _, stale := range offScreen {
		model, _ = a.Update(stale)
		a = model.(App)
	}
	if a.home.status != "Merged PR #42" {
		t.Fatalf("a timer from before the return left status %q", a.home.status)
	}
	model, _ = a.Update(onReturn[0])
	if got := model.(App).home.status; got != "" {
		t.Fatalf("status %q after the timer started on return, want it gone", got)
	}
}

// TestHomeFailureSeenOnReturnStaysAFailure covers a merge that failed while the
// operator was on another view: the rebuilt Home shows it as a failure, with
// no timer.
func TestHomeFailureSeenOnReturnStaysAFailure(t *testing.T) {
	timers := captureStatusTimers(t)
	a := appWithHomeOffScreen()
	refused := connect.NewError(connect.CodeFailedPrecondition, errors.New("checks are failing"))
	model, _ := a.Update(attentionActionResultMsg{sessionID: "session-1", action: pb.AttentionAction_ATTENTION_ACTION_MERGE, err: refused})
	a = model.(App)

	onReturn := timers.scheduledBy(a.switchToHome())
	if !a.home.statusErr() || len(onReturn) != 0 || len(timers.fired) != 0 {
		t.Fatalf("returning to Home showed %q (err %v) with expiries %#v, want the failure and no timer", a.home.status, a.home.statusErr(), onReturn)
	}
	if !strings.Contains(a.home.status, "checks are failing") {
		t.Fatalf("the rebuilt Home's status is %q, want the merge failure", a.home.status)
	}
}

func appWithHomeOffScreen() App {
	c := &attentionStub{}
	a := NewApp(c, nil)
	a.home = attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE))
	a.home.markMerging(attentionActionState{sessionID: "session-1", action: pb.AttentionAction_ATTENTION_ACTION_MERGE})
	a.activeView = ViewChatPicker
	return a
}
