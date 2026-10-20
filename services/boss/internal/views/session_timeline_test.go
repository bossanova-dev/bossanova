package views

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/client"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//go:embed testdata/chatpicker-timeline-*.golden
var chatPickerTimelineGoldens embed.FS

type timelineClient struct {
	chatPickerStub
	calls    int
	steps    []*pb.TimelineStep
	err      error
	question string
	target   string
}

func (c *timelineClient) GetSessionTimeline(context.Context, string) ([]*pb.TimelineStep, error) {
	c.calls++
	return c.steps, c.err
}

func (c *timelineClient) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	return nil, errors.New("unneeded")
}
func (c *timelineClient) GetChatTranscript(_ context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	c.target = req.GetAgentSessionId()
	if c.question != "" {
		return &pb.GetChatTranscriptResponse{FinalAssistantText: c.question}, nil
	}
	return &pb.GetChatTranscriptResponse{FinalAssistantText: strings.Repeat("Should I preserve this behavior? ", 20)}, nil
}
func applyTimelineCmd(m ChatPickerModel, cmd tea.Cmd) ChatPickerModel {
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, sub := range msg {
			m = applyTimelineCmd(m, sub)
		}
	case sessionTimelineMsg:
		model, _ := m.Update(msg)
		m = model.(ChatPickerModel)
	}
	return m
}
func TestChatPickerTimelineFetch(t *testing.T) {
	c := &timelineClient{steps: []*pb.TimelineStep{{Title: "Build started"}}}
	m := NewChatPickerModel(c, context.Background(), "s", "")
	s := &pb.Session{Id: "s", PrDisplayHeadSha: "abc", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW}}
	model, cmd := m.Update(chatPickerSessionMsg{session: s})
	m = applyTimelineCmd(model.(ChatPickerModel), cmd)
	if c.calls != 1 || len(m.timeline) != 1 {
		t.Fatalf("open: calls=%d steps=%d", c.calls, len(m.timeline))
	}
	for _, change := range []string{"same", "head", "kind"} {
		if change == "head" {
			s.PrDisplayHeadSha = "def"
		}
		if change == "kind" {
			s.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_FIX
		}
		model, cmd = m.Update(chatPickerRefreshMsg{session: s})
		m = applyTimelineCmd(model.(ChatPickerModel), cmd)
	}
	if c.calls != 3 {
		t.Fatalf("calls=%d, want open + head + kind", c.calls)
	}
	c.err = errors.New("offline")
	s.PrDisplayHeadSha = "ghi"
	model, cmd = m.Update(chatPickerRefreshMsg{session: s})
	m = applyTimelineCmd(model.(ChatPickerModel), cmd)
	if m.timelineBlock() != "" {
		t.Fatal("failed RPC retained timeline")
	}
	s.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_QUESTION
	s.Attention.TargetChatId = "q"
	model, cmd = m.Update(chatPickerRefreshMsg{session: s})
	m = applyTimelineCmd(model.(ChatPickerModel), cmd)
	if m.neededQuestion() != strings.TrimSpace(strings.Repeat("Should I preserve this behavior? ", 20)) {
		t.Fatalf("full question lost: %q", m.neededQuestion())
	}
	// An older response cannot restore history from the old head.
	model, _ = m.Update(sessionTimelineMsg{key: timelineKey{head: "abc"}, steps: c.steps})
	if model.(ChatPickerModel).timelineBlock() != "" {
		t.Fatal("stale timeline restored")
	}
}

func TestChatPickerTimelineRefreshesQuestion(t *testing.T) {
	for _, change := range []string{"target", "timestamp"} {
		t.Run(change, func(t *testing.T) {
			c := &timelineClient{question: "First full question?"}
			m := NewChatPickerModel(c, context.Background(), "s", "")
			s := &pb.Session{Id: "s", PrDisplayHeadSha: "abc", Attention: &pb.SessionAttention{
				Kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, TargetChatId: "first",
				Since: timestamppb.New(time.Unix(100, 0)), Sentence: "First question?",
			}}
			model, cmd := m.Update(chatPickerSessionMsg{session: s})
			m = applyTimelineCmd(model.(ChatPickerModel), cmd)
			if m.neededQuestion() != c.question {
				t.Fatal("initial full question missing")
			}
			oldResponse := sessionTimelineMsg{key: m.timelineKey, question: c.question}
			if change == "target" {
				s.Attention.TargetChatId = "second"
			} else {
				s.Attention.Since = timestamppb.New(time.Unix(100, 1))
			}
			s.Attention.Sentence = "Second question?"
			c.question = "Second full question?"
			model, cmd = m.Update(chatPickerRefreshMsg{session: s})
			m = model.(ChatPickerModel)
			if m.fullQuestion != "" || m.neededQuestion() != s.Attention.Sentence {
				t.Fatalf("old question retained while fetching: %q", m.neededQuestion())
			}
			m = applyTimelineCmd(m, cmd)
			if c.calls != 2 || c.target != s.Attention.TargetChatId || m.neededQuestion() != c.question {
				t.Fatalf("replacement question not fetched: calls=%d target=%q question=%q", c.calls, c.target, m.neededQuestion())
			}
			model, _ = m.Update(oldResponse)
			m = model.(ChatPickerModel)
			if m.neededQuestion() != c.question {
				t.Fatal("late response restored previous question")
			}
			model, cmd = m.Update(chatPickerRefreshMsg{session: s})
			m = applyTimelineCmd(model.(ChatPickerModel), cmd)
			if c.calls != 2 || m.neededQuestion() != c.question {
				t.Fatal("unchanged question was not cached")
			}
		})
	}
}

func TestChatPickerNeededQuestionAndTimeline(t *testing.T) {
	for _, kind := range []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_MERGE} {
		m := NewChatPickerModel(nil, context.Background(), "s", "")
		m.width = 100
		m.height = 24
		m.session = &pb.Session{Id: "s", Attention: &pb.SessionAttention{Kind: kind, Since: timestamppb.New(time.Now().Add(-time.Hour)), Sentence: "Should I retry after 3 attempts?", ParkCodes: []string{"human-review:ui"}}}
		m.timeline = []*pb.TimelineStep{{Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}
		if block := m.timelineBlock(); !strings.Contains(block, "●") {
			t.Fatalf("%s has no current waiting step: %s", kind, block)
		}
		q := m.neededQuestion()
		if kind == pb.AttentionKind_ATTENTION_KIND_FIX {
			// A blocked park states what blocks it and gives no advice.
			if q != "Verify is blocked: UI change." {
				t.Fatalf("blocked park needed text = %q, want only what blocks it", q)
			}
			continue
		}
		if !strings.HasSuffix(q, "?") {
			t.Fatalf("needed text is not one question: %q", q)
		}
	}
}

func detailFixture(kind pb.AttentionKind, height int) ChatPickerModel {
	now := time.Now().Truncate(time.Minute).Add(-time.Hour)
	m := NewChatPickerModel(nil, context.Background(), "s", "")
	m.loading = false
	m.newTabSupported = false
	m.width = 100
	m.height = height
	m.session = &pb.Session{Id: "s", Title: "[BOS-1522] Change the color of needs-human alerts in the TUI", PrNumber: new(int32(2979)), PrDisplayHeadSha: "2bbaede9000", WorktreePath: "/tmp/worktree", LastRepairRunnerError: "STALE ERROR", Attention: &pb.SessionAttention{Kind: kind, Since: timestamppb.New(now), Sentence: "Failed after 3 attempts on this machine", ParkCodes: []string{"human-review:ui"}, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_VIDEO, pb.AttentionAction_ATTENTION_ACTION_MERGE}, EvidenceUrl: "https://example.com/proof", TargetChatId: "verify"}}
	m.chats = []*pb.AgentChat{{AgentSessionId: "verify", Title: "verify", CreatedAt: timestamppb.New(now), AgentName: "claude"}, {AgentSessionId: "build", Title: "Run bs-build", CreatedAt: timestamppb.New(now), AgentName: "claude"}}
	m.agents = []client.AgentInfo{{Name: "claude"}, {Name: "codex"}}
	m.daemonStatuses = map[string]string{"verify": statusWaiting, "build": statusWaiting}
	m.daemonWaitingReasons = map[string]string{"build": armedTestReason("checks_failed")}
	for i, title := range []string{"Build started", "Built and reviewed", "CI passed", "Evidence recorded", "Parked for you"} {
		// The steps share the attention's local day so the rows show bare times.
		m.timeline = append(m.timeline, &pb.TimelineStep{At: timestamppb.New(time.Date(now.Year(), now.Month(), now.Day(), 23, 4+i, 0, 0, time.Local)), Title: title, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
	}
	m.timeline[len(m.timeline)-1].State = pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
	if kind == pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
		m.session.Attention = nil
		m.session.LastRepairRunnerError = ""
	}
	if kind == pb.AttentionKind_ATTENTION_KIND_FIX {
		// A blocked park is fixed through a chat, never merged from its row.
		m.session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_VIDEO}
	}
	m.buildTableRows()
	return m
}

func TestChatPickerTimelineGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   pb.AttentionKind
		height int
	}{{"review", pb.AttentionKind_ATTENTION_KIND_REVIEW, 40}, {"review-24", pb.AttentionKind_ATTENTION_KIND_REVIEW, 24}, {"fix", pb.AttentionKind_ATTENTION_KIND_FIX, 24}, {"legacy", pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 24}, {"rotations", pb.AttentionKind_ATTENTION_KIND_FIX, 24}} {
		t.Run(tt.name, func(t *testing.T) {
			m := detailFixture(tt.kind, tt.height)
			if tt.name == "rotations" {
				m = withRotations(m)
			}
			screen := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: m.session, width: m.width}) + "\n" + m.View().Content)
			assertChatPickerGolden(t, "chatpicker-timeline-"+tt.name, screen)
			if lipgloss.Height(screen) > tt.height {
				t.Fatalf("render uses %d rows, budget %d", lipgloss.Height(screen), tt.height)
			}
			for _, line := range strings.Split(screen, "\n") {
				if lipgloss.Width(line) > m.width {
					t.Fatalf("overwide line %q", line)
				}
			}
			if tt.kind != pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
				for _, text := range []string{"Timeline", "Needed from you", "done · standby: wakes if CI turns red", "verify", "[enter] open chat"} {
					if !strings.Contains(screen, text) {
						t.Errorf("missing %s", text)
					}
				}
				if strings.Contains(screen, "STALE ERROR") {
					t.Fatal("legacy warning rendered")
				}
			}

		})
	}
}

// assertChatPickerGolden compares screen with testdata/<name>.golden, which
// UPDATE_GOLDEN=1 rewrites first.
func assertChatPickerGolden(t *testing.T, name, screen string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(screen), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := chatPickerTimelineGoldens.ReadFile(filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if screen != string(want) {
		t.Fatalf("render differs from %s:\n%s", path, screen)
	}
}

func TestChatPickerAttentionDetailsContracts(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 40)
	m.session.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_REVIEW
	m.session.Attention.ParkCodes = []string{"human-review:security"}
	if m.neededQuestion() != "Is this safe to merge?" {
		t.Fatal("security question missing")
	}
	m.session.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS
	m.session.Attention.ProgressLabel = "building"
	if m.neededQuestion() != "" {
		t.Fatal("in-progress session has a needed block")
	}
	m.timeline[0].Detail = "https://secret.example/video"
	m.timeline[0].State = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
	if block := m.timelineBlock(); strings.Contains(block, "https://") || !strings.Contains(block, "✗") {
		t.Fatalf("failed step or URL rendering: %s", block)
	}
}

// TestChatPickerNeededAnswerIsEmphasised: the answer after "Needed from you:"
// is the one thing the details screen asks the operator to act on, so every
// line of it, wrapped continuations included, renders bold in the primary
// foreground, never faint, while the label stays plain.
func TestChatPickerNeededAnswerIsEmphasised(t *testing.T) {
	m := questionTimelineModel(80, 60, operatorQuestion)
	m.fullQuestion = strings.Repeat("Should I retain this behavior across all supported platforms? ", 4)
	lines := strings.Split(m.neededBlock(), "\n")
	if len(lines) < 3 || lines[0] != neededLabel {
		t.Fatalf("answer did not wrap under a plain label:\n%q", lines)
	}
	for i, line := range lines[1:] {
		rest, ok := strings.CutPrefix(line, neededIndent)
		if !ok {
			t.Fatalf("line %d does not open with a plain %q: %q", i, neededIndent, line)
		}
		text := stripANSI(rest)
		if rest != styleEmphasis.Render(text) || !strings.HasPrefix(rest, "\x1b[1") {
			t.Fatalf("answer line %d %q rendered %q, want bold %q", i, text, rest, styleEmphasis.Render(text))
		}
	}
}

// TestChatPickerUIReviewQuestionNamesTheVideoOnlyWithEvidence: a UI park asks
// about the video only when the park has evidence to watch.
func TestChatPickerUIReviewQuestionNamesTheVideoOnlyWithEvidence(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 40)
	if got := m.neededQuestion(); got != "Does this UI change look right in the video?" {
		t.Fatalf("with evidence: %q", got)
	}
	m.session.Attention.EvidenceUrl = ""
	if got := m.neededQuestion(); got != "Does this UI change look right?" {
		t.Fatalf("without evidence: %q", got)
	}
}

// TestChatPickerDetailsBannerSaysReviewNotNeedsSignOff: the session-details
// title never shows the API's "needs sign-off" label, with or without
// attention. Its status suffix carries the TUI's "review" mapping.
func TestChatPickerDetailsBannerSaysReviewNotNeedsSignOff(t *testing.T) {
	sess := &pb.Session{Id: "s", Title: "Hide the line", PrNumber: new(int32(3154)), DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: displaystatus.NeedsSignOffLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	for _, attention := range []*pb.SessionAttention{nil, {Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui"}, Since: timestamppb.Now()}} {
		sess.Attention = attention
		banner := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: sess, width: 160}))
		if !strings.Contains(banner, "Hide the line (review)") || strings.Contains(banner, displaystatus.NeedsSignOffLabel) {
			t.Fatalf("banner with attention %v:\n%s", attention, banner)
		}
	}
}

// TestChatPickerBannerKeepsSessionStatusWithAttention: attention does not
// change the session-details header. Line one ends in the session's display
// status, or the archiving spinner while an archive is in flight, and line two
// is the worktree and account. The Needed from you block and the timeline
// explain the attention.
func TestChatPickerBannerKeepsSessionStatusWithAttention(t *testing.T) {
	account := "acct-work"
	sp := newStatusSpinner()
	frame := stripANSI(sp.View())
	for _, tt := range []struct {
		name      string
		attention *pb.SessionAttention
		archiving bool
		want      string
	}{
		{"working", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "checking"}, false, "#3184 Retain links (" + frame + "checking 3/5)"},
		{"archiving a fix", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Since: timestamppb.Now()}, true, "#3184 Retain links (" + frame + "archiving)"},
		{"archiving a review", &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Since: timestamppb.Now()}, true, "#3184 Retain links (" + frame + "archiving)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sess := &pb.Session{Id: "s", Title: "Retain links", PrNumber: new(int32(3184)), PrDisplayHeadSha: "395f2d3aaaa", WorktreePath: "/tmp/wt-3184", AccountId: &account, DisplayLabel: "checking 3/5", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, DisplaySpinner: true, Attention: tt.attention}
			lines := strings.Split(stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: sess, spinner: sp, archiving: tt.archiving, width: 160})), "\n")
			if len(lines) < 3 || !strings.HasSuffix(strings.TrimRight(lines[1], " "), tt.want) {
				t.Fatalf("header line 1 = %q, want suffix %q", lines[1], tt.want)
			}
			if got := strings.TrimRight(lines[2], " "); !strings.HasSuffix(got, "/tmp/wt-3184 · acct-work") {
				t.Fatalf("header line 2 = %q, want the worktree and account", got)
			}
		})
	}
}

// TestChatPickerBannerSpinsWhileAChatIsWorking: the header suffix is the
// session's display status. A session with a chat working reads "working" with
// the spinner whatever outcome its attention names; one without keeps its
// outcome label and no spinner.
func TestChatPickerBannerSpinsWhileAChatIsWorking(t *testing.T) {
	sp := newStatusSpinner()
	frame := stripANSI(sp.View())
	for _, tt := range []struct {
		name         string
		displayLabel string
		spinner      bool
		want         string
	}{
		{"a merge-ready session with a chat working", "working", true, "(" + frame + "working)"},
		{"a merge-ready session with no chat working", "✓ passing", false, "(✓ passing)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sess := &pb.Session{Id: "s", Title: "Retain links", PrNumber: new(int32(3184)), DisplayLabel: tt.displayLabel, DisplaySpinner: tt.spinner, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE}}
			lines := strings.Split(stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: sess, spinner: sp, width: 160})), "\n")
			if len(lines) < 2 || !strings.HasSuffix(strings.TrimRight(lines[1], " "), tt.want) {
				t.Fatalf("header = %q, want line 1 suffix %q", lines, tt.want)
			}
		})
	}
}

func TestChatPickerLongQuestionKeepsChatsVisible(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 24)
	m.fullQuestion = strings.Repeat("Should we keep this behavior for all supported platforms? ", 40) + "Can you confirm the final choice?"
	screen := m.View().Content
	if got := lipgloss.Height(screen) + bannerOverhead; got > 24 {
		t.Fatalf("long question uses %d rows, want <=24", got)
	}
	for _, text := range []string{"verify", "Run bs-build", "[enter] open chat", "[n]ew chat"} {
		if !strings.Contains(screen, text) {
			t.Fatalf("missing %q: %s", text, screen)
		}
	}
	if rows := neededRows(t, m); len(rows) > summaryMaxRows || !strings.HasSuffix(rows[len(rows)-1], "Can you confirm the final choice?") || strings.Contains(stripANSI(screen), "pgup") {
		t.Fatalf("long question is not a capped summary:\n%s", stripANSI(screen))
	}
}

func TestChatPickerTimelineRejectsPreviousSession(t *testing.T) {
	c := &timelineClient{steps: []*pb.TimelineStep{{Title: "Another session's build"}}}
	first := NewChatPickerModel(c, context.Background(), "first", "")
	first.session = &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW}}
	pending := first.syncTimeline()
	second := NewChatPickerModel(c, context.Background(), "second", "")
	second.session = first.session
	second.syncTimeline()
	model, _ := second.Update(pending())
	if model.(ChatPickerModel).timelineBlock() != "" {
		t.Fatal("accepted timeline from previous session with identical head/kind")
	}
}

// operatorQuestion is the sentence from the operator's report: long enough to
// wrap, with a hyphenated token a hyphen-breaking wrapper would split.
const operatorQuestion = "I judge no real logical prerequisites. MAD-1745's YAML rewrite is a rebase risk the plan already handles, not a blocker. Next I'll run the classifier."

func questionTimelineModel(width, height int, sentence string) ChatPickerModel {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, height)
	m.width = width
	m.session.Attention.Sentence = "“" + sentence + "”"
	since := m.session.GetAttention().GetSince().AsTime().Local()
	m.timeline = []*pb.TimelineStep{
		{At: timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 17, 10, 0, 0, time.Local)), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{Title: "Asked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
	m.buildTableRows()
	return m
}

// currentStepRows returns the timeline's waiting step: its first line and the
// continuation lines under it.
func currentStepRows(t *testing.T, block string) []string {
	t.Helper()
	lines := strings.Split(stripANSI(block), "\n")
	for i, line := range lines {
		if strings.Contains(line, "● ") {
			return lines[i:]
		}
	}
	t.Fatalf("no waiting step:\n%s", stripANSI(block))
	return nil
}

// titleColumn is the display column the waiting step's title starts at.
func titleColumn(t *testing.T, row string) int {
	t.Helper()
	at := strings.Index(row, "Waiting for your answer")
	if at < 0 {
		t.Fatalf("no waiting title in %q", row)
	}
	return ansi.StringWidth(row[:at])
}

// TestChatPickerTimelineWrapsLongDetail: a long step detail wraps at the
// terminal's prose width on word boundaries, every continuation line starting
// under the step title, so no word is lost or split.
func TestChatPickerTimelineWrapsLongDetail(t *testing.T) {
	for _, tt := range []struct {
		width, rows int
	}{{80, 3}, {120, 2}} {
		t.Run(fmt.Sprint(tt.width), func(t *testing.T) {
			m := questionTimelineModel(tt.width, 60, operatorQuestion)
			block := m.timelineBlock()
			t.Logf("timeline at %d columns:\n%s", tt.width, stripANSI(block))
			rows := currentStepRows(t, block)
			if len(rows) != tt.rows {
				t.Fatalf("waiting step wraps into %d rows, want %d:\n%s", len(rows), tt.rows, strings.Join(rows, "\n"))
			}
			indent := titleColumn(t, rows[0])
			for _, line := range rows {
				if w := ansi.StringWidth(line); w > m.chatPickerProseAvailWidth() {
					t.Errorf("line is %d wide, budget %d: %q", w, m.chatPickerProseAvailWidth(), line)
				}
			}
			for _, line := range rows[1:] {
				if got := len(line) - len(strings.TrimLeft(line, " ")); got != indent || line[got:] == "" {
					t.Errorf("continuation indented %d, want %d (under the title): %q", got, indent, line)
				}
			}
			detail := strings.Join(strings.Fields(strings.Join(rows, " ")), " ")
			if detail == "" || !strings.HasSuffix(detail, "“"+operatorQuestion+"”") {
				t.Fatalf("detail words lost or split: %q", detail)
			}
			if strings.Contains(stripANSI(block), "…") {
				t.Fatal("detail truncated on a tall terminal")
			}
		})
	}
}

// operatorFixSentence is the ~250-column "Waiting for a fix" detail from the
// operator's report, which a 300-column terminal drew on one line.
const operatorFixSentence = "Run blocked: MAD-1751 implemented; the PR is open but its CI is red on an unrelated flaky e2e suite that the agent cannot fix from this worktree, so a person must rerun the job or override the gate before verify can pick it up again and merge it"

// TestChatPickerDetailsWrapAtAReadableWidth: on a terminal far wider than
// detailsMaxWidth the timeline and the "Needed from you" answer still wrap at
// detailsMaxWidth, keep every word, and fit the screen.
func TestChatPickerDetailsWrapAtAReadableWidth(t *testing.T) {
	const width, height = 300, 40
	fix := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, height)
	fix.width = width
	fix.session.Attention.ParkCodes = nil
	fix.session.Attention.Sentence = operatorFixSentence
	fix.buildTableRows()
	question := questionTimelineModel(width, height, operatorQuestion)
	question.fullQuestion = strings.Repeat("Should we keep the behavior for every supported platform? ", 6) + "FINAL?"
	for name, tt := range map[string]struct {
		m     ChatPickerModel
		block string
		words string
	}{
		"timeline": {fix, fix.timelineBlock(), operatorFixSentence},
		"needed":   {question, question.neededBlock(), question.fullQuestion},
	} {
		t.Run(name, func(t *testing.T) {
			block := stripANSI(tt.block)
			t.Logf("%s at %d columns:\n%s", name, width, block)
			for _, line := range strings.Split(block, "\n") {
				if w := ansi.StringWidth(line); w > detailsMaxWidth {
					t.Errorf("line is %d wide, cap %d: %q", w, detailsMaxWidth, line)
				}
			}
			if joined := strings.Join(strings.Fields(block), " "); !strings.Contains(joined, tt.words) {
				t.Fatalf("words lost or split:\n%s", block)
			}
			screen := tt.m.View().Content
			if got := lipgloss.Height(screen) + bannerOverhead; got > height {
				t.Fatalf("view uses %d rows, budget %d", got, height)
			}
			for _, line := range strings.Split(stripANSI(screen), "\n") {
				if ansi.StringWidth(line) > width {
					t.Errorf("overwide line %q", line)
				}
			}
		})
	}
	if rows := currentStepRows(t, stripANSI(fix.timelineBlock())); len(rows) != 3 || !regexp.MustCompile(`● Waiting for a fix +Run blocked: MAD-1751 `).MatchString(rows[0]) {
		t.Fatalf("waiting step = %q, want the title then the detail, wrapped onto 3 rows", rows)
	}
}

// TestChatPickerTimelineWrapsWideCharacters: CJK and emoji wrap by display
// width, so no line overflows and no wide character is split, and an
// unbroken CJK run longer than a line is hard-broken.
func TestChatPickerTimelineWrapsWideCharacters(t *testing.T) {
	sentence := "确认这个改动是否可以安全地合并到主分支并且不影响现有的部署流程以及回滚步骤 🚀 请检查数据库迁移 ✅ 然后回复"
	m := questionTimelineModel(60, 60, sentence)
	block := m.timelineBlock()
	t.Logf("timeline at 60 columns:\n%s", stripANSI(block))
	rows := currentStepRows(t, block)
	if len(rows) < 2 {
		t.Fatalf("wide detail did not wrap:\n%s", strings.Join(rows, "\n"))
	}
	indent := titleColumn(t, rows[0])
	for _, line := range rows {
		if w := ansi.StringWidth(line); w > m.chatPickerProseAvailWidth() {
			t.Errorf("line is %d wide, budget %d: %q", w, m.chatPickerProseAvailWidth(), line)
		}
		if !utf8.ValidString(line) {
			t.Errorf("wide character split: %q", line)
		}
	}
	for _, line := range rows[1:] {
		if got := len(line) - len(strings.TrimLeft(line, " ")); got != indent || line[got:] == "" {
			t.Errorf("continuation indented %d, want %d: %q", got, indent, line)
		}
	}
	joined := strings.Join(strings.Fields(strings.Join(rows, " ")), "")
	if !strings.HasSuffix(joined, strings.Join(strings.Fields("“"+sentence+"”"), "")) {
		t.Fatalf("wide detail lost characters: %q", joined)
	}
}

// operatorMarkdownQuestion has the shape of the operator's report: a long
// agent message with bold, inline code, and a numbered list with nested
// bullets, whose question sits in its opening sentences.
const operatorMarkdownQuestion = "**Should I keep the YAML rewrite in this PR, or split it out?** The `services/boss/internal/views` change is ready, but the rewrite touches `config.yaml` in three places and doubles the diff.\n\nHere is what I found:\n\n1. **Keep it here**\n   - one review, one merge\n   - the diff grows to 900 lines\n2. **Split it out**\n   - `MAD-1745` lands first\n   - this PR rebases on it\n\n## Next steps\n\nOnce you answer I'll run the classifier, update `__init__.py`, and push. Which do you prefer?"

// summaryMaxRows is the most rows the answer may take under its label. It is
// the operator's contract, a few lines, kept apart from neededMaxLines so a
// change to the code's cap fails the tests rather than moving them with it.
const summaryMaxRows = 5

// neededRows is the answer's rows under the "Needed from you:" label, which
// must sit alone on the block's first row, with the indent cut off each row.
func neededRows(t *testing.T, m ChatPickerModel) []string {
	t.Helper()
	block := stripANSI(m.neededBlock())
	rows := strings.Split(block, "\n")
	if rows[0] != neededLabel {
		t.Fatalf("block opens with %q, want the label alone:\n%s", rows[0], block)
	}
	var texts []string
	for _, row := range rows[1:] {
		text, ok := strings.CutPrefix(row, neededIndent)
		if !ok || strings.TrimSpace(text) == "" {
			t.Fatalf("row %q is not indented text under the label:\n%s", row, block)
		}
		texts = append(texts, text)
	}
	return texts
}

// TestChatPickerNeededFromYouSummarisesAMarkdownQuestion: a long markdown
// question shows its opening as plain words under the label, wrapped at the
// chat table's width, ending in an ellipsis, then the question it ends on, in
// at most summaryMaxRows rows.
func TestChatPickerNeededFromYouSummarisesAMarkdownQuestion(t *testing.T) {
	plain := plainMarkdown(operatorMarkdownQuestion)
	for _, width := range []int{40, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := questionTimelineModel(width, 40, operatorQuestion)
			m.fullQuestion = operatorMarkdownQuestion
			rows := neededRows(t, m)
			t.Logf("answer at %d columns:\n%s", width, stripANSI(m.neededBlock()))
			if len(rows) == 0 || len(rows) > summaryMaxRows {
				t.Fatalf("answer shows %d rows, want 1 to %d", len(rows), summaryMaxRows)
			}
			if wrap := min(detailsMaxWidth, m.blockWrapWidth()); m.neededTextWidth() != wrap-len(neededIndent) {
				t.Fatalf("answer wraps at %d, want the chat table's %d less the indent", m.neededTextWidth(), wrap)
			}
			for _, row := range rows {
				if strings.Contains(row, "**") || strings.Contains(row, "`") || strings.Contains(row, "__") {
					t.Errorf("raw markdown in %q", row)
				}
				if w := ansi.StringWidth(row); w > m.neededTextWidth() {
					t.Errorf("row is %d wide, want at most %d: %q", w, m.neededTextWidth(), row)
				}
			}
			if len(rows) < 2 || rows[len(rows)-1] != "Which do you prefer?" || !strings.HasSuffix(rows[len(rows)-2], "…") {
				t.Fatalf("cut answer is not its opening, an ellipsis, then its last question: %q", rows)
			}
			shown := strings.TrimSpace(strings.TrimSuffix(strings.Join(rows[:len(rows)-1], " "), "…"))
			if !strings.HasPrefix(plain, shown) || !strings.HasPrefix(shown, "Should I keep the YAML rewrite in this PR, or split it out?") {
				t.Fatalf("answer is not the message's opening: %q", shown)
			}
		})
	}
}

// TestChatPickerNeededFromYouShowsAShortQuestionWhole: a question that fits
// summaryMaxRows shows every word, with no ellipsis.
func TestChatPickerNeededFromYouShowsAShortQuestionWhole(t *testing.T) {
	for _, question := range []string{"Should this PR be merged?", "Is the **`config.yaml`** change safe to ship?", operatorQuestion} {
		m := questionTimelineModel(80, 40, operatorQuestion)
		m.fullQuestion = question
		rows := neededRows(t, m)
		if got := strings.Join(rows, " "); got != plainMarkdown(question) || strings.Contains(got, "…") {
			t.Fatalf("answer = %q, want %q whole", got, plainMarkdown(question))
		}
	}
}

// TestChatPickerBlockedParkNeededUnderItsLabel: a blocked park's open items
// sit under the label, one bullet per row, like every other kind's answer.
// The checklist is boss's own, so it is never cut to the question's summary
// length while the terminal has room for it.
func TestChatPickerBlockedParkNeededUnderItsLabel(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40)
	m.session.Attention.OpenItems = []string{"The Acceptance criteria section is missing.", "Open question: which region first?"}
	want := []string{"Open in the PR checklist:", "  • The Acceptance criteria section is missing.", "  • Open question: which region first?"}
	if rows := neededRows(t, m); !slices.Equal(rows, want) {
		t.Fatalf("blocked park answer = %q, want %q", rows, want)
	}
	m = detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 60)
	want = []string{"Open in the PR checklist:"}
	for i := range summaryMaxRows + 2 {
		item := fmt.Sprintf("Item %d is open.", i+1)
		m.session.Attention.OpenItems = append(m.session.Attention.OpenItems, item)
		want = append(want, "  • "+item)
	}
	if rows := neededRows(t, m); !slices.Equal(rows, want) {
		t.Fatalf("long checklist = %q, want every item %q", rows, want)
	}
}

// TestSummaryLinesCutsAtAParagraphSentenceOrWord: a summary too long for its
// lines ends in an ellipsis after the last whole paragraph that fits, never
// after a list or a paragraph that only introduces one; failing that, at the
// last sentence end when that keeps at least half the words that fit, else
// after the last word that leaves room for the ellipsis.
func TestSummaryLinesCutsAtAParagraphSentenceOrWord(t *testing.T) {
	for _, tt := range []struct {
		name, text  string
		width, most int
		want        []string
	}{
		{"sentence", "Is this ready to merge? I checked the docs and the tests.", 30, 1, []string{"Is this ready to merge? …"}},
		{"word", "One two. Three four five six seven eight nine ten.", 20, 2, []string{"One two. Three four", "five six seven…"}},
		{"fits", "Is this ready to merge?", 30, 1, []string{"Is this ready to merge?"}},
		{"list after the question", "Is this ready to merge?\n\n1. **Yes**\n   - ship it", 30, 1, []string{"Is this ready to merge? …"}},
		{"second paragraph", "I'm stuck on the build.\n\nShould I pin the version?\n\n- details one two three", 30, 2, []string{"I'm stuck on the build. Should", "I pin the version? …"}},
		{"never ends on a list", "Should I ship it?\n\n- one review\n- one merge\n\nThe rest of the reasons follow here at length.", 30, 2, []string{"Should I ship it? …"}},
		{"never ends introducing", "Two ways forward:\n\n- keep the cache\n- drop it", 20, 1, []string{"Two ways forward…"}},
		{"heading first", "## Question\n\nIs this ready to merge?", 30, 3, []string{"Is this ready to merge?"}},
		{"list number", "Pick one: 1. keep the cache 2. drop it entirely and move on", 20, 2, []string{"Pick one: 1. keep", "the cache 2. drop…"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := summaryLines(tt.text, tt.width, tt.most); !slices.Equal(got, tt.want) {
				t.Fatalf("summaryLines(%q, %d, %d) = %q, want %q", tt.text, tt.width, tt.most, got, tt.want)
			}
		})
	}
}

// TestPlainMarkdown: the markers a terminal would show raw are gone, bullets
// read as "•", and the words stay in order.
func TestPlainMarkdown(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"**Keep** the `cache`?", "Keep the cache?"},
		{"## Next steps\n\n1. **Keep it**\n   - one review\n   * two merges\n2. Split", "1. Keep it • one review • two merges 2. Split"},
		{"> quoted\n\n---\n\n```go\nx := 1\n```\nupdate `__init__.py` and __bold__", "quoted update __init__.py and bold"},
		{"## Done", "Done"},
		{"a **b** `c", "a b c"},
		{"plain words\nacross lines", "plain words across lines"},
	} {
		if got := plainMarkdown(tt.in); got != tt.want {
			t.Errorf("plainMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestChatPickerTimelineWaitIsPlainText: the timeline's "Waiting for your
// answer" step previews the question without raw markdown, like the answer.
func TestChatPickerTimelineWaitIsPlainText(t *testing.T) {
	m := questionTimelineModel(80, 40, "**Keep** the `cache` or drop it?")
	block := stripANSI(m.timelineBlock())
	if !strings.Contains(block, "“Keep the cache or drop it?”") || strings.Contains(block, "**") || strings.Contains(block, "`") {
		t.Fatalf("waiting step is not plain text:\n%s", block)
	}
}

// TestChatPickerNeededFromYouGoldens pins the details screen with a short
// question, the long markdown one, the question-last survey, the CJK
// question, a closing list of questions, "?" in code words, a quoted
// question, a heading question, a Japanese one, a closing list then a
// sign-off, a report opening with a heading question, a closing table, code
// with spaces inside its backticks and a heading question closed with hashes
// at 40, 80 and 120 columns.
func TestChatPickerNeededFromYouGoldens(t *testing.T) {
	answers := map[string]string{
		"short":        "Should this PR be merged?",
		"markdown":     operatorMarkdownQuestion,
		"survey":       surveyQuestion,
		"cjk":          cjkQuestion,
		"closing-list": closingListQuestion,
		"code-word":    codeWordQuestion,
		"quoted":       quotedQuestion,
		"heading":      headingQuestion,
		"japanese":     japaneseQuestion,
		"sign-off":     signOffQuestion,
		"report":       reportHeading,
		"table":        closingTable,
		"code-padding": codePadding,
		"heading-hash": headingCloser,
	}
	for _, width := range []int{40, 80, 120} {
		for name, answer := range answers {
			t.Run(fmt.Sprintf("%s-%d", name, width), func(t *testing.T) {
				m := questionTimelineModel(width, 40, answer)
				m.fullQuestion = answer
				asked := m.session.GetAttention().GetSince().AsTime().Local()
				m.session.Attention.Since = timestamppb.New(time.Date(asked.Year(), asked.Month(), asked.Day(), 17, 20, 0, 0, time.Local))
				screen := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: m.session, width: m.width}) + "\n" + m.View().Content)
				assertChatPickerGolden(t, fmt.Sprintf("chatpicker-timeline-needed-%s-%d", name, width), screen)
				for _, line := range strings.Split(screen, "\n") {
					if lipgloss.Width(line) > m.width {
						t.Fatalf("overwide line %q", line)
					}
				}
			})
		}
	}
}

// finishWithin runs fn and panics if it has not returned after d. A wrap
// loop that spins grows memory without bound, so a regression must kill the
// test binary rather than leave the spinning goroutine behind.
func finishWithin(d time.Duration, what string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		panic(what + " did not finish within " + d.String())
	}
}

// viewsSweepsEnv opts in to the exhaustive and timing checks of this package.
// The per-PR suite keeps one named case per failure mode they have found; the
// nightly native job runs them.
const viewsSweepsEnv = "BOSS_VIEWS_SWEEPS"

// requireViewsSweeps skips t unless viewsSweepsEnv is "1".
func requireViewsSweeps(t *testing.T) {
	t.Helper()
	if os.Getenv(viewsSweepsEnv) != "1" {
		t.Skipf("exhaustive views sweep; set %s=1 to run it", viewsSweepsEnv)
	}
}

// neededSizeAnswers are a markdown question, one with a word wider than a
// narrow terminal, and a CJK one with emoji.
var neededSizeAnswers = []string{
	operatorMarkdownQuestion,
	"Should we keep the implementation? It touches services/boss/internal/views and supercalifragilisticexpialidociousness-everywhere-on-every-platform, so say yes or no.",
	"确认这个改动是否可以安全地合并到主分支并且不影响现有的部署流程以及回滚步骤 🚀 请检查数据库迁移 ✅ 然后回复",
}

// assertNeededFits renders answer at width x height and checks that it opens
// with the label on its own row and keeps to summaryMaxRows rows. From 29
// columns, as before the answer was capped, no row is wider than the terminal,
// and from 18 rows the whole view fits it; narrower, the footer's own key
// hints overflow. It returns the answer's rows.
func assertNeededFits(t *testing.T, answer string, width, height int) []string {
	t.Helper()
	m := questionTimelineModel(width, height, operatorQuestion)
	m.fullQuestion = answer
	rows := neededRows(t, m)
	if len(rows) > summaryMaxRows {
		t.Fatalf("%dx%d: %d rows, want at most %d", width, height, len(rows), summaryMaxRows)
	}
	screen := m.View().Content
	for _, line := range strings.Split(stripANSI(screen), "\n") {
		if ansi.StringWidth(line) > width && width >= 29 {
			t.Fatalf("%dx%d: overwide line %q", width, height, line)
		}
	}
	if got := lipgloss.Height(screen) + bannerOverhead; width >= 29 && height >= 18 && got > height {
		t.Fatalf("%dx%d: view uses %d rows:\n%s", width, height, got, stripANSI(screen))
	}
	return rows
}

// TestChatPickerNeededFromYouFitsAtNamedSizes: each answer fits at the sizes
// where the exhaustive sweep and its paging predecessor found faults: one- and
// two-row answer windows below 41 columns, where a word wider than the label
// width froze the wrap, 40x20 and 30x24, the 29 to 31 column edge where rows
// must stop overflowing, the old pager's 41 and 45 column edges, and the
// sweep's corners. windowRows, when set, is the answer rows the size must
// give, so a layout change that moves the window fails here rather than
// leaving the case testing a different size.
func TestChatPickerNeededFromYouFitsAtNamedSizes(t *testing.T) {
	for _, tt := range []struct {
		width, height, windowRows int
	}{
		{20, 10, 1},
		{30, 18, 1},
		{30, 19, 2},
		{40, 20, 1},
		{40, 21, 2},
		{30, 24, 0},
		{29, 24, 0},
		{31, 24, 0},
		{41, 24, 0},
		{45, 24, 0},
		{60, 40, 0},
	} {
		t.Run(fmt.Sprintf("%dx%d", tt.width, tt.height), func(t *testing.T) {
			for i, answer := range neededSizeAnswers {
				finishWithin(time.Minute, fmt.Sprintf("answer %d at %dx%d", i, tt.width, tt.height), func() {
					if rows := assertNeededFits(t, answer, tt.width, tt.height); tt.windowRows > 0 && len(rows) != tt.windowRows {
						t.Fatalf("answer %d at %dx%d shows %d rows, want the %d-row window: %q", i, tt.width, tt.height, len(rows), tt.windowRows, rows)
					}
				})
			}
		})
	}
}

// TestViewsSweep_NeededFromYouFitsAtEverySize runs assertNeededFits at every
// width from 20 to 60 and height from 10 to 40. It takes about a minute per
// answer under -race, so it runs nightly, not per PR.
func TestViewsSweep_NeededFromYouFitsAtEverySize(t *testing.T) {
	requireViewsSweeps(t)
	for i, answer := range neededSizeAnswers {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			finishWithin(10*time.Minute, "the size sweep", func() {
				for width := 20; width <= 60; width++ {
					for height := 10; height <= 40; height++ {
						assertNeededFits(t, answer, width, height)
					}
				}
			})
		})
	}
}

// TestChatPickerTimelineFitsShortTerminal: the answer below the timeline is
// what the operator acts on, so a wrapped detail gets only the rows the chat
// table and the whole answer leave. When they leave none it is cut to one line
// ending in an ellipsis, the chats and footer stay on screen, and an answer
// that still does not fit is cut to the rows left, ending in an ellipsis. A
// tall terminal still caps a step at timelineMaxStepRows.
func TestChatPickerTimelineFitsShortTerminal(t *testing.T) {
	long := strings.Repeat("Should we keep the behavior for every supported platform? ", 12) + "FINAL?"
	for _, tt := range []struct {
		name         string
		height, rows int
		question     string
		cut          bool
	}{
		{"long answer at 24 rows", 24, 3, long, false},
		{"short answer at 18 rows", 18, 1, operatorQuestion, true},
		{"short answer at 24 rows", 24, 3, operatorQuestion, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := questionTimelineModel(80, tt.height, operatorQuestion)
			m.fullQuestion = tt.question
			screen := m.View().Content
			t.Logf("screen at 80x%d:\n%s", tt.height, stripANSI(screen))
			if got := lipgloss.Height(screen) + bannerOverhead; got > tt.height {
				t.Fatalf("view uses %d rows, budget %d", got, tt.height)
			}
			for _, text := range []string{"verify", "Run bs-build", "[enter] open chat", "[n]ew chat", "Waiting for your answer"} {
				if !strings.Contains(screen, text) {
					t.Errorf("missing %q", text)
				}
			}
			for _, line := range strings.Split(stripANSI(screen), "\n") {
				if ansi.StringWidth(line) > m.width {
					t.Errorf("overwide line %q", line)
				}
			}
			rows := currentStepRows(t, m.timelineBlock())
			if len(rows) != tt.rows {
				t.Fatalf("waiting step uses %d rows, want %d:\n%s", len(rows), tt.rows, strings.Join(rows, "\n"))
			}
			if cut := strings.HasSuffix(rows[len(rows)-1], "…"); cut != (tt.rows == 1) {
				t.Fatalf("ellipsis = %v, want %v:\n%s", cut, tt.rows == 1, strings.Join(rows, "\n"))
			}
			shown, whole := neededRows(t, m), m.neededLines()
			if cut := len(shown) < len(whole); cut != tt.cut || (cut && !strings.HasSuffix(shown[len(shown)-1], "…")) {
				t.Fatalf("answer cut = %v, want %v: %d of %d rows shown:\n%s", cut, tt.cut, len(shown), len(whole), strings.Join(shown, "\n"))
			}
		})
	}
	m := questionTimelineModel(80, 60, long)
	if rows := currentStepRows(t, m.timelineBlock()); len(rows) != timelineMaxStepRows || !strings.HasSuffix(rows[len(rows)-1], "…") {
		t.Fatalf("tall terminal: %d rows, want %d ending in an ellipsis:\n%s", len(rows), timelineMaxStepRows, strings.Join(rows, "\n"))
	}
}

// TestChatPickerTimelineCapsLongHistory: a long-running session's dozens of
// steps never push the chats, the answer or the footer off screen. The
// timeline keeps the most recent steps that fit, at most timelineMaxSteps,
// and folds the rest into one "… N earlier steps" line.
func TestChatPickerTimelineCapsLongHistory(t *testing.T) {
	for _, height := range []int{24, 40} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, height)
			since := m.session.GetAttention().GetSince().AsTime().Local()
			m.timeline = nil
			for i := range 50 {
				m.timeline = append(m.timeline, &pb.TimelineStep{At: timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 0, i, 0, 0, time.Local)), Title: fmt.Sprintf("Run %d", i+1), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
			}
			m.timeline = append(m.timeline, &pb.TimelineStep{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT})
			for _, id := range []string{"third", "fourth"} {
				m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: id, Title: id + " chat", CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
				m.daemonStatuses[id] = statusWaiting
			}
			m.buildTableRows()
			m.table.SetHeight(m.tableHeight())
			screen := stripANSI(m.View().Content)
			t.Logf("screen at 100x%d:\n%s", height, screen)
			if got := lipgloss.Height(screen) + bannerOverhead; got > height {
				t.Fatalf("view uses %d rows, budget %d", got, height)
			}
			for _, text := range []string{"verify", "Run bs-build", "third chat", "fourth chat", "Needed from you:", "Does this UI change look right in the video?", "[enter] open chat", "[esc] back", "Parked for you"} {
				if !strings.Contains(screen, text) {
					t.Errorf("missing %q", text)
				}
			}
			shown, earlier := 0, -1
			for _, line := range strings.Split(stripANSI(m.timelineBlock()), "\n") {
				if strings.Contains(line, "✓ ") || strings.Contains(line, "● ") {
					shown++
				}
				if _, err := fmt.Sscanf(strings.TrimSpace(line), "… %d earlier steps", &earlier); err == nil && !strings.HasPrefix(line, "  … ") {
					t.Errorf("earlier line not indented with the steps: %q", line)
				}
			}
			if shown < 1 || shown > timelineMaxSteps {
				t.Fatalf("timeline shows %d steps, want 1..%d", shown, timelineMaxSteps)
			}
			if earlier+shown != 51 {
				t.Fatalf("earlier line says %d, with %d shown of 51", earlier, shown)
			}
		})
	}
}

// PR 3162: the session entered its fixing state at 13:18, verify parked the
// rebased head at 14:12, and the operator's message started a run at 17:15.
// The waiting step takes the park's time and place, not the state's start
// drawn last below a later row.
func TestChatPickerTimelineWaitKeepsTheDaemonsTimeAndPlace(t *testing.T) {
	now := time.Now()
	clock := func(hour, minute int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.Local)
	}
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 60)
	m.session.Attention.Since = timestamppb.New(clock(13, 18))
	m.timeline = []*pb.TimelineStep{
		{At: timestamppb.New(clock(11, 0)), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: timestamppb.New(clock(13, 40)), Title: "Run ended unexpectedly", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: timestamppb.New(clock(14, 12)), Title: "Parked for you · UI change", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
		{At: timestamppb.New(clock(17, 15)), Title: "Resumed", Detail: "new run", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
	}
	block := stripANSI(m.timelineBlock())
	t.Logf("timeline:\n%s", block)
	rows := strings.Split(block, "\n")[1:]
	want := []string{
		"  11:00  ✓ Build started",
		"  13:40  ✗ Run ended unexpectedly",
		"  14:12  ● Waiting for a fix",
		"  17:15  ✓ Resumed",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), block)
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, want[i]) {
			t.Errorf("row %d = %q, want prefix %q", i, row, want[i])
		}
	}
}

// TestChatPickerTimelineDatesStepsAcrossDays: when visible steps span local
// days, each row that starts a new day carries its date (or "Today") before
// its time, the time and glyph columns stay aligned, and a timeline within one
// day keeps bare times.
func TestChatPickerTimelineDatesStepsAcrossDays(t *testing.T) {
	now := time.Now()
	day := func(back, hour, minute int) time.Time {
		d := now.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, time.Local)
	}
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 60)
	m.session.Attention.Since = timestamppb.New(day(0, 0, 5))
	m.timeline = []*pb.TimelineStep{
		{At: timestamppb.New(day(3, 21, 34)), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: timestamppb.New(day(2, 0, 30)), Title: "Built", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: timestamppb.New(day(2, 10, 52)), Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
	block := stripANSI(m.timelineBlock())
	t.Logf("multi-day timeline:\n%s", block)
	rows := strings.Split(block, "\n")[1:]
	want := []string{
		"  " + day(3, 21, 34).Format("Jan _2") + " 21:34  ✓ Build started",
		"  " + day(2, 0, 30).Format("Jan _2") + " 00:30  ✓ Built",
		"         10:52  ✓ CI passed",
		"  Today  00:05  ● Parked for you",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), block)
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, want[i]) {
			t.Errorf("row %d = %q, want prefix %q", i, row, want[i])
		}
	}
	m.timeline = m.timeline[2:]
	m.timeline[0].At = timestamppb.New(day(0, 0, 1))
	if block := stripANSI(m.timelineBlock()); !strings.Contains(block, "\n  00:01  ✓ CI passed") || strings.Contains(block, "Today") {
		t.Fatalf("one-day timeline is dated:\n%s", block)
	}
}

// TestChatPickerTimelineDatesStepsLeftAfterFold: the day labels describe the
// steps the fold leaves. When the step that carried a day's date is folded,
// the first row left for that day carries it instead, and when the fold leaves
// only today's steps no date column shows.
func TestChatPickerTimelineDatesStepsLeftAfterFold(t *testing.T) {
	now := time.Now()
	day := func(back, hour, minute int) time.Time {
		d := now.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, time.Local)
	}
	for _, tt := range []struct {
		name   string
		height int
		want   []string
	}{
		{"fold drops the labelled step", 21, []string{
			"  … 2 earlier steps",
			"  " + day(1, 0, 0).Format("Jan _2") + " 23:50  ✓ B",
			"         23:55  ✓ B2",
			"  Today  00:01  ✓ C",
			"         00:05  ● Parked for you",
		}},
		{"fold leaves only today", 19, []string{
			"  … 4 earlier steps",
			"  00:01  ✓ C",
			"  00:05  ● Parked for you",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, tt.height)
			m.width = 80
			m.session.Attention.Since = timestamppb.New(day(0, 0, 5))
			m.timeline = []*pb.TimelineStep{
				{At: timestamppb.New(day(1, 23, 30)), Title: "A0", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(1, 23, 40)), Title: "A", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(1, 23, 50)), Title: "B", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(1, 23, 55)), Title: "B2", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(0, 0, 1)), Title: "C", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
			}
			block := stripANSI(m.timelineBlock())
			t.Logf("timeline at 80x%d:\n%s", tt.height, block)
			rows := strings.Split(block, "\n")[1:]
			if len(rows) != len(tt.want) {
				t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(tt.want), block)
			}
			for i, row := range rows {
				if !strings.HasPrefix(row, tt.want[i]) {
					t.Errorf("row %d = %q, want prefix %q", i, row, tt.want[i])
				}
			}
		})
	}
}

// TestChatPickerTimelineKeepsStepsWhenUndatingWidensDetail: dropping the date
// column widens a step's detail, and a long unbroken token that only fits the
// wider continuation line moves whole off the title line, making that step
// taller. The fold keeps the step anyway, and the kept steps keep their date
// column.
func TestChatPickerTimelineKeepsStepsWhenUndatingWidensDetail(t *testing.T) {
	now := time.Now()
	day := func(back, hour, minute int) time.Time {
		d := now.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, time.Local)
	}
	for _, token := range []int{59, 60} {
		t.Run(fmt.Sprintf("token %d", token), func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 21)
			m.width = 80
			m.session.Attention.Since = timestamppb.New(day(0, 0, 9))
			m.timeline = []*pb.TimelineStep{
				{At: timestamppb.New(day(1, 23, 0)), Title: "Build started", Detail: strings.Repeat("word ", 20), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(0, 0, 1)), Title: "Built", Detail: strings.Repeat("x", token) + " tail words", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(0, 0, 2)), Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
			}
			block := stripANSI(m.timelineBlock())
			t.Logf("timeline at 80x21:\n%s", block)
			rows := strings.Split(block, "\n")[1:]
			want := []string{"  … 1 earlier step", "  Today  00:01  ✓ Built", strings.Repeat(" ", 18) + "x", "         00:02  ✓ CI passed", "         00:09  ● Parked for you"}
			if len(rows) != len(want) {
				t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), block)
			}
			for i, row := range rows {
				if !strings.HasPrefix(row, want[i]) {
					t.Errorf("row %d = %q, want prefix %q", i, row, want[i])
				}
			}
		})
	}
}

// randomWrapText is a few words, some of them long unbroken tokens, mixing
// narrow and wide characters.
func randomWrapText(r *rand.Rand) string {
	glyphs := []string{"x", "y", "語", "🙂"}
	words := make([]string, r.IntN(6))
	for i := range words {
		var word strings.Builder
		for range 1 + r.IntN([]int{6, 30, 130}[r.IntN(3)]) {
			word.WriteString(glyphs[r.IntN(len(glyphs))])
		}
		words[i] = word.String()
	}
	return strings.Join(words, " ")
}

// logSweepCase names the case of a seeded sweep that failed or panicked, so a
// defect the nightly sweep finds can be brought into the per-PR prefix. A
// panic is still unwinding when this runs, before the test counts as failed.
func logSweepCase(t *testing.T, c *int) {
	if p := recover(); p != nil {
		t.Logf("panicked at case %d of the seeded sequence", *c)
		panic(p)
	}
	if t.Failed() {
		t.Logf("failed at case %d of the seeded sequence", *c)
	}
}

// The per-PR suite runs a prefix of each seeded sweep below, and the nightly
// job runs the whole sequence with viewsSweepsEnv set. Each prefix reaches the
// first failing case of every defect injected into its code when the count was
// set: the word wrap's by case 10, the fold's by case 32 and the timeline's by
// case 54. Two later ones have named tests instead: a fold that drops a step on
// relabelling (case 1176, TestChatPickerTimelineKeepsStepsWhenUndatingWidensDetail)
// and a timeline given one row too many beside the answer (case 83,
// TestChatPickerLongTimelineKeepsChatsVisible).
const (
	wordWrapPerPRCases     = 20
	wordWrapSweepCases     = 500
	foldPerPRCases         = 40
	foldSweepCases         = 4000
	timelineWaitPerPRCases = 60
	timelineWaitSweepCases = 3000
)

// TestWordWrapWiderFirstLineNeverAddsLines runs a prefix of
// sweepWordWrapWiderFirstLine.
func TestWordWrapWiderFirstLineNeverAddsLines(t *testing.T) {
	sweepWordWrapWiderFirstLine(t, wordWrapPerPRCases)
}

func TestViewsSweep_WordWrapWiderFirstLineNeverAddsLines(t *testing.T) {
	requireViewsSweeps(t)
	t.Parallel()
	sweepWordWrapWiderFirstLine(t, wordWrapSweepCases)
}

// sweepWordWrapWiderFirstLine: a wider first line never yields more lines,
// every width keeps the text and stays within its limits once rest holds a
// wide character, and a word no wider than rest is never split. A wider rest
// can add a line, since a word it stops overflowing moves whole off the first
// line; foldTimelineSteps carries the fold invariant that used to lean on rest
// monotonicity.
func sweepWordWrapWiderFirstLine(t *testing.T, cases int) {
	r := rand.New(rand.NewPCG(3185, 1))
	strip := func(s string) string { return strings.Join(strings.Fields(s), "") }
	var c int
	defer logSweepCase(t, &c)
	for c = 0; c < cases; c++ {
		text := randomWrapText(r)
		widths := map[string]int{}
		for _, word := range strings.Fields(text) {
			widths[word] = ansi.StringWidth(word)
		}
		gap := r.IntN(40)
		for rest := 0; rest <= 140; rest++ {
			first := rest - gap
			lines := wordWrap(text, first, rest)
			if got := strip(strings.Join(lines, "")); got != strip(text) {
				t.Fatalf("wordWrap(%q, %d, %d) lost text: %q", text, first, rest, lines)
			}
			if rest < 2 {
				continue
			}
			whole := map[string]bool{}
			for _, line := range lines {
				for _, word := range strings.Fields(line) {
					whole[word] = true
				}
			}
			for word, width := range widths {
				if width <= rest && !whole[word] {
					t.Fatalf("wordWrap(%q, %d, %d) split %q, which fits a %d-cell line: %q", text, first, rest, word, rest, lines)
				}
			}
			for i, line := range lines {
				limit := rest
				if i == 0 {
					limit = max(0, first)
				}
				if ansi.StringWidth(line) > limit {
					t.Fatalf("wordWrap(%q, %d, %d) line %d = %q is wider than %d", text, first, rest, i, line, limit)
				}
			}
		}
		rest := 2 + r.IntN(100)
		previous := -1
		for first := -2; first <= rest; first++ {
			n := len(wordWrap(text, first, rest))
			if previous >= 0 && n > previous {
				t.Fatalf("wordWrap(%q, %d, %d) = %d lines, a first line one cell narrower gave %d", text, first, rest, n, previous)
			}
			previous = n
		}
	}
}

// TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept runs a prefix of
// sweepFoldKeepsTheFirstFold.
func TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept(t *testing.T) {
	sweepFoldKeepsTheFirstFold(t, foldPerPRCases)
}

func TestViewsSweep_FoldTimelineStepsKeepsEveryStepTheFirstFoldKept(t *testing.T) {
	requireViewsSweeps(t)
	t.Parallel()
	sweepFoldKeepsTheFirstFold(t, foldSweepCases)
}

// sweepFoldKeepsTheFirstFold: relabelling the steps a fold keeps never costs
// one of them, at any width or budget, wherever the pinned step sits.
func sweepFoldKeepsTheFirstFold(t *testing.T, cases int) {
	r := rand.New(rand.NewPCG(3185, 2))
	now := time.Now()
	var c int
	defer logSweepCase(t, &c)
	for c = 0; c < cases; c++ {
		steps := make([]*pb.TimelineStep, 1+r.IntN(9))
		yesterday, titleWidth := r.IntN(len(steps)), 0
		for i := range steps {
			detail := randomWrapText(r)
			if r.IntN(2) == 0 {
				detail = strings.Repeat("x", 1+r.IntN(130)) + " tail words"
			}
			step := &pb.TimelineStep{Title: []string{"Built", "CI passed", "Build started", "Parked for you"}[r.IntN(4)], Detail: detail, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}
			if r.IntN(8) > 0 {
				d := now
				if i < yesterday {
					d = now.AddDate(0, 0, -1)
				}
				step.At = timestamppb.New(time.Date(d.Year(), d.Month(), d.Day(), 12, i, 0, 0, time.Local))
			}
			titleWidth = max(titleWidth, ansi.StringWidth(step.GetTitle()))
			steps[i] = step
		}
		pin, avail, budget := r.IntN(len(steps)), 10+r.IntN(120), 3+r.IntN(10)
		candidates := timelineOrder(len(steps), pin)
		candidates = candidates[:1+r.IntN(len(candidates))]
		slices.Sort(candidates)
		picked := make([]*pb.TimelineStep, len(candidates))
		for i, k := range candidates {
			picked[i] = steps[k]
		}
		folded, _ := fitTimelineRows(timelineStepRows(picked, timelineDayLabels(picked, now, false), titleWidth, avail), candidates, len(steps), slices.Index(candidates, pin), budget)
		kept, rows := foldTimelineSteps(steps, candidates, pin, now, titleWidth, avail, budget)
		if !slices.Equal(kept, folded) {
			t.Fatalf("avail %d, budget %d, pin %d: first fold kept %v, relabelled fold kept %v:\n%s", avail, budget, pin, folded, kept, rows)
		}
	}
}

// withRotations gives the fixture the daemon's narration of a usage-limit
// incident and the session's two account rotations: one at the instant of the
// daemon's resume step, which is that step, and a later manual switch the
// daemon does not narrate.
func withRotations(m ChatPickerModel) ChatPickerModel {
	since := m.session.GetAttention().GetSince().AsTime().Local()
	at := func(minute int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 23, minute, 0, 0, time.Local))
	}
	m.timeline = []*pb.TimelineStep{
		{At: at(4), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(6), Title: "Hit its usage limit", Detail: "usage limit reached", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: at(7), Title: "Resumed automatically", Detail: "switched account", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(9), Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(10), Title: "Waiting", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
	m.session.RotationEvents = []*pb.RotationEvent{
		{Id: "manual", Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, FromAccount: "dave@kamik.ai", ToAccount: "yuki@kamik.ai", Detail: "resumed", CreatedAt: at(8)},
		{Id: "limit", Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, Trigger: pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED, FromAccount: "yuki@kamik.ai", ToAccount: "dave@kamik.ai", Detail: "resumed", CreatedAt: at(7)},
	}
	m.buildTableRows()
	return m
}

// timelineRowsOf is the rendered timeline below its heading, styling removed.
func timelineRowsOf(m ChatPickerModel) []string {
	return strings.Split(stripANSI(m.timelineBlock()), "\n")[1:]
}

// TestChatPickerTimelineMergesRotationsByTime: the session's rotations join the
// daemon's steps in time order. A rotation the daemon already narrates stays
// one step, the daemon's, now naming the accounts; one it does not narrate
// becomes its own step between its neighbours.
func TestChatPickerTimelineMergesRotationsByTime(t *testing.T) {
	m := withRotations(detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40))
	rows := timelineRowsOf(m)
	want := []string{
		"  23:04  ✓ Build started",
		"  23:06  ✗ Hit its usage limit    usage limit reached",
		"  23:07  ✓ Resumed automatically  yuki@kamik.ai switched to dave@kamik.ai — resumed",
		"  23:08  ✓ Account rotation       dave@kamik.ai switched to yuki@kamik.ai — resumed",
		"  23:09  ✓ CI passed",
		"● Waiting for a fix",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), strings.Join(rows, "\n"))
	}
	for i, row := range rows {
		if !strings.Contains(row, want[i]) {
			t.Errorf("row %d = %q, want %q", i, row, want[i])
		}
	}
	block := stripANSI(m.timelineBlock())
	if got := strings.Count(block, "switched to dave@kamik.ai"); got != 1 {
		t.Fatalf("the usage-limit rotation shows %d times, want once:\n%s", got, block)
	}
	if strings.Contains(block, "switched account") {
		t.Fatalf("the merged step kept the daemon's account-less detail:\n%s", block)
	}
}

// TestChatPickerTimelineShowsRotationsWithoutAttention: a session with nothing
// waiting has no daemon history, so its timeline is its rotations, oldest
// first, with a failed switch marked failed.
func TestChatPickerTimelineShowsRotationsWithoutAttention(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 40)
	m.timeline = nil
	m.session.RotationEvents = []*pb.RotationEvent{
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_FAILED, ToAccount: "b@x", CreatedAt: timestamppb.New(time.Now().Add(-time.Minute))},
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, FromAccount: "a@x", ToAccount: "c@x", CreatedAt: timestamppb.New(time.Now().Add(-time.Hour))},
	}
	rows := timelineRowsOf(m)
	if len(rows) != 2 || !strings.Contains(rows[0], "✓ Account rotation  a@x switched to c@x") || !strings.Contains(rows[1], "✗ Account rotation  switch to b@x failed") {
		t.Fatalf("rotation-only timeline:\n%s", strings.Join(rows, "\n"))
	}
	if screen := stripANSI(m.View().Content); strings.Contains(screen, "Rotation history") {
		t.Fatalf("the separate rotation block still renders:\n%s", screen)
	}
}

// expectTimelineRows fails unless rows match want one for one, each row
// containing its wanted text.
func expectTimelineRows(t *testing.T, rows, want []string) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), strings.Join(rows, "\n"))
	}
	for i, row := range rows {
		if !strings.Contains(row, want[i]) {
			t.Errorf("row %d = %q, want %q", i, row, want[i])
		}
	}
}

// TestChatPickerTimelineWaitOnAGiveUpKeepsItsReason: a FIX waiting on a
// rotation give-up shares the give-up's instant, because the daemon marks the
// give-up's step current in place. The waiting row keeps the attention's
// reason, and the give-up rotation stays its own failed row just ahead of it.
func TestChatPickerTimelineWaitOnAGiveUpKeepsItsReason(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40)
	since := m.session.GetAttention().GetSince().AsTime().Local()
	at := func(minute int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 23, minute, 0, 0, time.Local))
	}
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	m.timeline = []*pb.TimelineStep{
		{At: at(4), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(6), Title: "Lost its login", Detail: "login expired", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: at(6), Title: "Resumed automatically", Detail: "switched account", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(9), Title: "Stopped resuming automatically", Detail: "gave up", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
	m.session.RotationEvents = []*pb.RotationEvent{
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, Trigger: auth, CreatedAt: at(9)},
		{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, Trigger: auth, FromAccount: "a@x", ToAccount: "b@x", CreatedAt: at(6)},
	}
	expectTimelineRows(t, timelineRowsOf(m), []string{
		"23:04  ✓ Build started",
		"23:06  ✗ Lost its login",
		"23:06  ✓ Resumed automatically  a@x switched to b@x",
		"23:09  ✗ Account rotation       stopped restarting the agent: it stayed signed out",
		"23:09  ● Waiting for a fix      Failed after 3 attempts on this machine",
	})
}

// TestChatPickerTimelineRotationMergesOnlyIntoItsNarration: a rotation joins
// a daemon step at its instant only when that step narrates it, and a step
// takes one rotation at most. A manual switch at the instant of "CI passed"
// and a second rotation at the instant of one resume stay rows of their own.
func TestChatPickerTimelineRotationMergesOnlyIntoItsNarration(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40)
	since := m.session.GetAttention().GetSince().AsTime().Local()
	at := func(minute int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 23, minute, 0, 0, time.Local))
	}
	usage := pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED
	rotated := pb.RotationOutcome_ROTATION_OUTCOME_ROTATED
	m.timeline = []*pb.TimelineStep{
		{At: at(6), Title: "Hit its usage limit", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: at(7), Title: "Resumed automatically", Detail: "switched account", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(8), Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: at(10), Title: "Waiting", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}
	m.session.RotationEvents = []*pb.RotationEvent{
		{Outcome: rotated, Trigger: pb.RotationTrigger_ROTATION_TRIGGER_MANUAL, FromAccount: "m@x", ToAccount: "n@x", CreatedAt: at(8)},
		{Outcome: rotated, Trigger: usage, FromAccount: "c@x", ToAccount: "d@x", CreatedAt: at(7)},
		{Outcome: rotated, Trigger: usage, FromAccount: "a@x", ToAccount: "b@x", CreatedAt: at(7)},
	}
	rows := timelineRowsOf(m)
	expectTimelineRows(t, rows, []string{
		"23:06  ✗ Hit its usage limit",
		"23:07  ✓ Account rotation       a@x switched to b@x",
		"23:07  ✓ Resumed automatically  c@x switched to d@x",
		"23:08  ✓ Account rotation       m@x switched to n@x",
		"23:08  ✓ CI passed",
		"23:10  ● Waiting for a fix",
	})
	if strings.Contains(strings.Join(rows, "\n"), "CI passed  m@x") {
		t.Fatalf("CI passed swallowed the manual switch:\n%s", strings.Join(rows, "\n"))
	}
}

// TestChatPickerTimelineMarksEveryGiveUpFailed: a rotation the daemon does
// not narrate is failed exactly when the daemon stopped trying after it, the
// status-only outcomes and the respawn cap included.
func TestChatPickerTimelineMarksEveryGiveUpFailed(t *testing.T) {
	giveUps := map[pb.RotationOutcome]bool{
		pb.RotationOutcome_ROTATION_OUTCOME_FAILED:                          true,
		pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED:            true,
		pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY:       true,
		pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT: true,
		pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED:           true,
	}
	for value := range pb.RotationOutcome_name {
		outcome := pb.RotationOutcome(value)
		if outcome == pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED {
			continue
		}
		t.Run(outcome.String(), func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 40)
			m.timeline = nil
			m.session.RotationEvents = []*pb.RotationEvent{{Outcome: outcome, ToAccount: "b@x", CreatedAt: timestamppb.New(time.Now().Add(-time.Minute))}}
			rows := timelineRowsOf(m)
			want := "✓ Account rotation"
			if giveUps[outcome] {
				want = "✗ Account rotation"
			}
			if len(rows) != 1 || !strings.Contains(rows[0], want) {
				t.Fatalf("rows = %q, want one row with %q", rows, want)
			}
		})
	}
}

// TestChatPickerTimelineSitsUnderTheChatList: the answer stays above the chat
// table, and the timeline, rotations included, renders under the chat list
// and its action bar, with no separate rotation block.
func TestChatPickerTimelineSitsUnderTheChatList(t *testing.T) {
	m := withRotations(detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40))
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	row := func(text string) int {
		for i, line := range lines {
			if strings.Contains(line, text) {
				return i
			}
		}
		t.Fatalf("missing %q:\n%s", text, strings.Join(lines, "\n"))
		return -1
	}
	needed, header, lastChat, bar, timeline := row("Needed from you:"), row("CHAT"), row("Run bs-build"), row("[esc] back"), row("Timeline")
	if needed >= header || lastChat >= bar || bar >= timeline {
		t.Fatalf("needed row %d, table header %d, last chat %d, action bar %d, timeline %d:\n%s", needed, header, lastChat, bar, timeline, strings.Join(lines, "\n"))
	}
	if rotation := row("dave@kamik.ai switched to yuki@kamik.ai"); rotation <= timeline {
		t.Fatalf("rotation row %d is not on the timeline starting at %d", rotation, timeline)
	}
	if strings.Contains(strings.Join(lines, "\n"), "Rotation history") {
		t.Fatal("the separate rotation block still renders")
	}
}

// TestChatPickerTimelineUsesTheAccountSwapStyle: every timeline line, heading,
// fold line and wrapped continuation included, renders in styleStatusMuted,
// the style the rotation-history block used.
func TestChatPickerTimelineUsesTheAccountSwapStyle(t *testing.T) {
	m := withRotations(detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 21))
	m.timeline[3].Detail = strings.Repeat("all checks green ", 20)
	lines := strings.Split(m.timelineBlock(), "\n")
	if plain := stripANSI(strings.Join(lines, "\n")); !strings.Contains(plain, "earlier step") || !strings.Contains(plain, "…\n") {
		t.Fatalf("fixture lacks a fold line or a cut continuation:\n%s", plain)
	}
	for _, line := range lines {
		plain := stripANSI(line)
		if line != styleStatusMuted.Render(plain) || line == plain {
			t.Errorf("timeline line %q rendered %q, want it in styleStatusMuted", plain, line)
		}
	}
}

// TestChatPickerTimelineKeepsAPathWholeOnTheNextLine: a detail whose first
// word overflows the title line but fits a continuation line starts the next
// line whole instead of splitting mid-word, while a word wider than any line
// still fills the title line.
func TestChatPickerTimelineKeepsAPathWholeOnTheNextLine(t *testing.T) {
	const path = "services/boss/internal/views/session_timeline.go"
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 40)
	m.width = 80
	m.timeline[3].Detail = path + " changed"
	rows := timelineRowsOf(m)
	evidence := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, "Evidence recorded") })
	if evidence < 0 || evidence+1 >= len(rows) {
		t.Fatalf("no evidence step with a continuation:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.HasSuffix(rows[evidence], "Evidence recorded") {
		t.Errorf("title line carries part of the path: %q", rows[evidence])
	}
	if got := strings.TrimSpace(rows[evidence+1]); got != path+" changed" {
		t.Errorf("continuation = %q, want the whole path", got)
	}
	if got := wordWrap(strings.Repeat("x", 30)+" tail", 10, 20); got[0] != strings.Repeat("x", 10) {
		t.Errorf("a word wider than a continuation line no longer fills the title line: %q", got)
	}
}

// TestChatPickerLongTimelineKeepsChatsVisible: under a long history and many
// rotations the chat table keeps its rows and the frame its height, at every
// size, and the fold accounts for every step.
func TestChatPickerLongTimelineKeepsChatsVisible(t *testing.T) {
	for _, width := range []int{80, 100, 120} {
		for height := 18; height <= 40; height++ {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, height)
			m.width = width
			since := m.session.GetAttention().GetSince().AsTime().Local()
			stamp := func(minute int) *timestamppb.Timestamp {
				return timestamppb.New(time.Date(since.Year(), since.Month(), since.Day(), 0, minute, 0, 0, time.Local))
			}
			m.timeline = nil
			for i := range 50 {
				m.timeline = append(m.timeline, &pb.TimelineStep{At: stamp(2 * i), Title: fmt.Sprintf("Run %d", i+1), Detail: strings.Repeat("detail ", i%15), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
			}
			m.timeline = append(m.timeline, &pb.TimelineStep{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT})
			m.session.RotationEvents = nil
			for i := range 20 {
				m.session.RotationEvents = append([]*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, FromAccount: "a@x", ToAccount: "b@x", CreatedAt: stamp(2*i + 1)}}, m.session.RotationEvents...)
			}
			for _, id := range []string{"third", "fourth", "fifth"} {
				m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: id, Title: id + " chat", CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
				m.daemonStatuses[id] = statusWaiting
			}
			m.buildTableRows()
			m.table.SetHeight(m.tableHeight())
			screen := stripANSI(m.View().Content)
			if got := lipgloss.Height(screen) + bannerOverhead; got > height {
				t.Fatalf("%dx%d: view uses %d rows:\n%s", width, height, got, screen)
			}
			if !strings.Contains(screen, "[esc] back") || !strings.Contains(screen, "Needed from you") {
				t.Fatalf("%dx%d: footer or answer pushed off:\n%s", width, height, screen)
			}
			if m.timelineBlock() == "" {
				if height >= 24 {
					t.Fatalf("%dx%d: no timeline on a terminal with room for one:\n%s", width, height, screen)
				}
				continue
			}
			// A timeline on screen means every chat the table reserves is too.
			for _, text := range []string{"verify", "Run bs-build", "third chat", "fourth chat", "fifth chat", "Timeline", "Parked for you"} {
				if !strings.Contains(screen, text) {
					t.Fatalf("%dx%d: missing %q:\n%s", width, height, text, screen)
				}
			}
			shown, earlier := 0, 0
			for _, row := range timelineRowsOf(m) {
				if strings.Contains(row, "✓ ") || strings.Contains(row, "● ") {
					shown++
				}
				_, _ = fmt.Sscanf(strings.TrimSpace(row), "… %d earlier", &earlier)
			}
			if earlier+shown != 71 {
				t.Fatalf("%dx%d: %d earlier plus %d shown, want all 71 steps:\n%s", width, height, earlier, shown, screen)
			}
		}
	}
}

var (
	timelineStepRow = regexp.MustCompile(`^  .*?  ([✓✗●]) (.*?)(  |…|$)`)
	timelineFoldRow = regexp.MustCompile(`^  … (\d+) (earlier|later) steps?$`)
)

// assertTimelineFold reads the rendered timeline back against the steps it
// draws from: every step row is the next step not yet accounted for, every
// fold line counts the run of steps it stands for and says "earlier" only
// ahead of the first step row, and the rows and folds account for every step.
// It reports whether the timeline shows, and fails if it shows without the
// waiting step or takes a row the chat table reserves.
func assertTimelineFold(t *testing.T, m ChatPickerModel) bool {
	t.Helper()
	block := stripANSI(m.timelineBlock())
	if block == "" {
		return false
	}
	if got, want := m.tableHeight(), m.detailsReservedTableRows(); got < want {
		t.Fatalf("%dx%d: the chat table keeps %d rows beside the timeline, want its %d reserved rows:\n%s", m.width, m.height, got, want, stripANSI(m.View().Content))
	}
	steps := m.timelineSteps()
	next, current, seenStep := 0, 0, false
	for _, row := range strings.Split(block, "\n")[1:] {
		if fold := timelineFoldRow.FindStringSubmatch(row); fold != nil {
			n, _ := strconv.Atoi(fold[1])
			if (fold[2] == "earlier") == seenStep || n < 1 {
				t.Fatalf("fold line %q after a step row = %v:\n%s", row, seenStep, block)
			}
			next += n
			continue
		}
		step := timelineStepRow.FindStringSubmatch(row)
		if step == nil {
			continue
		}
		if next >= len(steps) || !strings.HasPrefix(steps[next].GetTitle(), step[2]) {
			t.Fatalf("row %q is not step %d of %d:\n%s", row, next, len(steps), block)
		}
		if step[1] == "●" {
			current++
		}
		seenStep = true
		next++
	}
	if next != len(steps) {
		t.Fatalf("rows and folds account for %d of %d steps:\n%s", next, len(steps), block)
	}
	if want := slices.ContainsFunc(steps, func(s *pb.TimelineStep) bool { return s.GetState() == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT }); current > 1 || (current == 1) != want {
		t.Fatalf("timeline shows %d waiting steps, want one = %v:\n%s", current, want, block)
	}
	return true
}

// waitAmongLaterSteps is a fix the session has waited on since 10:05, after
// four earlier steps and with later runs after it.
func waitAmongLaterSteps(height, later int) ChatPickerModel {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, height)
	m.width = 80
	day := m.session.GetAttention().GetSince().AsTime().Local()
	clock := func(minute int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(day.Year(), day.Month(), day.Day(), 10, minute, 0, 0, time.Local))
	}
	m.timeline = nil
	for i, title := range []string{"Build started", "Built", "CI failed", "Repair started"} {
		m.timeline = append(m.timeline, &pb.TimelineStep{At: clock(i + 1), Title: title, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
	}
	m.timeline = append(m.timeline, &pb.TimelineStep{At: clock(5), Title: "Parked for you", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT})
	for i := range later {
		m.timeline = append(m.timeline, &pb.TimelineStep{At: clock(10 + i), Title: fmt.Sprintf("Resumed %d", i), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
	}
	m.buildTableRows()
	return m
}

// The waiting step sits in place, so later steps follow it. The fold keeps it
// however many follow and however short the terminal, folding the steps on
// either side of it; a terminal too short for it and its folds shows no
// timeline rather than one without it.
func TestChatPickerTimelineKeepsTheWaitAmongLaterSteps(t *testing.T) {
	for _, later := range []int{1, 2, 3, 6, 8, 12} {
		for height := 16; height <= 60; height++ {
			m := waitAmongLaterSteps(height, later)
			if !assertTimelineFold(t, m) && height >= 19 {
				t.Fatalf("%d later steps at 80x%d: no timeline:\n%s", later, height, stripANSI(m.View().Content))
			}
		}
	}
	for _, tt := range []struct {
		name          string
		height, later int
		want          []string
	}{
		{"two later at 18 rows", 18, 2, nil},
		{"two later at 19 rows", 19, 2, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 2 later steps"}},
		{"two later at 20 rows", 20, 2, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{"two later at 21 rows", 21, 2, []string{"  … 3 earlier steps", "  10:04  ✓ Repair started", "  10:05  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{"three later at 20 rows", 20, 3, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 2 later steps", "  10:12  ✓ Resumed 2"}},
		{"six later at 38 rows", 38, 6, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 1 later step", "  10:11  ✓ Resumed 1", "  10:12  ✓ Resumed 2", "  10:13  ✓ Resumed 3", "  10:14  ✓ Resumed 4", "  10:15  ✓ Resumed 5"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := waitAmongLaterSteps(tt.height, tt.later)
			block := stripANSI(m.timelineBlock())
			t.Logf("timeline at 80x%d:\n%s", tt.height, block)
			if tt.want == nil {
				if block != "" {
					t.Fatalf("a timeline too short for the waiting step and its folds shows:\n%s", block)
				}
				return
			}
			rows := strings.Split(block, "\n")[1:]
			if len(rows) != len(tt.want) {
				t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(tt.want), block)
			}
			for i, row := range rows {
				if strings.TrimRight(row, " ") != tt.want[i] && !strings.HasPrefix(row, tt.want[i]+"  ") {
					t.Errorf("row %d = %q, want %q", i, row, tt.want[i])
				}
			}
		})
	}
}

// A timeline fetched out of step with the session row can lack the daemon's
// current step while attention still needs the operator. The waiting step then
// goes in at attention's since among the daemon's steps, not drawn last below
// a later one.
func TestChatPickerTimelineWaitWithoutTheDaemonsStepKeepsTimeOrder(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 60)
	day := m.session.GetAttention().GetSince().AsTime().Local()
	clock := func(minute int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(day.Year(), day.Month(), day.Day(), 10, minute, 0, 0, time.Local))
	}
	m.timeline = []*pb.TimelineStep{
		{At: clock(0), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: clock(1), Title: "Run ended unexpectedly", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: clock(10), Title: "Resumed 0", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: clock(11), Title: "Resumed 1", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
	}
	for _, tt := range []struct {
		since int
		want  []string
	}{
		{1, []string{"  10:00  ✓ Build started", "  10:01  ✗ Run ended unexpectedly", "  10:01  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{5, []string{"  10:00  ✓ Build started", "  10:01  ✗ Run ended unexpectedly", "  10:05  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{20, []string{"  10:00  ✓ Build started", "  10:01  ✗ Run ended unexpectedly", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1", "  10:20  ● Waiting for a fix"}},
	} {
		m.session.Attention.Since = clock(tt.since)
		rows := timelineRowsOf(m)
		if len(rows) != len(tt.want) {
			t.Fatalf("since 10:%02d: rows = %d, want %d:\n%s", tt.since, len(rows), len(tt.want), strings.Join(rows, "\n"))
		}
		for i, row := range rows {
			if !strings.HasPrefix(row, tt.want[i]) {
				t.Errorf("since 10:%02d: row %d = %q, want prefix %q", tt.since, i, row, tt.want[i])
			}
		}
	}
}

// TestChatPickerTimelineAlwaysShowsTheWait runs a prefix of
// sweepTimelineShowsTheWait.
func TestChatPickerTimelineAlwaysShowsTheWait(t *testing.T) {
	sweepTimelineShowsTheWait(t, timelineWaitPerPRCases)
}

func TestViewsSweep_ChatPickerTimelineAlwaysShowsTheWait(t *testing.T) {
	requireViewsSweeps(t)
	t.Parallel()
	sweepTimelineShowsTheWait(t, timelineWaitSweepCases)
}

// sweepTimelineShowsTheWait is a random sweep over timelines, waiting
// positions, rotations, kinds and terminal sizes: whenever the timeline shows,
// it shows the waiting step, its rows keep time order, its folds account for
// every step, and the view stays within the terminal.
func sweepTimelineShowsTheWait(t *testing.T, cases int) {
	r := rand.New(rand.NewPCG(3200, 1))
	kinds := []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_MERGE}
	// A fixed day, never today, gives every case the same rows at any hour and
	// in any time zone.
	day := time.Date(2026, time.March, 2, 0, 0, 0, 0, time.Local)
	shown := 0
	var c int
	defer logSweepCase(t, &c)
	for c = 0; c < cases; c++ {
		height, width := 14+r.IntN(50), 40+r.IntN(111)
		m := detailFixture(kinds[r.IntN(len(kinds))], height)
		m.width = width
		n := 1 + r.IntN(14)
		current := r.IntN(n + 1)
		// The steps sit at 09:00 and every two minutes after it, and the wait
		// begins before, at, between or after them.
		m.session.Attention.Since = timestamppb.New(time.Date(day.Year(), day.Month(), day.Day(), 9, r.IntN(2*n+3)-1, 0, 0, time.Local))
		m.timeline = nil
		for i := range n {
			d := day.AddDate(0, 0, -r.IntN(2)*(n-i)/n)
			step := &pb.TimelineStep{At: timestamppb.New(time.Date(d.Year(), d.Month(), d.Day(), 9, 2*i, 0, 0, time.Local)), Title: fmt.Sprintf("Step %d", i), Detail: strings.Repeat("detail ", r.IntN(20)), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}
			if r.IntN(5) == 0 {
				step.State = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
			}
			if i == current {
				step.State = pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
			}
			m.timeline = append(m.timeline, step)
		}
		m.session.RotationEvents = nil
		for range r.IntN(4) {
			at := time.Date(day.Year(), day.Month(), day.Day(), 9, 2*r.IntN(n)+1, 0, 0, time.Local)
			m.session.RotationEvents = append(m.session.RotationEvents, &pb.RotationEvent{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, FromAccount: "a@x", ToAccount: "b@x", CreatedAt: timestamppb.New(at)})
		}
		slices.SortFunc(m.session.RotationEvents, func(a, b *pb.RotationEvent) int { return b.GetCreatedAt().AsTime().Compare(a.GetCreatedAt().AsTime()) })
		for i := range r.IntN(4) {
			id := fmt.Sprintf("chat-%d", i)
			m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: id, Title: id, CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
			m.daemonStatuses[id] = statusWaiting
		}
		m.buildTableRows()
		m.table.SetHeight(m.tableHeight())
		if !assertTimelineFold(t, m) {
			continue
		}
		shown++
		steps := m.timelineSteps()
		for i := 1; i < len(steps); i++ {
			if steps[i].GetAt() != nil && steps[i-1].GetAt() != nil && steps[i].GetAt().AsTime().Before(steps[i-1].GetAt().AsTime()) {
				t.Fatalf("step %d %q at %s follows step %q at %s", i, steps[i].GetTitle(), steps[i].GetAt().AsTime(), steps[i-1].GetTitle(), steps[i-1].GetAt().AsTime())
			}
		}
		screen := stripANSI(m.View().Content)
		if got := lipgloss.Height(screen) + bannerOverhead; got > height {
			t.Fatalf("%dx%d: view uses %d rows:\n%s", width, height, got, screen)
		}
		for _, line := range strings.Split(screen, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("%dx%d: overwide line %q", width, height, line)
			}
		}
	}
	if 2*shown < cases {
		t.Fatalf("the timeline showed in only %d of %d cases", shown, cases)
	}
	t.Logf("the timeline showed in %d of %d cases", shown, cases)
}
