package tui

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/recurser/boss/api/tui/cron"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func newReadyCronFormForTest(width, height int) CronFormModel {
	m := NewCronFormModel(nil, context.Background())
	m.SetSize(width, height)
	v, _ := m.Update(cron.FormReposMsg{Repos: []*pb.Repo{{Id: "a", DisplayName: "repo-a"}}})
	m = v.(CronFormModel)
	v, _ = m.Update(cron.FormAgentsMsg{})
	return v.(CronFormModel)
}
func submittingCronFormForTest(width, height int) CronFormModel {
	m := newReadyCronFormForTest(width, height)
	m.Form().State = huh.StateCompleted
	v, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return v.(CronFormModel)
}

// viewSourceEntries retains source guards across the screen package split.
func viewSourceEntries(t *testing.T) ([]fs.DirEntry, error) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	for _, screen := range []string{"trash", "bugreport", "login", "onboarding", "cron", "repos", "newsession", "home"} {
		dir := filepath.Join("../../api/tui", screen)
		moved, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range moved {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			entries = append(entries, namedFormSource{filepath.Join(dir, entry.Name()), info})
		}
	}
	return entries, nil
}
