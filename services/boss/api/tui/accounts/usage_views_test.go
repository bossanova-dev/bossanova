package accounts

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestUsageViews_Accounts_AC26_AC27(t *testing.T) {
	account := &pb.AgentAccount{Id: "usage", Label: "Usage account production with a long label", Provider: "claude", Status: "active", SyncedAgentAccountId: "synced-id", Usage: &pb.UsageSnapshot{FetchedAt: timestamppb.Now(), Util_5H: .75, Util_7D: .92, ModelWindows: []*pb.UsageModelWindow{{ModelName: "Fable", Util: .92}, {ModelName: "Other", Util: .2}, {ModelName: "Hidden"}}}}
	m := seedAccountsList(t, &accountsStub{accounts: []*pb.AgentAccount{account}})
	view := ansi.Strip(m.table.View())
	for _, want := range []string{"75%", "92%", strings.Repeat(ui.MeterGlyph, 5), "synced"} {
		if !strings.Contains(view, want) {
			t.Errorf("list missing %q: %s", want, view)
		}
	}
	for _, width := range []int{80, 260, 80} {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		m = updated.(AgentAccountsListModel)
		for _, line := range strings.Split(m.View().Content, "\n") {
			if lipgloss.Width(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
		if width == 80 {
			titles := columnTitles(m.table.Columns())
			if !slices.Contains(titles, "UTIL5H") || !slices.Contains(titles, "UTIL7D") {
				t.Fatalf("narrow columns = %v", titles)
			}
		}
	}
	detail := ansi.Strip(renderAccountDetails(accountDetailRows(account, time.Now()), 0))
	for _, want := range []string{"Usage Fable:", "Usage Other:", "Synced across your daemons", strings.Repeat(ui.MeterGlyph, 10) + " 92%"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "Usage Hidden:") {
		t.Error("zero window visible")
	}
	account.SyncedAgentAccountId = ""
	if accountSyncLabel(account) != "local" || !strings.Contains(renderAccountDetails(accountDetailRows(account, time.Now()), 0), "This daemon only") {
		t.Error("local sync label missing")
	}
	account.Status = "disabled"
	m.accounts = []*pb.AgentAccount{account}
	m.width = 260
	m.rebuildTable()
	if strings.Contains(m.table.View(), lipgloss.NewStyle().Foreground(ui.ColorDanger).Render(strings.Repeat(ui.MeterGlyph, 5))) {
		t.Error("disabled row retains danger accent")
	}
	if !strings.Contains(m.table.View(), ui.RenderMutedMeter(.92, 5)) {
		t.Error("disabled row lost its muted meter fill")
	}
}

func TestAccountEditAlignsDetailAndEditableValues_Accounts_AC27(t *testing.T) {
	account := &pb.AgentAccount{Id: "a", Label: "prod", Provider: "claude", Status: "active", Usage: &pb.UsageSnapshot{FetchedAt: timestamppb.Now(), Util_5H: .27}}
	view := ansi.Strip(newAccountEditModel(&stubAccountEditClient{}, account).View().Content)
	col := -1
	for _, key := range []string{"Provider:", "Credential check:", "Usage 5h:", "Label:", "Priority:"} {
		got, ok := valueColumn(view, key)
		if !ok {
			t.Fatalf("view has no %q row:\n%s", key, view)
		}
		if col >= 0 && got != col {
			t.Errorf("%q value starts at column %d, want %d:\n%s", key, got, col, view)
		}
		col = got
	}
}

// valueColumn is the terminal column where the value of the first line
// carrying key starts.
func valueColumn(view, key string) (int, bool) {
	for _, line := range strings.Split(view, "\n") {
		before, after, found := strings.Cut(line, key)
		if !found {
			continue
		}
		value := strings.TrimLeft(after, " ")
		return lipgloss.Width(before + key + after[:len(after)-len(value)]), true
	}
	return 0, false
}
