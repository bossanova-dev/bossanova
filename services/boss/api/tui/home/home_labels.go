package home

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

type homeLabelClient interface {
	ListSessionLabels(context.Context) ([]*pb.SessionLabel, error)
	CreateSessionLabel(context.Context, *pb.CreateSessionLabelRequest) (*pb.SessionLabel, error)
	SetSessionLabels(context.Context, string, []string) (*pb.Session, error)
}

type labelsLoadedMsg struct {
	SessionID string
	Labels    []*pb.SessionLabel
	Err       error
}

type sessionLabelsUpdatedMsg struct {
	Session *pb.Session
	Err     error
}

type labelCreatedAndAppliedMsg struct {
	Label   *pb.SessionLabel
	Session *pb.Session
	Err     error
}

// LabelPickerState holds the TUI Linear-style label picker for one session.
type LabelPickerState struct {
	active       bool
	sessionID    string
	sessionTitle string
	allLabels    []*pb.SessionLabel
	selectedIDs  map[string]bool
	query        string
	cursor       int
	loading      bool
	errText      string
}

func (p LabelPickerState) Active() bool { return p.active }

func (p LabelPickerState) filtered() []*pb.SessionLabel {
	q := strings.ToLower(strings.TrimSpace(p.query))
	if q == "" {
		return p.allLabels
	}
	out := make([]*pb.SessionLabel, 0, len(p.allLabels))
	for _, l := range p.allLabels {
		if strings.Contains(strings.ToLower(l.GetName()), q) {
			out = append(out, l)
		}
	}
	return out
}

func (p LabelPickerState) canCreateInline() bool {
	q := strings.TrimSpace(p.query)
	if q == "" {
		return false
	}
	for _, l := range p.allLabels {
		if strings.EqualFold(l.GetName(), q) {
			return false
		}
	}
	return true
}

func (p LabelPickerState) itemCount() int {
	n := len(p.filtered())
	if p.canCreateInline() {
		n++
	}
	return n
}

func (p LabelPickerState) LineCount() int {
	if !p.active {
		return 0
	}
	rows := min(max(p.itemCount(), 1), 6)
	return 3 + rows
}

func (p LabelPickerState) Footer(_ int) string {
	var b strings.Builder
	header := fmt.Sprintf("Labels for %s", p.sessionTitle)
	b.WriteString("\n  ")
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(header))
	b.WriteString("  ")
	if p.query == "" {
		b.WriteString(lipgloss.NewStyle().Foreground(ui.ColorMuted).Render("(type to search or create, space/enter toggle, esc close)"))
	} else {
		b.WriteString(lipgloss.NewStyle().Foreground(ui.ColorInfo).Render("/" + p.query))
	}
	b.WriteString("\n")
	if p.loading {
		b.WriteString("  Loading labels…\n")
		return b.String()
	}
	if p.errText != "" {
		b.WriteString("  ")
		b.WriteString(lipgloss.NewStyle().Foreground(ui.ColorDanger).Render(p.errText))
		b.WriteString("\n")
	}
	items := p.filtered()
	total := p.itemCount()
	if total == 0 {
		b.WriteString("  ")
		b.WriteString(lipgloss.NewStyle().Foreground(ui.ColorMuted).Render("No labels yet — type a name and press enter to create one."))
		b.WriteString("\n")
		return b.String()
	}
	maxShow := 6
	start := 0
	if p.cursor >= maxShow {
		start = p.cursor - maxShow + 1
	}
	end := min(start+maxShow, total)
	for idx := start; idx < end; idx++ {
		prefix := "  "
		if idx == p.cursor {
			prefix = "▸ "
		}
		if idx < len(items) {
			l := items[idx]
			check := "[ ]"
			if p.selectedIDs[l.GetId()] {
				check = "[x]"
			}
			dot := renderColoredDot(l.GetColor())
			line := fmt.Sprintf("  %s%s %s %s", prefix, check, dot, l.GetName())
			b.WriteString(line)
			b.WriteString("\n")
		} else {
			q := strings.TrimSpace(p.query)
			fmt.Fprintf(&b, "  %s+ Create label %q\n", prefix, q)
		}
	}
	return b.String()
}

func renderColoredDot(hexColor string) string {
	c := strings.TrimSpace(hexColor)
	if c == "" {
		c = models.DefaultLabelColor
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render("●")
}

func renderSessionLabelsInline(labels []*pb.SessionLabel) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	muted := lipgloss.NewStyle().Foreground(ui.ColorMuted)
	for _, l := range labels {
		parts = append(parts, renderColoredDot(l.GetColor())+" "+muted.Render(l.GetName()))
	}
	return strings.Join(parts, "  ")
}

func (h Model) startLabelPicker() (Model, tea.Cmd, bool) {
	sess := h.SelectedSession()
	if sess == nil {
		return h, nil, true
	}
	lc, ok := h.Client.(homeLabelClient)
	if !ok {
		h.setStatus("Labels are not supported by this daemon connection", statusFailure)
		return h, nil, true
	}
	selected := make(map[string]bool, len(sess.GetLabels()))
	for _, l := range sess.GetLabels() {
		selected[l.GetId()] = true
	}
	title := sess.GetTitle()
	if title == "" {
		title = sess.GetBranchName()
	}
	h.LabelPicker = LabelPickerState{
		active:       true,
		sessionID:    sess.GetId(),
		sessionTitle: title,
		selectedIDs:  selected,
		loading:      true,
	}
	h.clearStatus()
	h.Table.SetHeight(h.TableHeight())
	ctx := h.Ctx
	sessionID := sess.GetId()
	return h, func() tea.Msg {
		labels, err := lc.ListSessionLabels(ctx)
		return labelsLoadedMsg{SessionID: sessionID, Labels: labels, Err: err}
	}, true
}

func (h Model) handleLabelsLoaded(msg labelsLoadedMsg) Model {
	if !h.LabelPicker.active || h.LabelPicker.sessionID != msg.SessionID {
		return h
	}
	h.LabelPicker.loading = false
	if msg.Err != nil {
		h.LabelPicker.errText = msg.Err.Error()
	} else {
		h.LabelPicker.allLabels = msg.Labels
		h.LabelPicker.cursor = 0
	}
	h.Table.SetHeight(h.TableHeight())
	return h
}

func (h Model) handleSessionLabelsUpdated(msg sessionLabelsUpdatedMsg) Model {
	if msg.Err != nil {
		if h.LabelPicker.active {
			h.LabelPicker.errText = msg.Err.Error()
		} else {
			h.setStatus(fmt.Sprintf("Failed to update labels: %v", msg.Err), statusFailure)
		}
		return h
	}
	if msg.Session != nil {
		for i, s := range h.Sessions {
			if s.GetId() == msg.Session.GetId() {
				h.Sessions[i].Labels = msg.Session.GetLabels()
				break
			}
		}
		h.BuildTableRows()
	}
	return h
}

func (h Model) handleLabelCreatedAndApplied(msg labelCreatedAndAppliedMsg) Model {
	if msg.Err != nil {
		h.LabelPicker.errText = msg.Err.Error()
		return h
	}
	if msg.Label != nil && h.LabelPicker.active {
		h.LabelPicker.allLabels = append(h.LabelPicker.allLabels, msg.Label)
		h.LabelPicker.selectedIDs[msg.Label.GetId()] = true
		h.LabelPicker.query = ""
		h.LabelPicker.cursor = 0
	}
	if msg.Session != nil {
		for i, s := range h.Sessions {
			if s.GetId() == msg.Session.GetId() {
				h.Sessions[i].Labels = msg.Session.GetLabels()
				break
			}
		}
		h.BuildTableRows()
	}
	h.Table.SetHeight(h.TableHeight())
	return h
}

func (h Model) handleLabelPickerKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		h.LabelPicker = LabelPickerState{}
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	case "up", "ctrl+p":
		if h.LabelPicker.cursor > 0 {
			h.LabelPicker.cursor--
		}
		return h, nil
	case "down", "ctrl+n":
		if n := h.LabelPicker.itemCount(); h.LabelPicker.cursor+1 < n {
			h.LabelPicker.cursor++
		}
		return h, nil
	case "backspace":
		if len(h.LabelPicker.query) > 0 {
			r := []rune(h.LabelPicker.query)
			h.LabelPicker.query = string(r[:len(r)-1])
			h.LabelPicker.cursor = 0
			h.Table.SetHeight(h.TableHeight())
		}
		return h, nil
	case " ", "enter":
		return h.activateLabelPickerCursor()
	default:
		if len([]rune(key)) == 1 && key != " " {
			h.LabelPicker.query += key
			h.LabelPicker.cursor = 0
			h.LabelPicker.errText = ""
			h.Table.SetHeight(h.TableHeight())
		}
		return h, nil
	}
}

func (h Model) activateLabelPickerCursor() (Model, tea.Cmd) {
	lc, ok := h.Client.(homeLabelClient)
	if !ok {
		return h, nil
	}
	items := h.LabelPicker.filtered()
	ctx := h.Ctx
	sessionID := h.LabelPicker.sessionID

	if h.LabelPicker.cursor < len(items) {
		chosen := items[h.LabelPicker.cursor]
		if h.LabelPicker.selectedIDs[chosen.GetId()] {
			delete(h.LabelPicker.selectedIDs, chosen.GetId())
		} else {
			h.LabelPicker.selectedIDs[chosen.GetId()] = true
		}
		ids := make([]string, 0, len(h.LabelPicker.selectedIDs))
		for id := range h.LabelPicker.selectedIDs {
			ids = append(ids, id)
		}
		return h, func() tea.Msg {
			sess, err := lc.SetSessionLabels(ctx, sessionID, ids)
			return sessionLabelsUpdatedMsg{Session: sess, Err: err}
		}
	}
	if h.LabelPicker.canCreateInline() {
		name := strings.TrimSpace(h.LabelPicker.query)
		color := models.LabelColorPresets[len(h.LabelPicker.allLabels)%len(models.LabelColorPresets)].Hex
		ids := make([]string, 0, len(h.LabelPicker.selectedIDs)+1)
		for id := range h.LabelPicker.selectedIDs {
			ids = append(ids, id)
		}
		return h, func() tea.Msg {
			lbl, err := lc.CreateSessionLabel(ctx, &pb.CreateSessionLabelRequest{
				Name:  name,
				Color: color,
			})
			if err != nil {
				return labelCreatedAndAppliedMsg{Err: err}
			}
			ids = append(ids, lbl.GetId())
			sess, err := lc.SetSessionLabels(ctx, sessionID, ids)
			return labelCreatedAndAppliedMsg{Label: lbl, Session: sess, Err: err}
		}
	}
	return h, nil
}
