package newsession

import (
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPickerTableHeightsReserveActiveFilterFooter(t *testing.T) {
	filter := ui.NewListFilter()
	filter.Activate()
	const (
		width  = 40
		height = 24
	)
	footerLines := prSelectActionBarLineCount(width, filter, true)
	if footerLines < 2 {
		t.Fatalf("filter footer line count = %d, want folded action bar", footerLines)
	}

	model := Model{
		prsFiltered:    make([]int, 100),
		issuesFiltered: make([]int, 100),
		prFilter:       filter,
		issueFilter:    filter,
		width:          width,
		height:         height,
	}
	want := ui.ClampedTableHeight(100, height, ui.BannerOverhead+5+footerLines+filter.Height())
	if got := model.prTableHeight(); got != want {
		t.Fatalf("prTableHeight() = %d, want %d", got, want)
	}
	if got := model.issueTableHeight(); got != want {
		t.Fatalf("issueTableHeight() = %d, want %d", got, want)
	}
}
func TestRepositoryPickerTableHeightReservesFoldedFooter(t *testing.T) {
	const (
		width  = 20
		height = 24
	)
	footerLines := ui.ActionBarLineCount(width, []string{"[enter] select"}, []string{"[esc] back"})
	if footerLines < 2 {
		t.Fatalf("repository picker footer line count = %d, want folded action bar", footerLines)
	}
	model := Model{repos: make([]*pb.Repo, 100), width: width, height: height}
	want := ui.ClampedTableHeight(len(model.repos), height, ui.BannerOverhead+5+footerLines)
	if got := model.repoTableHeight(); got != want {
		t.Fatalf("repoTableHeight() = %d, want %d", got, want)
	}
}
