package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/ui"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Backing out of a cron job's form lands on that job's row, not the top of the
// rebuilt list.
func TestAppCronFormCancelKeepsSelectedJob(t *testing.T) {
	jobs := []*pb.CronJob{{Id: "a"}, {Id: "b"}, {Id: "c"}}
	a := NewApp(nil, nil)
	a.cronList, _ = updateModel(NewCronListModel(nil, a.ctx), cronJobsLoadedMsg{Jobs: jobs})
	a.cronList.Table().SetCursor(2)
	a.cronForm = NewCronFormModel(nil, a.ctx)
	a.activeView = ui.ViewCronForm

	model, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := model.(App)
	if got.activeView != ui.ViewCron {
		t.Fatalf("cron form esc routed to %v, want ViewCron", got.activeView)
	}
	model, _ = got.Update(cronJobsLoadedMsg{Jobs: jobs})
	if id := model.(App).cronList.SelectedJobID(); id != "c" {
		t.Fatalf("selected job after esc = %q, want %q", id, "c")
	}
}

// Backing out of an account's edit form lands on that account's row, while
// opening the list afresh from Settings starts at the top.
func TestAppAccountEditCancelKeepsSelectedAccount(t *testing.T) {
	accts := []*pb.AgentAccount{{Id: "a", Status: "active"}, {Id: "b", Status: "active"}, {Id: "c", Status: "active"}}
	a := NewApp(nil, nil)
	a.accountsList, _ = updateModel(accounts.NewAgentAccountsListModel(nil, a.ctx), accounts.AgentAccountsLoadedMsg{Accounts: accts})
	a.accountsList.Table().SetCursor(2)
	edit := accounts.NewAgentAccountEditModel(nil, a.ctx, accts[2])
	edit, _ = updateModel(edit, tea.KeyPressMsg{Code: tea.KeyEscape})
	edit.SetReturnView(ui.ViewAgentAccounts)
	a.accountEdit = edit
	a.activeView = ui.ViewAgentAccountEdit

	model, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got := model.(App)
	msg := runCmd(cmd)
	if msg == nil {
		t.Fatal("account edit cancel emitted no view switch")
	}
	model, _ = got.Update(msg)
	model, _ = model.(App).Update(accounts.AgentAccountsLoadedMsg{Accounts: accts})
	if id := model.(App).accountsList.SelectedAgentAccountID(); id != "c" {
		t.Fatalf("selected account after esc = %q, want %q", id, "c")
	}

	model, _ = model.(App).Update(ui.SwitchViewMsg{View: ui.ViewAgentAccounts, ReturnView: ui.ViewSettings})
	model, _ = model.(App).Update(accounts.AgentAccountsLoadedMsg{Accounts: accts})
	if id := model.(App).accountsList.SelectedAgentAccountID(); id != "a" {
		t.Fatalf("selected account on a fresh entry = %q, want %q", id, "a")
	}
}
