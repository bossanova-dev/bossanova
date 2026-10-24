package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"github.com/recurser/boss/api/tui/ui"

	"connectrpc.com/connect"

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

func TestHomeOutcomeSeenOnReturnStartsItsTimer(t *testing.T) {
	timers := captureStatusTimers(t)
	a := appWithHomeOffScreen()
	model, _ := a.Update(ui.AttentionActionResultMsg{SessionID: "session-1", Action: pb.AttentionAction_ATTENTION_ACTION_MERGE, Text: "Merged PR #42"})
	a = model.(App)
	offScreen := append([]homeStatusExpiredMsg(nil), timers.fired...)

	onReturn := timers.scheduledBy(a.switchToHome())
	if a.home.Status != "Merged PR #42" || len(onReturn) != 1 || len(timers.fired) != len(offScreen)+1 || onReturn[0] != timers.fired[len(timers.fired)-1] {
		t.Fatalf("returning to Home showed %q and Init delivers expiries %#v, want the merge outcome on one new timer", a.home.Status, onReturn)
	}
	for _, stale := range offScreen {
		model, _ = a.Update(stale)
		a = model.(App)
	}
	if a.home.Status != "Merged PR #42" {
		t.Fatalf("a timer from before the return left status %q", a.home.Status)
	}
	model, _ = a.Update(onReturn[0])
	if got := model.(App).home.Status; got != "" {
		t.Fatalf("status %q after the timer started on return, want it gone", got)
	}
}

// TestHomeFailureSeenOnReturnStaysAFailure covers a merge that failed while the
// operator was on another view: the rebuilt Home shows it as a failure, with
// no timer.

func TestHomeFailureSeenOnReturnStaysAFailure(t *testing.T) {
	timers := captureStatusTimers(t)
	a := appWithHomeOffScreen()
	refused := rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, errors.New("checks are failing")))
	model, _ := a.Update(ui.AttentionActionResultMsg{SessionID: "session-1", Action: pb.AttentionAction_ATTENTION_ACTION_MERGE, Err: refused})
	a = model.(App)

	onReturn := timers.scheduledBy(a.switchToHome())
	if !a.home.StatusErr() || len(onReturn) != 0 || len(timers.fired) != 0 {
		t.Fatalf("returning to Home showed %q (err %v) with expiries %#v, want the failure and no timer", a.home.Status, a.home.StatusErr(), onReturn)
	}
	if !strings.Contains(a.home.Status, "checks are failing") {
		t.Fatalf("the rebuilt Home's status is %q, want the merge failure", a.home.Status)
	}
}

func appWithHomeOffScreen() App {
	c := &attentionStub{}
	a := NewApp(c, nil)
	a.home = attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE))
	a.home.MarkMerging(ui.AttentionActionState{SessionID: "session-1", Action: pb.AttentionAction_ATTENTION_ACTION_MERGE})
	a.activeView = ui.ViewChatPicker
	return a
}
