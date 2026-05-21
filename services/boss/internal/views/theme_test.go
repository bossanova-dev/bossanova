package views

import (
	"strings"
	"testing"

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
