package home

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attentionBoard preserves the daemon's manual order within In progress. A
// session whose PR merged or closed stays there, struck through, until it is
// archived: hiding it lost sessions whose chat was still in use after the PR
// closed.
func attentionBoard(sessions []*pb.Session) []*pb.Session {
	out := make([]*pb.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.GetArchivedAt() == nil {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b *pb.Session) int {
		aa, ba := a.GetAttention(), b.GetAttention()
		an, bn := attention.NeedsYou(aa.GetKind()), attention.NeedsYou(ba.GetKind())
		if an != bn {
			if an {
				return -1
			}
			return 1
		}
		if !an {
			return 0
		}
		af, bf := aa.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX, ba.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX
		if af != bf {
			if af {
				return -1
			}
			return 1
		}
		return aa.GetSince().AsTime().Compare(ba.GetSince().AsTime())
	})
	return out
}

// Legacy-only boards retain their layout against an older daemon.
func (h Model) hasAttentionBoard() bool {
	for _, s := range h.Sessions {
		if s.GetAttention() != nil {
			return true
		}
	}
	return false
}

// showsSections reports whether the board titles its groups. A title only
// separates two groups, so a board with one group shows none.
func (h Model) showsSections() bool {
	if !h.hasAttentionBoard() {
		return false
	}
	var needs, progress bool
	for _, s := range h.BoardSessions() {
		if attention.NeedsYou(s.GetAttention().GetKind()) {
			needs = true
		} else {
			progress = true
		}
	}
	return needs && progress
}

// sectionHeaderRows is how many non-selectable rows open a group: a blank
// spacer, then the title. Every cursor and height helper counts it through
// sectionRows, so the spacer can never strand the cursor.
const sectionHeaderRows = 2

// sectionRows returns how many rows open session i's group: none when the
// board is untitled or i continues the group above it.
func (h Model) sectionRows(sessions []*pb.Session, i int, sectioned bool) int {
	if !sectioned || (i > 0 && attention.NeedsYou(sessions[i-1].GetAttention().GetKind()) == attention.NeedsYou(sessions[i].GetAttention().GetKind())) {
		return 0
	}
	return sectionHeaderRows
}

// sectionTitleCell is the styled group title buildTableRows lays out and
// boardView finds again.
func (h Model) sectionTitleCell(sessions []*pb.Session, i int) string {
	return ui.StyleEmphasis.Render(h.sectionTitle(sessions, i))
}

// boardView is the table with each group's title row redrawn as a rule from
// the REPO column's left edge to the table's right edge, the title centred in
// it. bubbles truncates every cell to its column, so buildTableRows lays the
// bare title out in NAME, which never narrows below it, and this finds the row
// by that cell.
func (h Model) boardView() string {
	view := h.Table.View()
	if !h.showsSections() {
		return view
	}
	shift := 0
	for _, c := range h.Table.Columns() {
		if c.Title == "REPO" {
			shift = c.Width + ui.TableColumnGap
		}
	}
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		for _, title := range []string{"NEEDS YOU", "IN PROGRESS"} {
			if !strings.Contains(line, ui.StyleEmphasis.Render(title)) {
				continue
			}
			plain := ansi.Strip(line)
			left := max(0, strings.Index(plain, title)-shift)
			lines[i] = plain[:left] + sectionRule(title, ansi.StringWidth(plain)-left)
		}
	}
	return strings.Join(lines, "\n")
}

// sectionRule centres title in a muted light rule width cells wide. Without
// room for a rule on both sides it shows the title alone, cut to width.
func sectionRule(title string, width int) string {
	side := width - lipgloss.Width(title) - 2
	if side < 2 {
		return ui.StyleEmphasis.Render(ansi.Truncate(title, max(0, width), "…"))
	}
	return ui.StyleStatusMuted.Render(strings.Repeat("─", side/2)) + " " + ui.StyleEmphasis.Render(title) + " " + ui.StyleStatusMuted.Render(strings.Repeat("─", side-side/2))
}

func (h Model) sectionTitle(sessions []*pb.Session, i int) string {
	if attention.NeedsYou(sessions[i].GetAttention().GetKind()) {
		return "NEEDS YOU"
	}
	return "IN PROGRESS"
}

// renderAttentionStatus styles a row's attention cell. working says an agent
// chat of the row's session is working now (the session's served display label).
// An outcome the row names (fix, review, ready) keeps its words and colour, but
// the spinner takes the place of its leading glyph: the PR may be ready, but the
// agent is still at it. A verified merge keeps its check mark, because the PR
// is mergeable whatever the agent is doing. A question stays as it is, because
// the agent is waiting on the operator.
func (h Model) renderAttentionStatus(a *pb.SessionAttention, working bool) string {
	lead := func(resting string) string {
		if working {
			return h.Spinner.View()
		}
		return resting
	}
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		return ui.StyleStatusDanger.Render(lead("") + "fix")
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		return ui.StyleStatusWarning.Render("? question")
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		return ui.StyleStatusWarning.Render(lead("") + "review")
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		if a.GetIsUnverified() {
			return ui.StyleStatusSuccess.Render(lead("✓ ") + "ready")
		}
		return ui.StyleStatusSuccess.Render("✓ merge")
	default:
		return h.renderProgressStatus(a.GetProgressLabel())
	}
}

// renderProgressStatus styles an in-progress label the way the display status
// styles the same state: stopped muted, idle in warning, running checks as a
// warning spinner, a session still starting or a park on an external event as
// an info spinner, and live work as a success spinner.
func (h Model) renderProgressStatus(label string) string {
	style, spin := ui.StyleStatusSuccess, true
	switch {
	case label == "stopped", label == attention.VerifyUnknownLabel:
		style, spin = ui.StyleStatusMuted, false
	case label == "idle", label == attention.OutsideVerifyLabelPrefix, strings.HasPrefix(label, attention.OutsideVerifyLabelPrefix+":"):
		style, spin = ui.StyleStatusWarning, false
	case strings.HasPrefix(label, "checking "):
		style = ui.StyleStatusWarning
	case label == attention.InitializingLabel, attention.IsWaitLabel(label):
		style = ui.StyleStatusInfo
	}
	if spin {
		label = h.Spinner.View() + label
	}
	return style.Render(ansi.Truncate(label, homeStatusWidth, "…"))
}
