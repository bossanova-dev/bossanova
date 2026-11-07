package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestHomeWaitingOpensReviewQueueAndReturnsWithOrder(t *testing.T) {
	a := NewApp(&stubClient{}, nil)
	a.width, a.height = 120, 40
	model, cmd := a.Update(keyPress('w'))
	a = model.(App)
	if cmd == nil {
		t.Fatal("Home waiting key has no command")
	}
	switchMsg, ok := cmd().(ui.SwitchViewMsg)
	if !ok || switchMsg.View != ui.ViewReviewQueue {
		t.Fatalf("waiting target = %#v", switchMsg)
	}
	model, _ = a.Update(switchMsg)
	a = model.(App)
	if a.activeView != ui.ViewReviewQueue || a.reviewQueue.Width() != 120 {
		t.Fatalf("queue not constructed at screen width")
	}
	model, _ = a.Update(keyPress('o'))
	a = model.(App)
	if a.reviewQueueSort != pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT {
		t.Fatal("order not retained by App")
	}
	model, cmd = a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	a = model.(App)
	if cmd == nil {
		t.Fatal("escape has no navigation command")
	}
	model, _ = a.Update(cmd())
	a = model.(App)
	if a.activeView != ui.ViewHome {
		t.Fatalf("escape returns to %v", a.activeView)
	}
	model, _ = a.Update(switchMsg)
	a = model.(App)
	if a.reviewQueue.Order() != pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT {
		t.Fatal("reopening lost chosen order")
	}
}

func TestQueuePollAndLoadContinueBehindBugReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewApp(&queueModalClient{stubClient: &stubClient{}}, nil)
		model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewReviewQueue})
		a = model.(App)
		batch := a.reviewQueue.Init()().(tea.BatchMsg)
		a.bugReport = NewBugReportModel(nil, a.ctx, nil, ui.ViewReviewQueue, nil, nil)
		a.activeView = ui.ViewBugReport
		model, next := a.Update(batch[1]())
		a = model.(App)
		if next == nil || a.activeView != ui.ViewBugReport {
			t.Fatal("modal swallowed queue timer")
		}
		model, _ = a.Update(batch[0]())
		a = model.(App)
		a.bugReport, _ = updateModel(a.bugReport, bugReportSubmitMsg{})
		model, _ = a.Update(keyPress('x'))
		a = model.(App)
		if a.activeView != ui.ViewReviewQueue || !strings.Contains(a.reviewQueue.View().Content, "load finished behind modal") {
			t.Fatal("modal swallowed queue load")
		}
	})
}

type queueModalClient struct{ *stubClient }

func (*queueModalClient) ListReviewQueueRepos(context.Context) ([]*pb.Repo, error) {
	return nil, errors.New("load finished behind modal")
}
