package ui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/bossalib/kit/buildinfo"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// accountBannerLabel renders the bound-account label shown beside the worktree
// line in the session banner. The Session message carries only the account id
// today (no server-provided label), so we surface that id verbatim and fall
// back to "Unmanaged" for an unbound session. Best-effort — never panics.
func AgentAccountBannerLabel(accountID string) string {
	if accountID == "" {
		return display.UnmanagedLocalCredentialsShortLabel
	}
	return accountID
}

func RenderBanner(active View, opts BannerOpts) string {
	// Logo chars per row, matching `npx oh-my-logo "B" dawn --filled --block-font tiny`.
	row1 := []string{" ", "█", "▄", "▄"}
	row2 := []string{" ", "█", "▄", "█"}

	colorize := func(chars []string) string {
		var b strings.Builder
		for i, ch := range chars {
			b.WriteString(lipgloss.NewStyle().Foreground(BannerGradient[i]).Render(ch))
		}
		return b.String()
	}

	var line1, line2 string
	switch {
	case (active == ViewChatPicker || active == ViewSessionSettings) && opts.Session != nil:
		// Session title with clickable tracker ID and PR number. Merged/closed
		// PRs get the same muted strikethrough treatment as merged rows on the
		// home screen, so the banner reflects the terminal state at a glance.
		merged := opts.Session.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			opts.Session.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED
		var title string
		if merged {
			title = RenderMutedTrackerLink(opts.Session, opts.Session.Title)
			if prLink := RenderMutedPRLink(opts.Session); prLink != "" {
				title = prLink + " " + title
			}
		} else {
			title = RenderTrackerLink(opts.Session, opts.Session.Title)
			if prLink := RenderPRLink(opts.Session); prLink != "" {
				title = prLink + " " + title
			}
		}
		displayStatus := RenderDisplayStatus(opts.Session, opts.Spinner)
		if opts.Archiving {
			displayStatus = RenderArchivingStatus(opts.Spinner)
		}
		if displayStatus != "" {
			title += " (" + displayStatus + ")"
		}
		line1 = title

		// Worktree root path, followed by the bound account label (best-effort;
		// "Unmanaged" when the session is unbound).
		wt := opts.Session.GetWorktreePath()
		if home, err := os.UserHomeDir(); err == nil {
			wt = strings.Replace(wt, home, "~", 1)
		}
		line2 = StyleSubtle.Render(wt)
		line2 += " " + StyleSubtle.Render("· "+AgentAccountBannerLabel(opts.Session.GetAccountId()))

	case opts.Repo != nil:
		line1 = opts.Repo.DisplayName
		lp := opts.Repo.LocalPath
		if home, err := os.UserHomeDir(); err == nil {
			lp = strings.Replace(lp, home, "~", 1)
		}
		line2 = StyleSubtle.Render(lp)

	case opts.Line1 != "":
		line1 = opts.Line1
		line2 = opts.Line2

	default:
		line1 = "Bossanova"
		line2 = StyleSubtle.Render(buildinfo.Version)
	}

	logo1 := colorize(row1) + "  "
	logo2 := colorize(row2) + "  "
	if opts.Width > 0 {
		line1 = ansi.Truncate(line1, max(opts.Width-2-ansi.StringWidth(logo1), 0), "…")
		line2 = ansi.Truncate(line2, max(opts.Width-2-ansi.StringWidth(logo2), 0), "…")
	}
	banner := logo1 + line1 + "\n" + logo2 + line2

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}

// bannerOpts carries optional per-screen overrides for the banner.
type BannerOpts struct {
	Session *pb.Session
	Repo    *pb.Repo
	Spinner spinner.Model

	// archiving, when true, overrides the session's PR display status with the
	// animated "archiving" label. Set when the user re-enters a session whose
	// archive is still in flight (the HomeModel tracks that session's ID).
	Archiving bool

	// Screen-specific overrides (used when session/repo are nil).
	Line1 string
	Line2 string
	Width int
}
