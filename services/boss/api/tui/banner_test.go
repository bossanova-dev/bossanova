package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/bossalib/kit/buildinfo"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

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
			got := renderBanner(ui.ViewHome, bannerOpts{})
			if !strings.Contains(got, tt.want) {
				t.Fatalf("renderBanner() missing %q in %q", tt.want, got)
			}
			if strings.Contains(got, tt.notWant) {
				t.Fatalf("renderBanner() contained %q in %q", tt.notWant, got)
			}
		})
	}
}

func TestRenderBannerTruncatesNarrowLinesWithoutStrandingLinks(t *testing.T) {
	trackerURL := "https://linear.app/bossanova-dev/issue/BOS-576"
	prURL := "https://github.com/recurser/bossanova/pull/1721"
	prNumber := int32(1721)
	banner := renderBanner(ui.ViewChatPicker, bannerOpts{
		width: 72,
		session: &pb.Session{
			Title:        "[BOS-576] Wrap every action bar and banner safely on narrow terminal windows",
			TrackerId:    strPtr("BOS-576"),
			TrackerUrl:   strPtr(trackerURL),
			PrNumber:     &prNumber,
			PrUrl:        &prURL,
			WorktreePath: "/very/deep/worktree/path/that/keeps/going/until/it/would/overflow/the/screen",
		},
	})

	if !strings.Contains(banner, "…") {
		t.Fatalf("narrow banner did not truncate: %q", banner)
	}
	for _, line := range strings.Split(banner, "\n") {
		if got := ansi.StringWidth(line); got > 72 {
			t.Errorf("banner line is %d columns, want <= 72: %q", got, line)
		}
	}
	if markers := strings.Count(banner, "\x1b]8;;"); markers%2 != 0 {
		t.Errorf("banner has %d OSC 8 markers; truncation left a link open: %q", markers, banner)
	}
}

// TestBannerOverhead ties the layout constants to reality: renderBanner must
// emit exactly bannerHeight lines (the padded two-line banner), and the
// bannerOverhead used by every table-height calculation (chatpicker, home,
// cron_list, repo_list, trash, ...) must add the one trailing newline that
// App.View appends. A drift in either misclamps every table on the screen.
func TestBannerOverhead(t *testing.T) {
	banner := renderBanner(ui.ViewHome, bannerOpts{})
	gotLines := strings.Count(banner, "\n") + 1
	if gotLines != ui.BannerHeight {
		t.Errorf("renderBanner emitted %d lines, want bannerHeight = %d", gotLines, ui.BannerHeight)
	}
	if got, want := ui.BannerOverhead, ui.BannerHeight+1; got != want {
		t.Errorf("bannerOverhead = %d, want bannerHeight+1 = %d", got, want)
	}
}

// TestRenderBannerStrikesAMergedSessionWithAttention pins the header of a
// merged or closed session that still carries attention to the same
// strikethrough PR and title as one without it, and an open session with
// attention to none.
func TestRenderBannerStrikesAMergedSessionWithAttention(t *testing.T) {
	prNum := int32(42)
	prURL := "https://github.com/owner/repo/pull/42"
	for _, tc := range []struct {
		status pb.DisplayStatus
		struck bool
	}{
		{pb.DisplayStatus_DISPLAY_STATUS_MERGED, true},
		{pb.DisplayStatus_DISPLAY_STATUS_CLOSED, true},
		{pb.DisplayStatus_DISPLAY_STATUS_PASSING, false},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			sess := &pb.Session{
				Title:         "Fix the thing",
				DisplayStatus: tc.status,
				PrNumber:      &prNum,
				PrUrl:         &prURL,
				Attention:     &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}},
			}
			c := &attentionStub{}
			a := NewApp(c, nil)
			a.chatPicker = attentionDetails(c, sess)
			a.activeView = ui.ViewChatPicker
			header := strings.SplitN(a.View().Content, "\n", 3)[1]
			for _, text := range []string{"#42", "Fix the thing"} {
				if got := struckThrough(header, text); got != tc.struck {
					t.Errorf("%q struck through = %v, want %v: %q", text, got, tc.struck, header)
				}
			}
		})
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
		banner := renderBanner(ui.ViewChatPicker, bannerOpts{session: sess})
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
		banner := renderBanner(ui.ViewSessionSettings, bannerOpts{session: sess})
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

	t.Run("account label beside worktree", func(t *testing.T) {
		bound := "acct-work"
		// renderBanner collapses any occurrence of $HOME in the worktree path to
		// "~". Pin $HOME to a sentinel that cannot appear in the hard-coded path
		// below so the collapse leaves it verbatim regardless of the ambient
		// $HOME (e.g. CI running with HOME=/tmp), letting us assert on the exact
		// glue between the path and the "· label" separator.
		t.Setenv("HOME", "/nonexistent-home-for-bos-321-test")
		wt := filepath.Join("/tmp", "bos-321", "worktree")
		boundSess := &pb.Session{Title: "Work", AccountId: &bound, WorktreePath: wt}
		banner := renderBanner(ui.ViewChatPicker, bannerOpts{session: boundSess})
		if !strings.Contains(banner, "acct-work") {
			t.Fatalf("bound banner missing account label acct-work: %q", banner)
		}
		// BOS-321: the worktree path must join the "· <label>" separator with exactly
		// one space, not two. The path and the "· "+label are wrapped in separate
		// styleSubtle.Render envelopes, so strip ANSI and assert on the plain glue.
		plain := ansi.Strip(banner)
		if !strings.Contains(plain, wt+" · acct-work") {
			t.Fatalf("worktree line is not single-spaced before ·: %q", plain)
		}
		if strings.Contains(plain, wt+"  · ") {
			t.Fatalf("worktree line reintroduced the double space before ·: %q", plain)
		}

		unboundSess := &pb.Session{Title: "Work"}
		unbound := renderBanner(ui.ViewChatPicker, bannerOpts{session: unboundSess})
		if !strings.Contains(unbound, display.UnmanagedLocalCredentialsShortLabel) {
			t.Fatalf("unbound banner missing unmanaged account label: %q", unbound)
		}
	})

	t.Run("repo banner with home tilde", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		local := filepath.Join(home, "code", "my-repo")
		repo := &pb.Repo{DisplayName: "my-repo", LocalPath: local}
		banner := renderBanner(ui.ViewHome, bannerOpts{repo: repo})
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
		banner := renderBanner(ui.ViewHome, bannerOpts{line1: "Custom Heading", line2: "subtitle text"})
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
	sp := ui.NewStatusSpinner()

	homeStatus := (HomeModel{Spinner: sp}).RenderSessionStatus(sess)
	banner := renderBanner(ui.ViewChatPicker, bannerOpts{
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
