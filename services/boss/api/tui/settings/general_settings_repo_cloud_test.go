package settings

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

type settingsRepoCloudClient struct{ repos []*pb.RepoCloudControl }

func (c settingsRepoCloudClient) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	return nil, nil
}
func (c settingsRepoCloudClient) ListRepoCloudControls(context.Context) ([]*pb.RepoCloudControl, error) {
	return c.repos, nil
}

func repoCloudModel(t *testing.T) GeneralSettingsModel {
	t.Helper()
	withTempConfigHome(t)
	repos := []*pb.RepoCloudControl{
		{DisplayName: "oss", Host: "github.com", Name: "acme/oss", Checkout: t.TempDir(), ProviderRepoId: 11, IsIdVerified: true},
		{DisplayName: "secret", Host: "github.com", Name: "acme/secret", Checkout: t.TempDir(), ProviderRepoId: 12, IsIdVerified: true},
		{DisplayName: "other", Host: "github.com", Name: "else/other", Checkout: t.TempDir(), ProviderRepoId: 13, IsIdVerified: true},
	}
	return NewGeneralSettingsModel(settingsRepoCloudClient{repos}, context.Background(), nil)
}
func selectCloudRow(t *testing.T, m GeneralSettingsModel, label string) GeneralSettingsModel {
	t.Helper()
	for i, row := range m.rows {
		if row.Label == label && !row.IsHeader {
			m.cursor = i
			return m
		}
	}
	t.Fatalf("editable cloud row %q missing", label)
	return m
}
func Test_Settings_AC237902_RepoAndGroupEditing(t *testing.T) {
	m := repoCloudModel(t)
	m = selectCloudRow(t, m, "oss")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.settings.CloudControlRepos) != 1 || m.settings.CloudControlRepos[0].Mode != "private" {
		t.Fatalf("single repo edit: %+v", m.settings.CloudControlRepos)
	}
	m = selectCloudRow(t, m, "github.com/acme")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.settings.CloudControlRepos) != 2 {
		t.Fatalf("group edit touched wrong repos: %+v", m.settings.CloudControlRepos)
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	before := append([]config.CloudControlRepoGrant(nil), m.settings.CloudControlRepos...)
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.confirmCloudControl || !reflect.DeepEqual(before, m.settings.CloudControlRepos) {
		t.Fatal("full group grant did not await confirmation")
	}
	if !strings.Contains(m.View().Content, "every other repo on this machine") {
		t.Fatal("cross-repo warning missing")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if !reflect.DeepEqual(before, m.settings.CloudControlRepos) {
		t.Fatal("cancel changed grants")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, g := range m.settings.CloudControlRepos {
		if g.Mode != "full" || g.RepoID == 0 || g.Host != "github.com" || g.Checkout == "" {
			t.Fatalf("identity grant: %+v", g)
		}
	}
	saved, err := config.Load()
	if err != nil || !reflect.DeepEqual(saved.CloudControlRepos, m.settings.CloudControlRepos) {
		t.Fatalf("save = %+v, %v", saved.CloudControlRepos, err)
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.settings.CloudControlRepos) != 0 {
		t.Fatal("default did not remove group grants")
	}
}
func Test_Settings_AC237902_SaveAndValidationRollback(t *testing.T) {
	m := repoCloudModel(t)
	m = selectCloudRow(t, m, "github.com/acme")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	before := append([]config.CloudControlRepoGrant(nil), m.settings.CloudControlRepos...)
	badPath := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(badPath, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.err == nil || !reflect.DeepEqual(before, m.settings.CloudControlRepos) {
		t.Fatal("save failure did not restore grants")
	}
}
func Test_Settings_AC237903_MachineDefault(t *testing.T) {
	m := cloudControlModel(t, "full")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "")
	if !strings.Contains(m.View().Content, "(default)") {
		t.Fatal("default label missing")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "private")
}

func Test_Settings_AC237902_UnverifiedGroupRollback(t *testing.T) {
	m := repoCloudModel(t)
	// The second repository refuses widening after the first has been processed.
	m.repoCloudControls[1].IsIdVerified = false
	m = selectCloudRow(t, m, "github.com/acme")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	before := append([]config.CloudControlRepoGrant(nil), m.settings.CloudControlRepos...)
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.err == nil || !reflect.DeepEqual(before, m.settings.CloudControlRepos) {
		t.Fatalf("unverified group widening did not roll back all changes: %+v, %v", m.settings.CloudControlRepos, m.err)
	}
	saved, err := config.Load()
	if err != nil || !reflect.DeepEqual(before, saved.CloudControlRepos) {
		t.Fatalf("failed group widening reached disk: %+v, %v", saved.CloudControlRepos, err)
	}
}

func Test_Settings_AC237902_SingleRepoConfirmationAndStaleGrant(t *testing.T) {
	m := repoCloudModel(t)
	m = selectCloudRow(t, m, "oss")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	before := append([]config.CloudControlRepoGrant(nil), m.settings.CloudControlRepos...)
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.confirmCloudControl || !reflect.DeepEqual(before, m.settings.CloudControlRepos) {
		t.Fatal("single repo full did not await confirmation")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.Cancelled() || m.confirmCloudControl {
		t.Fatal("confirmation escape exited settings")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: 'Y', Text: "Y"})
	if len(m.settings.CloudControlRepos) != 1 || m.settings.CloudControlRepos[0].Mode != "full" {
		t.Fatal("single repo confirmed grant missing")
	}
	m.repoCloudControls[0].IsIdVerified = false
	view := m.View().Content
	if !strings.Contains(view, "grant not verified: narrowing only") || !strings.Contains(view, "github.com/acme") || !strings.Contains(view, "github.com/else") {
		t.Fatal("group or stale identity disclosure missing")
	}
}

func TestRepoCloudGroupLocalFallback(t *testing.T) {
	if got := repoCloudGroup(&pb.RepoCloudControl{}); got != "local/(unknown owner)" {
		t.Fatalf("local group = %q", got)
	}
}
