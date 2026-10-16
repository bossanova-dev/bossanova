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
		if attention.IsUnverifiedReview(a) {
			verb = "ready"
		}
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
		if attention.IsUnverifiedReview(a) {
			return "Is this unverified PR safe to merge?"
		}
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
		if attention.IsUnverifiedReview(a) {
			title = "Waiting for your review"
		} else if a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW {
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
	if m.height > 0 && m.height <= 24 && len(steps) > 2 {
		steps = steps[len(steps)-2:]
	}
	titleWidth := 0
	for _, step := range steps {
		titleWidth = max(titleWidth, ansi.StringWidth(step.GetTitle()))
	}
	lines := []string{"Timeline"}
	for _, step := range steps {
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
		detail := step.GetDetail()
		if strings.Contains(detail, "://") {
			detail = ""
		}
		line := fmt.Sprintf("  %s  %s %s%s  %s", at, glyph, step.GetTitle(), strings.Repeat(" ", titleWidth-ansi.StringWidth(step.GetTitle())), detail)
		lines = append(lines, m.fitProseLine(strings.TrimRight(line, " ")))
	}
	return strings.Join(lines, "\n")
}

func (m ChatPickerModel) attentionDetailsBlock() string {
	var blocks []string
	if timeline := m.timelineBlock(); timeline != "" {
		blocks = append(blocks, timeline)
	}
	if q := m.neededQuestion(); q != "" {
		lines, start, size := m.neededWindow()
		heading := "Needed from you"
		if len(lines) > size {
			heading = fmt.Sprintf("Needed from you · pgup/pgdown (%d–%d of %d)", start+1, start+size, len(lines))
		}
		// The answer is the one thing on the screen the operator must act on,
		// so it renders bold in the primary foreground, above the faint chrome.
		answer := make([]string, 0, size)
		for _, line := range lines[start : start+size] {
			answer = append(answer, styleEmphasis.Render(line))
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

// neededWindow keeps the complete question available while protecting the chat
// table and footer. Only the question scrolls; chat navigation stays independent.
func (m ChatPickerModel) neededWindow() (lines []string, start, size int) {
	width := max(1, m.blockWrapWidth())
	lines = strings.Split(ansi.Hardwrap(ansi.Wrap(m.neededQuestion(), width, ""), width, false), "\n")
	size = len(lines)
	if m.height > 0 {
		timelineRows := 0
		if block := m.timelineBlock(); block != "" {
			timelineRows = lipgloss.Height(block) + 2
		}
		tableRows := min(len(m.chats)+1, 3)
		size = min(size, max(1, m.height-m.chatListOverhead()-timelineRows-2-tableRows))
	}
	return lines, min(max(0, m.neededOffset), len(lines)-size), size
}
