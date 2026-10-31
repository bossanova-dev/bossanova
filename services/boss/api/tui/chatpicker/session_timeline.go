package chatpicker

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type timelineKey struct {
	chatCount      int
	chatTitles     string
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

func (m *Model) syncTimeline() tea.Cmd {
	key := timelineKey{chatCount: len(m.chats), sessionID: m.sessionID, head: m.session.GetPrDisplayHeadSha(), kind: m.session.GetAttention().GetKind()}
	var titles strings.Builder
	for _, chat := range m.chats {
		fmt.Fprintf(&titles, "%q:%q;", chat.GetAgentSessionId(), chat.GetTitle())
	}
	key.chatTitles = titles.String()
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

func (m Model) neededQuestion() string {
	a := m.session.GetAttention()
	if !attention.NeedsYou(a.GetKind()) {
		return ""
	}
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		if text := strings.TrimSpace(m.fullQuestion); plainMarkdown(text) != "" {
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
			return blockedParkNeeded(a)
		}
		return "How should the agent proceed after this failure?"
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		return "Should this PR be merged?"
	default:
		return ""
	}
}

// blockedParkNeeded is what keeps a blocked verify park from merging, in
// plain words: each open checklist item its park note recorded, or what the
// park is about.
func blockedParkNeeded(a *pb.SessionAttention) string {
	var lines []string
	if items := a.GetOpenItems(); len(items) > 0 {
		lines = append(lines, "Open in the PR checklist:")
		for _, item := range items {
			lines = append(lines, "  • "+item)
		}
	} else {
		lines = append(lines, "Verify is blocked: "+strings.Join(attention.ParkCategories(a.GetParkCodes()), ", ")+".")
	}
	return strings.Join(lines, "\n")
}

// timelineSteps is the session's history, oldest first: the daemon's dated
// steps merged by time with the session's account rotations, including sessions
// without an attention row.
func (m Model) timelineSteps() []*pb.TimelineStep {
	a := m.session.GetAttention()
	if len(m.timeline) == 0 {
		return mergeRotationSteps(nil, m.session.GetRotationEvents())
	}
	// The daemon's current step is replaced with the session's current
	// attention so every kind explains its waiting point, in place and at the
	// daemon's time. A question, a park and a fix carry their own since when
	// the daemon can date them. Otherwise, and for a merge outside a factory
	// repo, since is when the session's state began, which can predate the
	// wait by hours. A timeline fetched without a current step takes the
	// waiting step at attention's since, still in time order.
	steps := slices.Clone(m.timeline)
	if attention.NeedsYou(a.GetKind()) {
		title := map[pb.AttentionKind]string{pb.AttentionKind_ATTENTION_KIND_FIX: "Waiting for a fix", pb.AttentionKind_ATTENTION_KIND_QUESTION: "Waiting for your answer", pb.AttentionKind_ATTENTION_KIND_REVIEW: "Parked for you", pb.AttentionKind_ATTENTION_KIND_MERGE: "Waiting for merge"}[a.GetKind()]
		detail := a.GetSentence()
		if a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW {
			detail, _ = attention.ParkCategory(a.GetParkCodes())
		} else if a.GetKind() == pb.AttentionKind_ATTENTION_KIND_QUESTION {
			detail = plainMarkdown(detail)
		}
		wait := &pb.TimelineStep{At: a.GetSince(), Title: title, Detail: detail, State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT}
		if i := slices.IndexFunc(steps, func(s *pb.TimelineStep) bool {
			return s.GetState() == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
		}); i >= 0 {
			if at := steps[i].GetAt(); at != nil {
				wait.At = at
			}
			steps[i] = wait
		} else {
			at := slices.IndexFunc(steps, func(s *pb.TimelineStep) bool {
				return wait.GetAt() != nil && s.GetAt() != nil && s.GetAt().AsTime().After(wait.GetAt().AsTime())
			})
			if at < 0 {
				at = len(steps)
			}
			steps = slices.Insert(steps, at, wait)
		}
	}
	return mergeRotationSteps(steps, m.session.GetRotationEvents())
}

// rotationStepTitle names every rotation on the timeline; its detail says what
// the rotation did.
const rotationStepTitle = "Account rotation"

// mergeRotationSteps inserts the rotation events, which arrive newest first,
// into history by time, keeping history's own order. The daemon narrates a
// rotation incident from the same audit rows, so a rotation at the instant of a
// history step that narrates it (attention.NarratesRotation) is that step: the
// last such step keeps its title and state, and takes the rotation's text,
// which names the accounts, as its detail. A step narrates one rotation at
// most, and the waiting step narrates none. Any other rotation is its own step,
// ahead of history steps at its instant.
func mergeRotationSteps(history []*pb.TimelineStep, events []*pb.RotationEvent) []*pb.TimelineStep {
	merged := slices.Clone(history)
	narrated := make([]bool, len(merged))
	var rotations []*pb.TimelineStep
	for _, ev := range events {
		text := ui.RotationEventText(ev)
		trigger, outcome := ev.GetTrigger().String(), ev.GetOutcome().String()
		matched := -1
		for i, step := range merged {
			if !narrated[i] && step.GetAt() != nil && step.GetAt().AsTime().Equal(ev.GetCreatedAt().AsTime()) && attention.NarratesRotation(step.GetTitle(), trigger, outcome) {
				matched = i
			}
		}
		if matched >= 0 {
			narrated[matched] = true
			if step := merged[matched]; text != "" {
				merged[matched] = &pb.TimelineStep{At: step.GetAt(), Title: step.GetTitle(), Detail: text, State: step.GetState()}
			}
			continue
		}
		state := pb.TimelineStepState_TIMELINE_STEP_STATE_DONE
		if attention.IsRotationGiveUp(outcome) {
			state = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
		}
		rotations = append(rotations, &pb.TimelineStep{At: ev.GetCreatedAt(), Title: rotationStepTitle, Detail: text, State: state})
	}
	slices.SortStableFunc(rotations, func(a, b *pb.TimelineStep) int { return a.GetAt().AsTime().Compare(b.GetAt().AsTime()) })
	out := make([]*pb.TimelineStep, 0, len(merged)+len(rotations))
	for _, step := range merged {
		for len(rotations) > 0 && step.GetAt() != nil && !rotations[0].GetAt().AsTime().After(step.GetAt().AsTime()) {
			out, rotations = append(out, rotations[0]), rotations[1:]
		}
		out = append(out, step)
	}
	return append(out, rotations...)
}

// timelineBlock renders the timeline under the chat list in the muted style of
// the rotation-history block it absorbed: the chats and the answer are what
// the operator acts on, and the timeline is the record behind them.
func (m Model) timelineBlock() string {
	steps := m.timelineSteps()
	if len(steps) == 0 {
		return ""
	}
	pin := timelinePin(steps)
	candidates := timelineOrder(len(steps), pin)
	candidates = candidates[:min(len(candidates), timelineMaxSteps)]
	slices.Sort(candidates)
	// The pinned step's first line, and a fold line for each run of steps
	// on either side of it.
	least := 1 + len(timelineGaps([]int{pin}, len(steps)))
	budget := m.timelineRowBudget()
	if budget < least {
		return ""
	}
	titleWidth := 0
	for _, i := range candidates {
		titleWidth = max(titleWidth, ansi.StringWidth(steps[i].GetTitle()))
	}
	kept, rows := foldTimelineSteps(steps, candidates, pin, time.Now(), titleWidth, detailsWrapWidth(m.chatPickerProseAvailWidth()), budget)
	gaps := timelineGaps(kept, len(steps))
	lines := []string{"Timeline"}
	for i := 0; i <= len(kept); i++ {
		if len(gaps) > 0 && gaps[0].before == i {
			noun := "steps"
			if gaps[0].steps == 1 {
				noun = "step"
			}
			when := "later"
			if i == 0 {
				when = "earlier"
			}
			lines = append(lines, fmt.Sprintf("  … %d %s %s", gaps[0].steps, when, noun))
			gaps = gaps[1:]
		}
		if i < len(kept) {
			lines = append(lines, rows[i]...)
		}
	}
	for i, line := range lines {
		lines[i] = ui.StyleStatusMuted.Render(m.fitProseLine(line))
	}
	return strings.Join(lines, "\n")
}

// timelinePin is the step the timeline always shows: the current step, which
// sits in place with later steps after it, else the newest.
func timelinePin(steps []*pb.TimelineStep) int {
	if i := slices.IndexFunc(steps, func(s *pb.TimelineStep) bool {
		return s.GetState() == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
	}); i >= 0 {
		return i
	}
	return len(steps) - 1
}

// timelineOrder is the order the timeline keeps n steps in: the pinned one,
// then the rest newest first.
func timelineOrder(n, pin int) []int {
	order := []int{pin}
	for i := n - 1; i >= 0; i-- {
		if i != pin {
			order = append(order, i)
		}
	}
	return order
}

// timelineGap is a run of steps the timeline leaves out, drawn as one fold
// line ahead of the kept step at position before (len(kept) for a run after
// the last one).
type timelineGap struct{ before, steps int }

// timelineGaps is each run of the total steps that kept, ascending indices,
// leaves out.
func timelineGaps(kept []int, total int) []timelineGap {
	var gaps []timelineGap
	next := 0
	for i, k := range kept {
		if k > next {
			gaps = append(gaps, timelineGap{i, k - next})
		}
		next = k + 1
	}
	if next < total {
		gaps = append(gaps, timelineGap{len(kept), total - next})
	}
	return gaps
}

// foldTimelineSteps keeps the candidates, ascending indices into steps, whose
// rows fit budget, then dates the steps it kept: a folded step may have
// carried its day's label, and the fold may leave a single day. Dropping the
// date column widens the details, and a detail's first word that stops being
// wider than a continuation line then moves whole off the title line, which
// can make its row taller. When a relabelled row comes out taller the kept
// steps keep the date column, so the second fold never loses a step the first
// kept. It returns the kept steps' indices and their rows.
func foldTimelineSteps(steps []*pb.TimelineStep, candidates []int, pin int, now time.Time, titleWidth, avail, budget int) ([]int, [][]string) {
	pick := func(at []int) []*pb.TimelineStep {
		picked := make([]*pb.TimelineStep, len(at))
		for i, k := range at {
			picked[i] = steps[k]
		}
		return picked
	}
	fit := func(at []int, days []string) ([]int, [][]string) {
		return fitTimelineRows(timelineStepRows(pick(at), days, titleWidth, avail), at, len(steps), slices.Index(at, pin), budget)
	}
	days := timelineDayLabels(pick(candidates), now, false)
	folded, _ := fit(candidates, days)
	kept, rows := fit(folded, timelineDayLabels(pick(folded), now, false))
	if len(kept) < len(folded) {
		kept, rows = fit(folded, timelineDayLabels(pick(folded), now, days != nil))
	}
	return kept, rows
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

// timelineMaxStepRows caps one step's wrapped detail. A question's summary
// already has its own block above the chats, so the timeline only previews it.
const timelineMaxStepRows = 3

// detailsMaxWidth is the widest the timeline and the "Needed from you" answer
// wrap, however wide the terminal: a 250-column line is too long to read.
const detailsMaxWidth = 100

// detailsWrapWidth is width capped at detailsMaxWidth. Zero, an unsized model,
// stays zero, which leaves the lines whole.
func detailsWrapWidth(width int) int {
	return min(width, detailsMaxWidth)
}

// detailsReservedChats is how many chat rows the details blocks always leave
// the chat table, beyond its header.
const detailsReservedChats = 5

// detailsReservedTableRows is the chat table's header plus every chat row up to
// detailsReservedChats.
func (m Model) detailsReservedTableRows() int {
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
// The chat table and the answer above it are what the operator acts on, so the
// timeline gets only the rows the footer, the table's reserved rows and the
// whole answer leave, matching what neededBlock and tableHeight then
// subtract. A short terminal cuts the answer rather than squeeze the timeline
// below two rows, its newest step and the fold line, and the answer keeps its
// label and a row, but the table's reserved rows are never given up: the
// budget can fall below that, and timelineBlock then renders nothing.
func (m Model) timelineRowBudget() int {
	if m.height <= 0 {
		return (timelineMaxSteps + 1) * timelineMaxStepRows
	}
	room := m.height - m.chatListOverhead() - m.detailsReservedTableRows() - 1
	if lines := m.neededLines(); len(lines) > 0 {
		room -= neededChromeRows + 1
		room = min(room-1, max(2, room-len(lines)))
	}
	return room
}

// fitTimelineRows keeps the steps whose wrapped rows fit budget, keeping one
// row for the fold line of each run of steps left out. rows[i] is the rows of
// step at[i], at ascending indices into a timeline of total steps; the steps
// missing from at are already left out. The step at position pin always stays,
// then the most recent others while they fit. Each step is cut to
// timelineMaxStepRows, then the longest kept step loses a line at a time until
// the rows fit, and each cut step's last line ends with an ellipsis. It returns
// the kept steps' indices into the timeline and their rows.
func fitTimelineRows(rows [][]string, at []int, total, pin, budget int) ([]int, [][]string) {
	trimmed := make([]bool, len(rows))
	for i, row := range rows {
		if len(row) > timelineMaxStepRows {
			rows[i], trimmed[i] = row[:timelineMaxStepRows], true
		}
	}
	keep := make([]bool, len(rows))
	kept := func() []int {
		var indices []int
		for i, k := range keep {
			if k {
				indices = append(indices, at[i])
			}
		}
		return indices
	}
	used := 0
	for n, i := range timelineOrder(len(rows), pin) {
		keep[i] = true
		if n > 0 && used+len(rows[i])+len(timelineGaps(kept(), total)) > budget {
			keep[i] = false
			break
		}
		used += len(rows[i])
	}
	var keptRows [][]string
	var keptTrimmed []bool
	for i, k := range keep {
		if k {
			keptRows, keptTrimmed = append(keptRows, rows[i]), append(keptTrimmed, trimmed[i])
		}
	}
	rows, trimmed = keptRows, keptTrimmed
	indices := kept()
	budget -= len(timelineGaps(indices, total))
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
	return indices, rows
}

// wordWrap breaks text at whitespace into lines of at most first display cells
// on the first line and rest on the others. Only a word wider than rest is
// broken inside itself, filling the line it starts on; a narrower word that
// does not fit the first line starts the second whole, so a path never splits
// where a continuation line would hold it. A rest below two cells cannot hold a
// wide character, so the text wraps as if both widths were that much wider and
// fitProseLine truncates the overflow. An empty line that cannot hold a word
// takes as much of it as fits, and at least its first rune, unless the next
// line is wider, so every pass places part of the word or moves to a wider
// line and the wrap always ends.
func wordWrap(text string, first, rest int) []string {
	if rest < 2 {
		first, rest = first+2-rest, 2
	}
	width := func(line int) int {
		if line == 0 {
			return first
		}
		return rest
	}
	lines := []string{""}
	for _, word := range strings.Fields(text) {
		for word != "" {
			at := len(lines) - 1
			cur, limit := lines[at], width(at)
			used := ansi.StringWidth(cur)
			if used > 0 {
				used++
			}
			if used+ansi.StringWidth(word) <= limit {
				lines[at] = strings.TrimPrefix(cur+" "+word, " ")
				break
			}
			if next := width(at + 1); cur == "" && ansi.StringWidth(word) > next {
				if piece := ansi.Truncate(word, max(0, limit), ""); ansi.StringWidth(piece) > 0 {
					lines[at] = piece
					word = ansi.TruncateLeft(word, ansi.StringWidth(piece), "")
				} else if next <= limit {
					_, size := utf8.DecodeRuneInString(word)
					lines[at], word = word[:size], word[size:]
				}
			}
			lines = append(lines, "")
		}
	}
	return lines
}

// neededLabel is the answer's heading row.
const neededLabel = "Needed from you:"

// neededIndent opens each of the answer's rows under the label.
const neededIndent = "  "

// neededMaxLines is the most rows the answer takes under its label. The answer
// is a summary: the whole message is one [enter] away in the chat.
const neededMaxLines = 5

// neededBlock is the "Needed from you" answer above the chat table, or ""
// when the session asks nothing of the operator: the label on its own row,
// then the answer indented under it. A terminal too short for the whole answer
// gives it the rows left, at least one: a question is summarised into them.
func (m Model) neededBlock() string {
	room := math.MaxInt
	if m.height > 0 {
		room = max(1, m.height-m.chatListOverhead()-m.timelineHeight()-neededChromeRows-m.detailsReservedTableRows()-1)
	}
	lines := m.neededLinesIn(room)
	if len(lines) == 0 {
		return ""
	}
	rows := []string{neededLabel}
	for _, line := range lines {
		// The answer is the one thing on the screen the operator must act on,
		// so it renders bold in the primary foreground, above the faint chrome.
		rows = append(rows, m.fitProseLine(neededIndent+ui.StyleEmphasis.Render(line)))
	}
	return strings.Join(rows, "\n")
}

// neededLines is the whole answer wrapped at neededTextWidth. An agent's
// question is its message summarised as plain text in at most neededMaxLines
// rows; the other kinds are boss's own short lines, shown whole, each keeping
// its leading spaces, such as a checklist bullet's, on every row it wraps into.
func (m Model) neededLines() []string {
	return m.neededLinesIn(math.MaxInt)
}

// neededLinesIn is the answer in at most rows rows. A question is summarised
// to fit them, so a short terminal still shows its ask; boss's own lines are
// cut after the rows that fit.
func (m Model) neededLinesIn(rows int) []string {
	text := m.neededQuestion()
	if text == "" {
		return nil
	}
	width := m.neededTextWidth()
	if m.session.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_QUESTION {
		return summaryLines(text, width, min(rows, neededMaxLines))
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		lead := paragraph[:len(paragraph)-len(strings.TrimLeft(paragraph, " "))]
		for _, line := range wordWrap(paragraph, width-len(lead), width-len(lead)) {
			lines = append(lines, lead+line)
		}
	}
	return cutLines(lines, rows, width)
}

// neededTextWidth is the width the answer's rows wrap at after their indent:
// the chat table's width capped at detailsMaxWidth, which is also what an
// unsized model wraps at.
func (m Model) neededTextWidth() int {
	wrap := detailsWrapWidth(m.blockWrapWidth())
	if wrap <= 0 {
		wrap = detailsMaxWidth
	}
	return wrap - len(neededIndent)
}

// neededChromeRows is the blank row below the answer.
const neededChromeRows = 1

// attentionDetailsHeight is the rows the answer above the table and the
// timeline below the action bar take. The answer's blank row is its own; the
// timeline's blank row above is the action bar's bottom padding, which
// chatListOverhead already holds.
func (m Model) attentionDetailsHeight() int {
	rows := m.timelineHeight()
	if block := m.neededBlock(); block != "" {
		rows += lipgloss.Height(block) + 1
	}
	return rows
}

func (m Model) timelineHeight() int {
	if block := m.timelineBlock(); block != "" {
		return lipgloss.Height(block)
	}
	return 0
}
