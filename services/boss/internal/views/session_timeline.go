package views

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type timelineKey struct {
	sessionID      string
	head           string
	kind           pb.AttentionKind
	questionChatID string
	questionSince  time.Time
}
type sessionTimelineMsg struct {
	key      timelineKey
	steps    []*pb.TimelineStep
	question string
}

func (m *ChatPickerModel) syncTimeline() tea.Cmd {
	key := timelineKey{sessionID: m.sessionID, head: m.session.GetPrDisplayHeadSha(), kind: m.session.GetAttention().GetKind()}
	if key.kind == pb.AttentionKind_ATTENTION_KIND_QUESTION {
		key.questionChatID = m.session.GetAttention().GetTargetChatId()
		key.questionSince = m.session.GetAttention().GetSince().AsTime()
	}
	if m.timelineRequested && m.timelineKey == key {
		return nil
	}
	m.timelineRequested = true
	m.timelineKey = key
	m.timeline = nil
	m.fullQuestion = ""
	m.neededOffset = 0
	if m.session.GetAttention() == nil {
		return nil
	}
	c, ctx, id, target := m.client, m.ctx, m.sessionID, m.session.GetAttention().GetTargetChatId()
	return func() tea.Msg {
		steps, err := c.GetSessionTimeline(ctx, id)
		if err != nil {
			slog.Debug("session timeline unavailable", "session", id, "error", err)
			steps = nil
		}
		question := ""
		if key.kind == pb.AttentionKind_ATTENTION_KIND_QUESTION && target != "" {
			resp, err := c.GetChatTranscript(ctx, &pb.GetChatTranscriptRequest{SessionId: id, AgentSessionId: target, MaxMessages: 1})
			if err == nil {
				question = resp.GetFinalAssistantText()
			}
		}
		return sessionTimelineMsg{key: key, steps: steps, question: question}
	}
}

func attentionDetailStatus(s *pb.Session) string {
	a := s.GetAttention()
	kind, verb := "", ""
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		kind, verb = "review", "parked"
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		kind, verb = "fix", "failed"
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		kind, verb = "question", "asked"
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		kind, verb = "merge", "ready"
	case pb.AttentionKind_ATTENTION_KIND_DONE:
		kind = "done"
	default:
		kind = a.GetProgressLabel()
	}
	parts := []string{kind}
	if verb != "" && a.GetSince() != nil {
		parts = append(parts, verb+" "+RelativeTime(a.GetSince().AsTime()))
	}
	if sha := s.GetPrDisplayHeadSha(); sha != "" {
		parts = append(parts, "head "+sha[:min(len(sha), shortIDLen)])
	}
	return strings.Join(parts, " · ")
}

func (m ChatPickerModel) neededQuestion() string {
	a := m.session.GetAttention()
	if !attention.NeedsYou(a.GetKind()) {
		return ""
	}
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		if text := strings.TrimSpace(m.fullQuestion); text != "" {
			return text
		}
		return strings.Trim(a.GetSentence(), "“”")
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		category, _ := attention.ParkCategory(a.GetParkCodes())
		switch category {
		case "UI change":
			if a.GetEvidenceUrl() == "" {
				return "Does this UI change look right?"
			}
			return "Does this UI change look right in the video?"
		case "security":
			return "Is this safe to merge?"
		case "database migration":
			return "Is this migration safe to run?"
		default:
			return "Is this ready to merge?"
		}
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		if len(a.GetParkCodes()) > 0 {
			return blockedParkNeeded(m.session.GetPrNumber(), a)
		}
		return "How should the agent proceed after this failure?"
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		return "Should this PR be merged?"
	default:
		return ""
	}
}

// blockedParkNeeded is what a blocked verify park asks of the operator, whose
// row offers no merge: what keeps it blocked, in plain words (each open
// checklist item its park note recorded, or what the park is about), then the
// two ways forward: fix it through a chat, or override it from the CLI.
func blockedParkNeeded(pr int32, a *pb.SessionAttention) string {
	var lines []string
	if items := a.GetOpenItems(); len(items) > 0 {
		lines = append(lines, "Open in the PR checklist:")
		for _, item := range items {
			lines = append(lines, "  • "+item)
		}
	} else {
		lines = append(lines, "Verify is blocked: "+strings.Join(attention.ParkCategories(a.GetParkCodes()), ", ")+".")
	}
	return strings.Join(append(lines, fmt.Sprintf("Open a chat to fix it, or run boss pr approve %d to override it; verify re-checks the PR before it merges.", pr)), "\n")
}

func (m ChatPickerModel) timelineBlock() string {
	if m.session.GetAttention() == nil || len(m.timeline) == 0 {
		return ""
	}
	// Dated history comes from the daemon. Its current step is replaced with
	// the session's current attention so every kind explains its waiting point.
	var steps []*pb.TimelineStep
	for _, step := range m.timeline {
		if step.GetState() != pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT {
			steps = append(steps, step)
		}
	}
	a := m.session.GetAttention()
	if attention.NeedsYou(a.GetKind()) {
		title := map[pb.AttentionKind]string{pb.AttentionKind_ATTENTION_KIND_FIX: "Waiting for a fix", pb.AttentionKind_ATTENTION_KIND_QUESTION: "Waiting for your answer", pb.AttentionKind_ATTENTION_KIND_REVIEW: "Parked for you", pb.AttentionKind_ATTENTION_KIND_MERGE: "Waiting for merge"}[a.GetKind()]
		detail := a.GetSentence()
		if a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW {
			detail, _ = attention.ParkCategory(a.GetParkCodes())
		}
		steps = append(steps, &pb.TimelineStep{At: a.GetSince(), Title: title, Detail: detail, State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT})
	} else {
		for _, step := range m.timeline {
			if step.GetState() == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT {
				steps = append(steps, step)
			}
		}
	}
	older := max(0, len(steps)-timelineMaxSteps)
	steps = steps[older:]
	titleWidth := 0
	for _, step := range steps {
		titleWidth = max(titleWidth, ansi.StringWidth(step.GetTitle()))
	}
	rows, earlier := foldTimelineSteps(steps, older, time.Now(), titleWidth, m.chatPickerProseAvailWidth(), m.timelineRowBudget())
	lines := []string{"Timeline"}
	if earlier > 0 {
		noun := "steps"
		if earlier == 1 {
			noun = "step"
		}
		lines = append(lines, m.fitProseLine(styleStatusMuted.Render(fmt.Sprintf("  … %d earlier %s", earlier, noun))))
	}
	for _, row := range rows {
		for _, line := range row {
			lines = append(lines, m.fitProseLine(line))
		}
	}
	return strings.Join(lines, "\n")
}

// foldTimelineSteps keeps the most recent steps whose rows fit budget, then
// dates the steps it kept: a folded step may have carried its day's label, and
// the fold may leave a single day. Dropping the date column only widens the
// details, which wordWrap never makes taller, but should a relabelled row come
// out taller anyway the kept steps keep the date column, so the second fold
// never loses a step the first kept. It returns the kept rows and how many
// steps are left out.
func foldTimelineSteps(steps []*pb.TimelineStep, older int, now time.Time, titleWidth, avail, budget int) ([][]string, int) {
	days := timelineDayLabels(steps, now, false)
	_, folded := fitTimelineRows(timelineStepRows(steps, days, titleWidth, avail), older, budget)
	kept := steps[folded-older:]
	rows, earlier := fitTimelineRows(timelineStepRows(kept, timelineDayLabels(kept, now, false), titleWidth, avail), folded, budget)
	if earlier > folded {
		rows, earlier = fitTimelineRows(timelineStepRows(kept, timelineDayLabels(kept, now, days != nil), titleWidth, avail), folded, budget)
	}
	return rows, earlier
}

// timelineStepRows renders each step as its wrapped rows, with a date column
// holding days[i] when days is not nil.
func timelineStepRows(steps []*pb.TimelineStep, days []string, titleWidth, avail int) [][]string {
	rows := make([][]string, 0, len(steps))
	for i, step := range steps {
		var glyph string
		switch step.GetState() {
		case pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT:
			glyph = "●"
		case pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED:
			glyph = "✗"
		default:
			glyph = "✓"
		}
		at := "     "
		if step.GetAt() != nil {
			at = step.GetAt().AsTime().Local().Format("15:04")
		}
		if days != nil {
			at = fmt.Sprintf("%-6s %s", days[i], at)
		}
		detail := step.GetDetail()
		if strings.Contains(detail, "://") {
			detail = ""
		}
		head := fmt.Sprintf("  %s  %s ", at, glyph)
		title := step.GetTitle() + strings.Repeat(" ", titleWidth-ansi.StringWidth(step.GetTitle())) + "  "
		indent := ansi.StringWidth(head)
		// An unsized model has no width to wrap to, and fitProseLine leaves its
		// lines whole.
		parts := []string{strings.Join(strings.Fields(detail), " ")}
		if avail > 0 {
			parts = wordWrap(detail, avail-indent-ansi.StringWidth(title), avail-indent)
		}
		row := []string{strings.TrimRight(head+title+parts[0], " ")}
		for _, part := range parts[1:] {
			row = append(row, strings.Repeat(" ", indent)+part)
		}
		rows = append(rows, row)
	}
	return rows
}

// timelineMaxSteps is the most steps the timeline shows, however tall the
// terminal. A long-running session collects dozens, and the chats below them
// are what the operator came to the screen for.
const timelineMaxSteps = 6

// timelineMaxStepRows caps one step's wrapped detail. A question's full text
// already has its own pageable block below, so the timeline only previews it.
const timelineMaxStepRows = 3

// detailsReservedChats is how many chat rows the details blocks always leave
// the chat table, beyond its header.
const detailsReservedChats = 5

// detailsReservedTableRows is the chat table's header plus every chat row up to
// detailsReservedChats.
func (m ChatPickerModel) detailsReservedTableRows() int {
	return min(len(m.chats), detailsReservedChats) + 1
}

// timelineDayLabels returns, when the steps span more than one local day or
// dated is set, the day each row shows before its time: blank while a row
// stays on the day of the row above, else "Today" or a short date. Otherwise
// it returns nil, so the rows keep the bare time.
func timelineDayLabels(steps []*pb.TimelineStep, now time.Time, dated bool) []string {
	day := func(step *pb.TimelineStep) string {
		if step.GetAt() == nil {
			return ""
		}
		return step.GetAt().AsTime().Local().Format("2006-01-02")
	}
	spans := dated
	first := ""
	for _, step := range steps {
		if d := day(step); d != "" {
			if first == "" {
				first = d
			}
			spans = spans || d != first
		}
	}
	if !spans {
		return nil
	}
	today := now.Local().Format("2006-01-02")
	labels := make([]string, len(steps))
	previous := ""
	for i, step := range steps {
		d := day(step)
		if d == "" || d == previous {
			continue
		}
		previous = d
		labels[i] = step.GetAt().AsTime().Local().Format("Jan _2")
		if d == today {
			labels[i] = "Today"
		}
	}
	return labels
}

// timelineRowBudget is how many rows the timeline may use below its heading.
// The chat table and the answer below are what the operator acts on, so the
// timeline gets only the rows they and the footer leave, matching what
// neededWindow and tableHeight then subtract.
func (m ChatPickerModel) timelineRowBudget() int {
	if m.height <= 0 {
		return (timelineMaxSteps + 1) * timelineMaxStepRows
	}
	// The "Timeline" heading plus the blank row below the details block.
	room := m.height - m.chatListOverhead() - m.detailsReservedTableRows() - 2
	if m.neededQuestion() != "" {
		// The blank row and heading above the answer, the row neededWindow
		// keeps free, and the answer itself.
		room -= 3 + len(m.neededLines())
	}
	return max(1, room)
}

// fitTimelineRows keeps the most recent steps whose wrapped rows fit budget,
// keeping one row for the "earlier steps" line whenever a step is left out;
// older counts steps already left out. The newest step always stays. Each
// step is cut to timelineMaxStepRows, then the longest kept step loses a line
// at a time until the rows fit, and each cut step's last line ends with an
// ellipsis. It returns the kept rows and how many steps are left out.
func fitTimelineRows(rows [][]string, older, budget int) ([][]string, int) {
	trimmed := make([]bool, len(rows))
	for i, row := range rows {
		if len(row) > timelineMaxStepRows {
			rows[i], trimmed[i] = row[:timelineMaxStepRows], true
		}
	}
	start, used := len(rows), 0
	for i := len(rows) - 1; i >= 0; i-- {
		reserve := 0
		if i > 0 || older > 0 {
			reserve = 1
		}
		if start < len(rows) && used+len(rows[i])+reserve > budget {
			break
		}
		start, used = i, used+len(rows[i])
	}
	rows, trimmed = rows[start:], trimmed[start:]
	earlier := older + start
	if earlier > 0 {
		budget--
	}
	for ; used > budget; used-- {
		longest := 0
		for i, row := range rows {
			if len(row) > len(rows[longest]) {
				longest = i
			}
		}
		if len(rows[longest]) <= 1 {
			break
		}
		rows[longest], trimmed[longest] = rows[longest][:len(rows[longest])-1], true
	}
	for i, row := range rows {
		if trimmed[i] {
			row[len(row)-1] += "…"
		}
	}
	return rows, earlier
}

// wordWrap breaks text at whitespace into lines of at most first display cells
// on the first line and rest on the others. A word that does not fit an empty
// line is broken inside itself, so more width never yields more lines. A rest
// below two cells cannot hold a wide character, so the text wraps as if both
// widths were that much wider and fitProseLine truncates the overflow.
func wordWrap(text string, first, rest int) []string {
	words := strings.Fields(text)
	if rest < 2 {
		first, rest = first+2-rest, 2
	}
	lines := []string{""}
	limit := first
	for _, word := range words {
		for word != "" {
			cur := lines[len(lines)-1]
			used := ansi.StringWidth(cur)
			if used > 0 {
				used++
			}
			if used+ansi.StringWidth(word) <= limit {
				lines[len(lines)-1] = strings.TrimPrefix(cur+" "+word, " ")
				break
			}
			if cur == "" {
				if piece := ansi.Truncate(word, max(0, limit), ""); piece != "" {
					lines[len(lines)-1] = piece
					word = ansi.TruncateLeft(word, ansi.StringWidth(piece), "")
				}
			}
			lines = append(lines, "")
			limit = rest
		}
	}
	return lines
}

func (m ChatPickerModel) attentionDetailsBlock() string {
	var blocks []string
	if timeline := m.timelineBlock(); timeline != "" {
		blocks = append(blocks, timeline)
	}
	if q := m.neededQuestion(); q != "" {
		lines, start, size := m.neededWindow()
		heading := "Needed from you:"
		if len(lines) > size {
			heading = fmt.Sprintf("Needed from you · pgup/pgdown (%d–%d of %d):", start+1, start+size, len(lines))
		}
		// The answer is the one thing on the screen the operator must act on,
		// so it renders bold in the primary foreground, above the faint chrome.
		answer := make([]string, 0, size)
		for _, line := range lines[start : start+size] {
			answer = append(answer, neededIndent+styleEmphasis.Render(line))
		}
		blocks = append(blocks, m.fitProseLine(heading)+"\n"+strings.Join(answer, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

func (m ChatPickerModel) attentionDetailsHeight() int {
	if block := m.attentionDetailsBlock(); block != "" {
		return lipgloss.Height(block) + 1
	}
	return 0
}

// neededIndent sets the answer in from its "Needed from you:" heading.
const neededIndent = "  "

// neededWindow keeps the complete question available while protecting the chat
// table and footer. Only the question scrolls; chat navigation stays independent.
func (m ChatPickerModel) neededWindow() (lines []string, start, size int) {
	lines = m.neededLines()
	size = len(lines)
	if m.height > 0 {
		timelineRows := 0
		if block := m.timelineBlock(); block != "" {
			timelineRows = lipgloss.Height(block) + 2
		}
		size = min(size, max(1, m.height-m.chatListOverhead()-timelineRows-2-m.detailsReservedTableRows()))
	}
	return lines, min(max(0, m.neededOffset), len(lines)-size), size
}

// neededLines is the answer word-wrapped to the block width less its indent.
// A line's own leading spaces, such as a checklist bullet's, carry onto every
// line it wraps into.
func (m ChatPickerModel) neededLines() []string {
	wrap := m.blockWrapWidth()
	width := max(1, wrap-len(neededIndent))
	var lines []string
	for _, paragraph := range strings.Split(m.neededQuestion(), "\n") {
		lead := paragraph[:len(paragraph)-len(strings.TrimLeft(paragraph, " "))]
		// A zero blockWrapWidth leaves the block unconstrained, so each
		// paragraph stays one line.
		wrapped := []string{strings.Join(strings.Fields(paragraph), " ")}
		if wrap > 0 {
			wrapped = wordWrap(paragraph, width-len(lead), width-len(lead))
		}
		for _, line := range wrapped {
			lines = append(lines, lead+line)
		}
	}
	return lines
}
