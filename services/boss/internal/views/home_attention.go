package views

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
func (h HomeModel) hasAttentionBoard() bool {
	for _, s := range h.sessions {
		if s.GetAttention() != nil {
			return true
		}
	}
	return false
}

// showsSections reports whether the board titles its groups. A title only
// separates two groups, so a board with one group shows none.
func (h HomeModel) showsSections() bool {
	if !h.hasAttentionBoard() {
		return false
	}
	var needs, progress bool
	for _, s := range h.boardSessions() {
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
func (h HomeModel) sectionRows(sessions []*pb.Session, i int, sectioned bool) int {
	if !sectioned || (i > 0 && attention.NeedsYou(sessions[i-1].GetAttention().GetKind()) == attention.NeedsYou(sessions[i].GetAttention().GetKind())) {
		return 0
	}
	return sectionHeaderRows
}

// sectionTitleCell is the styled group title buildTableRows lays out and
// boardView finds again.
func (h HomeModel) sectionTitleCell(sessions []*pb.Session, i int) string {
	return styleStatusInfo.Render(h.sectionTitle(sessions, i))
}

// boardView is the table with each group's title row redrawn as a rule from
// the REPO column's left edge to the table's right edge, the title centred in
// it. bubbles truncates every cell to its column, so buildTableRows lays the
// bare title out in NAME, which never narrows below it, and this finds the row
// by that cell.
func (h HomeModel) boardView() string {
	view := h.table.View()
	if !h.showsSections() {
		return view
	}
	shift := 0
	for _, c := range h.table.Columns() {
		if c.Title == "REPO" {
			shift = c.Width + tableColumnGap
		}
	}
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		for _, title := range []string{"NEEDS YOU", "IN PROGRESS"} {
			if !strings.Contains(line, styleStatusInfo.Render(title)) {
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
		return styleStatusInfo.Render(ansi.Truncate(title, max(0, width), "…"))
	}
	return styleStatusMuted.Render(strings.Repeat("─", side/2)) + " " + styleStatusInfo.Render(title) + " " + styleStatusMuted.Render(strings.Repeat("─", side-side/2))
}

func (h HomeModel) sectionTitle(sessions []*pb.Session, i int) string {
	if attention.NeedsYou(sessions[i].GetAttention().GetKind()) {
		return "NEEDS YOU"
	}
	return "IN PROGRESS"
}

func (h HomeModel) renderAttentionStatus(a *pb.SessionAttention) string {
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		return styleStatusDanger.Render("fix")
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		return styleStatusWarning.Render("? question")
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		return styleStatusWarning.Render("review")
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		if a.GetIsUnverified() {
			return styleStatusSuccess.Render("✓ ready")
		}
		return styleStatusSuccess.Render("✓ merge")
	default:
		return h.renderProgressStatus(a.GetProgressLabel())
	}
}

// renderProgressStatus styles an in-progress label the way the display status
// styles the same state: stopped muted, idle in warning, running checks as a
// warning spinner, a park on an external event as an info spinner, and live
// work as a success spinner.
func (h HomeModel) renderProgressStatus(label string) string {
	style, spin := styleStatusSuccess, true
	switch {
	case label == "stopped":
		style, spin = styleStatusMuted, false
	case label == "idle":
		style, spin = styleStatusWarning, false
	case strings.HasPrefix(label, "checking "):
		style = styleStatusWarning
	case strings.HasPrefix(label, "waiting"):
		style = styleStatusInfo
	}
	if spin {
		label = h.spinner.View() + label
	}
	return style.Render(ansi.Truncate(label, homeStatusWidth, "…"))
}

// homeAttentionSentence is the sentence Home shows under a Needs-you row. A
// MERGE sentence only restates the row's merge status cell, so Home drops it;
// the API still carries it for the details view and notifications.
func homeAttentionSentence(a *pb.SessionAttention) string {
	if !attention.NeedsYou(a.GetKind()) || a.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE {
		return ""
	}
	return strings.Join(strings.Fields(a.GetSentence()), " ")
}

func attentionSentenceLines(s *pb.Session, width int) []string {
	text := homeAttentionSentence(s.GetAttention())
	if text == "" {
		return nil
	}
	lines := strings.Split(ansi.Hardwrap(ansi.Wrap(text, max(1, width), ""), max(1, width), false), "\n")
	if len(lines) > 4 {
		lines = lines[:4]
		lines[3] = ansi.Truncate(lines[3], max(1, width)-1, "") + "…"
	}
	return lines
}

func (h HomeModel) attentionSentenceWidth() int {
	for _, c := range h.table.Columns() {
		if c.Title == "NAME" {
			return c.Width
		}
	}
	return 60
}
