package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func updateModel[T tea.Model](model T, msg tea.Msg) (T, tea.Cmd) {
	next, cmd := model.Update(msg)
	return next.(T), cmd
}
func readyCronForm(ctx context.Context) CronFormModel {
	m := NewCronFormModel(nil, ctx)
	m, _ = updateModel(m, cronFormReposMsg{})
	m, _ = updateModel(m, cronFormAgentsMsg{})
	return m
}
func submittingCronForm(ctx context.Context) CronFormModel {
	m := readyCronForm(ctx)
	m.Form().State = huh.StateCompleted
	m, _ = updateModel(m, nil)
	return m
}
func submittingBugReport(m BugReportModel) BugReportModel {
	m.Form().State = huh.StateCompleted
	m, _ = updateModel(m, nil)
	return m
}
func filteredTrash(m TrashModel, query string) TrashModel {
	m, _ = updateModel(m, trashListMsg{Sessions: []*pb.Session{{Id: "sess-1", Title: "demo"}}})
	m, _ = updateModel(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m, _ = updateModel(m, tea.PasteMsg{Content: query})
	return m
}
func pendingCron(m CronListModel, keys ...rune) CronListModel {
	m, _ = updateModel(m, cronJobsLoadedMsg{Jobs: []*pb.CronJob{{Id: "job-1", Name: "job"}}})
	for _, key := range keys {
		m, _ = updateModel(m, tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	return m
}
func pendingTrash(m TrashModel, keys ...rune) TrashModel {
	m, _ = updateModel(m, trashListMsg{Sessions: []*pb.Session{{Id: "sess-1", Title: "demo"}}})
	for _, key := range keys {
		m, _ = updateModel(m, tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	return m
}
