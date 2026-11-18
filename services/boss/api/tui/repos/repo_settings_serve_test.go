package repos

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestRepoSettingsServeCommand_RepoSettings_AC250601(t *testing.T) {
	for _, value := range []string{"make serve", ""} {
		t.Run(value, func(t *testing.T) {
			stub := &stubRepoClient{repos: []*pb.Repo{{Id: "repo-1", DisplayName: "Widgets", ServeScript: proto.String("pnpm dev")}}, updated: &pb.Repo{Id: "repo-1"}}
			m := initSettings(t, stub)
			if !strings.Contains(m.View().Content, "Serve command") || !strings.Contains(m.View().Content, "pnpm dev") {
				t.Fatal("serve command is not rendered with its stored value")
			}
			cursorToRow(t, &m, repoSettingsRowSetupScript)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			m = updated.(RepoSettingsModel)
			updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(RepoSettingsModel)
			// Clear and replace through real editor messages, then save across the client boundary.
			for _, key := range []tea.KeyPressMsg{{Code: 'a', Mod: tea.ModCtrl}, {Code: 'k', Mod: tea.ModCtrl}, {Code: 'm', Text: value}} {
				updated, _ = m.Update(key)
				m = updated.(RepoSettingsModel)
			}
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil {
				t.Fatal("saving serve command returned no request")
			}
			cmd()
			if stub.updateReq == nil || stub.updateReq.ServeScript == nil || *stub.updateReq.ServeScript != value {
				t.Fatalf("UpdateRepo = %v, want serve command %q", stub.updateReq, value)
			}

		})
	}
}
