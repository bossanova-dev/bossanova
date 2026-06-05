package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestActionBar(t *testing.T) {
	tests := []struct {
		name   string
		groups [][]string
		want   string // substring that must appear in rendered output
	}{
		{
			name:   "single group",
			groups: [][]string{{"[q]uit"}},
			want:   "[q]uit",
		},
		{
			name:   "two groups separated by dot",
			groups: [][]string{{"[enter] select", "[a]rchive"}, {"[q]uit"}},
			want:   "[enter] select  [a]rchive · [q]uit",
		},
		{
			name:   "three groups",
			groups: [][]string{{"[enter] select"}, {"[n]ew", "[r]epos"}, {"[q]uit"}},
			want:   "[enter] select · [n]ew  [r]epos · [q]uit",
		},
		{
			name:   "empty groups are skipped",
			groups: [][]string{{}, {"[a]dd"}, {}, {"[esc] back"}},
			want:   "[a]dd · [esc] back",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := actionBar(tt.groups...)
			if !strings.Contains(got, tt.want) {
				t.Errorf("actionBar() = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestRenderBannerUsesBuildVersionVerbatim(t *testing.T) {
	oldVersion := buildinfo.Version
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
	})

	tests := []struct {
		name    string
		version string
		want    string
		notWant string
	}{
		{
			name:    "release tag",
			version: "v1.29.1",
			want:    "v1.29.1",
			notWant: "vv1.29.1",
		},
		{
			name:    "dev build",
			version: "dev",
			want:    "dev",
			notWant: "vdev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buildinfo.Version = tt.version
			got := renderBanner(ViewHome, bannerOpts{})
			if !strings.Contains(got, tt.want) {
				t.Fatalf("renderBanner() missing %q in %q", tt.want, got)
			}
			if strings.Contains(got, tt.notWant) {
				t.Fatalf("renderBanner() contained %q in %q", tt.notWant, got)
			}
		})
	}
}

func TestCLIColumnsWidth(t *testing.T) {
	// Each column contributes its width plus 2 chars of cell padding (1 per side).
	tests := []struct {
		name string
		cols []table.Column
		want int
	}{
		{name: "empty", cols: nil, want: 0},
		{name: "single", cols: []table.Column{{Width: 3}}, want: 5},
		{name: "multiple", cols: []table.Column{{Width: 3}, {Width: 5}, {Width: 10}}, want: 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CLIColumnsWidth(tt.cols); got != tt.want {
				t.Errorf("CLIColumnsWidth(%v) = %d, want %d", tt.cols, got, tt.want)
			}
		})
	}
}

// TestBannerOverhead ties the layout constants to reality: renderBanner must
// emit exactly bannerHeight lines (the padded two-line banner), and the
// bannerOverhead used by every table-height calculation (chatpicker, home,
// cron_list, repo_list, trash, ...) must add the one trailing newline that
// App.View appends. A drift in either misclamps every table on the screen.
func TestBannerOverhead(t *testing.T) {
	banner := renderBanner(ViewHome, bannerOpts{})
	gotLines := strings.Count(banner, "\n") + 1
	if gotLines != bannerHeight {
		t.Errorf("renderBanner emitted %d lines, want bannerHeight = %d", gotLines, bannerHeight)
	}
	if got, want := bannerOverhead, bannerHeight+1; got != want {
		t.Errorf("bannerOverhead = %d, want bannerHeight+1 = %d", got, want)
	}
}

// TestRenderBannerBranches exercises the non-default branches of renderBanner's
// switch: the merged/closed session title (muted strikethrough PR + tracker),
// the active session title (live PR + tracker link), the repo banner, and the
// explicit line1/line2 override. Each assertion is chosen so that negating the
// branch guard would drop the expected payload and fail the test.
func TestRenderBannerBranches(t *testing.T) {
	prNum := int32(42)
	prURL := "https://github.com/owner/repo/pull/42"
	trackerURL := "https://linear.app/issue/FRE-7"

	t.Run("merged session title", func(t *testing.T) {
		sess := &pb.Session{
			Title:         "[FRE-7] Fix the thing",
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			PrNumber:      &prNum,
			PrUrl:         &prURL,
			TrackerId:     strPtr("FRE-7"),
			TrackerUrl:    &trackerURL,
		}
		banner := renderBanner(ViewChatPicker, bannerOpts{session: sess})
		if !strings.Contains(banner, "#42") {
			t.Fatalf("merged banner missing PR label #42: %q", banner)
		}
		if !strings.Contains(banner, prURL) {
			t.Fatalf("merged banner missing PR url %q: %q", prURL, banner)
		}
		if !strings.Contains(banner, "Fix the thing") {
			t.Fatalf("merged banner missing title text: %q", banner)
		}
		// Merged rows use the muted-strikethrough SGR envelope (#626262 + strike).
		if !strings.Contains(banner, "38;2;98;98;98") {
			t.Fatalf("merged banner missing muted styling: %q", banner)
		}
	})

	t.Run("active session title via session settings", func(t *testing.T) {
		sess := &pb.Session{
			Title:         "[FRE-7] Live work",
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			PrNumber:      &prNum,
			PrUrl:         &prURL,
			TrackerId:     strPtr("FRE-7"),
			TrackerUrl:    &trackerURL,
		}
		banner := renderBanner(ViewSessionSettings, bannerOpts{session: sess})
		if !strings.Contains(banner, "#42") {
			t.Fatalf("active banner missing PR label #42: %q", banner)
		}
		if !strings.Contains(banner, "Live work") {
			t.Fatalf("active banner missing title text: %q", banner)
		}
		// Active (non-merged) rows must NOT use the muted-strikethrough envelope.
		if strings.Contains(banner, "38;2;98;98;98;58;2;98;98;98;9;4m") {
			t.Fatalf("active banner unexpectedly muted: %q", banner)
		}
	})

	t.Run("repo banner with home tilde", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		local := filepath.Join(home, "code", "my-repo")
		repo := &pb.Repo{DisplayName: "my-repo", LocalPath: local}
		banner := renderBanner(ViewHome, bannerOpts{repo: repo})
		if !strings.Contains(banner, "my-repo") {
			t.Fatalf("repo banner missing display name: %q", banner)
		}
		// The home prefix must be collapsed to ~, proving the UserHomeDir branch ran.
		if !strings.Contains(banner, filepath.Join("~", "code", "my-repo")) {
			t.Fatalf("repo banner did not collapse home to ~: %q", banner)
		}
		if strings.Contains(banner, home+string(filepath.Separator)+"code") {
			t.Fatalf("repo banner leaked absolute home path: %q", banner)
		}
	})

	t.Run("explicit line1 override", func(t *testing.T) {
		banner := renderBanner(ViewHome, bannerOpts{line1: "Custom Heading", line2: "subtitle text"})
		if !strings.Contains(banner, "Custom Heading") {
			t.Fatalf("override banner missing line1: %q", banner)
		}
		if !strings.Contains(banner, "subtitle text") {
			t.Fatalf("override banner missing line2: %q", banner)
		}
		if strings.Contains(banner, "Bossanova") {
			t.Fatalf("override banner fell through to default: %q", banner)
		}
	})
}

func TestSessionTitleStatusUsesHydratedDisplayComposite(t *testing.T) {
	sess := &pb.Session{
		Id:             "sess-1",
		Title:          "Fix status mismatch",
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_REJECTED,
		DisplayLabel:   "checking",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		DisplaySpinner: true,
	}
	sp := newStatusSpinner()

	homeStatus := (HomeModel{spinner: sp}).renderSessionStatus(sess)
	banner := renderBanner(ViewChatPicker, bannerOpts{
		session: sess,
		spinner: sp,
	})

	if !strings.Contains(homeStatus, "checking") {
		t.Fatalf("home status = %q, want checking", homeStatus)
	}
	if strings.Contains(homeStatus, "rejected") {
		t.Fatalf("home status = %q, must not render stale rejected label", homeStatus)
	}
	if !strings.Contains(banner, "("+homeStatus+")") {
		t.Fatalf("banner = %q, want title status %q", banner, homeStatus)
	}
	if strings.Contains(banner, "rejected") {
		t.Fatalf("banner = %q, must not render stale rejected label", banner)
	}
}
