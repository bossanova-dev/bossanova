package views

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
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

func (c *timelineClient) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
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
			m.neededOffset = 10
			if change == "target" {
				s.Attention.TargetChatId = "second"
			} else {
				s.Attention.Since = timestamppb.New(time.Unix(100, 1))
			}
			s.Attention.Sentence = "Second question?"
			c.question = "Second full question?"
			model, cmd = m.Update(chatPickerRefreshMsg{session: s})
			m = model.(ChatPickerModel)
			if m.fullQuestion != "" || m.neededOffset != 0 || m.neededQuestion() != s.Attention.Sentence {
				t.Fatalf("old question retained while fetching: %q, offset %d", m.neededQuestion(), m.neededOffset)
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
			// A blocked park's row offers no merge, so it names the ways forward.
			if !strings.Contains(q, "Open a chat to fix it") || !strings.Contains(q, "boss pr approve") {
				t.Fatalf("blocked park needed text lacks its ways forward: %q", q)
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
	m.chats = []*pb.ClaudeChat{{AgentSessionId: "verify", Title: "verify", CreatedAt: timestamppb.New(now), AgentName: "claude"}, {AgentSessionId: "build", Title: "Run bs-build", CreatedAt: timestamppb.New(now), AgentName: "claude"}}
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
	}{{"review", pb.AttentionKind_ATTENTION_KIND_REVIEW, 40}, {"review-24", pb.AttentionKind_ATTENTION_KIND_REVIEW, 24}, {"fix", pb.AttentionKind_ATTENTION_KIND_FIX, 24}, {"legacy", pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 24}} {
		t.Run(tt.name, func(t *testing.T) {
			m := detailFixture(tt.kind, tt.height)
			screen := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: m.session, width: m.width}) + "\n" + m.View().Content)
			if tt.kind != pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
				screen = strings.ReplaceAll(screen, m.session.GetAttention().GetSince().AsTime().Local().Format("15:04")+"  ●", "23:40  ●")
			}
			path := filepath.Join("testdata", "chatpicker-timeline-"+tt.name+".golden")
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
			if lipgloss.Height(screen) > tt.height {
				t.Fatalf("render uses %d rows, budget %d", lipgloss.Height(screen), tt.height)
			}
			for _, line := range strings.Split(screen, "\n") {
				if lipgloss.Width(line) > m.width {
					t.Fatalf("overwide line %q", line)
				}
			}
			if tt.kind != pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED {
				for _, text := range []string{"Timeline", "Needed from you", "done · standby: wakes if CI turns red", "verify", "[enter] open chat", "head 2bbaede"} {
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

func TestChatPickerAttentionDetailsContracts(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 40)
	m.fullQuestion = strings.Repeat("Should I retain this behavior across all supported platforms? ", 10)
	block := stripANSI(m.attentionDetailsBlock())
	if strings.Join(strings.Fields(block[strings.Index(block, "Needed from you:")+len("Needed from you:"):]), " ") != strings.TrimSpace(m.fullQuestion) {
		t.Fatal("full question truncated")
	}
	m.session.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_REVIEW
	m.session.Attention.ParkCodes = []string{"human-review:security"}
	if m.neededQuestion() != "Is this safe to merge?" {
		t.Fatal("security question missing")
	}
	for _, tt := range []struct {
		kind  pb.AttentionKind
		label string
	}{{pb.AttentionKind_ATTENTION_KIND_FIX, "fix · failed"}, {pb.AttentionKind_ATTENTION_KIND_QUESTION, "question · asked"}, {pb.AttentionKind_ATTENTION_KIND_REVIEW, "review · parked"}, {pb.AttentionKind_ATTENTION_KIND_MERGE, "merge · ready"}} {
		m.session.Attention.Kind = tt.kind
		if !strings.HasPrefix(attentionDetailStatus(m.session), tt.label) {
			t.Errorf("header %s", attentionDetailStatus(m.session))
		}
	}
	m.session.Attention.Kind = pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS
	m.session.Attention.ProgressLabel = "building"
	if !strings.HasPrefix(attentionDetailStatus(m.session), "building") || m.neededQuestion() != "" {
		t.Fatal("progress header/needed block")
	}
	m.timeline[0].Detail = "https://secret.example/video"
	m.timeline[0].State = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
	if block := m.timelineBlock(); strings.Contains(block, "https://") || !strings.Contains(block, "✗") {
		t.Fatalf("failed step or URL rendering: %s", block)
	}
}

// TestChatPickerNeededAnswerIsEmphasised: the answer under "Needed from you"
// is the one line the details screen asks the operator to act on, so every
// line of it renders bold in the primary foreground, never faint, while the
// heading stays plain.
func TestChatPickerNeededAnswerIsEmphasised(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 40)
	m.fullQuestion = strings.Repeat("Should I retain this behavior across all supported platforms? ", 4)
	block := m.attentionDetailsBlock()
	lines := strings.Split(block, "\n")
	heading := -1
	for i, line := range lines {
		if stripANSI(line) == "Needed from you:" {
			heading = i
		}
	}
	if heading < 0 || heading == len(lines)-1 {
		t.Fatalf("no answer under the heading:\n%s", stripANSI(block))
	}
	if lines[heading] != "Needed from you:" {
		t.Fatalf("heading is styled: %q", lines[heading])
	}
	for _, line := range lines[heading+1:] {
		text := strings.TrimPrefix(stripANSI(line), neededIndent)
		if want := neededIndent + styleEmphasis.Render(text); line != want {
			t.Fatalf("answer line %q rendered %q, want bold %q", text, line, want)
		}
		if line == styleSubtle.Render(text) || line == text {
			t.Fatalf("answer line %q is not emphasised: %q", text, line)
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
// title never shows the API's "needs sign-off" label. Without attention it
// carries the TUI's "review" mapping; with attention the second line says
// what the session waits on and the title carries no status suffix.
func TestChatPickerDetailsBannerSaysReviewNotNeedsSignOff(t *testing.T) {
	sess := &pb.Session{Id: "s", Title: "Hide the line", PrNumber: new(int32(3154)), DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: displaystatus.NeedsSignOffLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	banner := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: sess, width: 160}))
	if !strings.Contains(banner, "Hide the line (review)") || strings.Contains(banner, displaystatus.NeedsSignOffLabel) {
		t.Fatalf("banner without attention:\n%s", banner)
	}
	sess.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui"}, Since: timestamppb.Now()}
	banner = stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: sess, width: 160}))
	if strings.Contains(banner, displaystatus.NeedsSignOffLabel) || strings.Contains(banner, "Hide the line (") || !strings.Contains(banner, "review · parked") {
		t.Fatalf("banner with attention:\n%s", banner)
	}
}

func TestChatPickerLongQuestionKeepsChatsVisible(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 24)
	m.fullQuestion = strings.Repeat("Should we keep this behavior for all supported platforms? ", 40) + "Can you confirm the final choice?"
	screen := m.View().Content
	if got := lipgloss.Height(screen) + bannerOverhead; got > 24 {
		t.Fatalf("long question uses %d rows, want <=24", got)
	}
	for _, text := range []string{"verify", "Run bs-build", "[enter] open chat", "[n]ew chat", "pgup/pgdown"} {
		if !strings.Contains(screen, text) {
			t.Fatalf("missing %q: %s", text, screen)
		}
	}
	for range 100 {
		model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		m = model.(ChatPickerModel)
	}
	if !strings.Contains(strings.Join(strings.Fields(stripANSI(m.View().Content)), " "), "Can you confirm the final choice?") {
		t.Fatalf("cannot read end offset=%d: %s", m.neededOffset, m.View().Content)
	}
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = model.(ChatPickerModel)
	if strings.Contains(strings.Join(strings.Fields(stripANSI(m.View().Content)), " "), "Can you confirm the final choice?") {
		t.Fatal("page up did not scroll question")
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

// TestChatPickerNeededFromYouLabelAndIndent: the answer sits under a
// "Needed from you:" label, set in two columns, wrapped to the block width
// less that indent, on word boundaries.
func TestChatPickerNeededFromYouLabelAndIndent(t *testing.T) {
	m := questionTimelineModel(80, 60, operatorQuestion)
	m.fullQuestion = operatorQuestion
	block := stripANSI(m.attentionDetailsBlock())
	t.Logf("details at 80 columns:\n%s", block)
	_, answer, ok := strings.Cut(block, "\nNeeded from you:\n")
	if !ok {
		t.Fatalf("no \"Needed from you:\" label:\n%s", block)
	}
	lines := strings.Split(answer, "\n")
	if len(lines) < 2 {
		t.Fatalf("answer did not wrap at %d: %q", m.blockWrapWidth(), answer)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, neededIndent) || strings.HasPrefix(line, neededIndent+" ") {
			t.Errorf("answer line not indented by %q: %q", neededIndent, line)
		}
		if w := ansi.StringWidth(line); w > m.blockWrapWidth() {
			t.Errorf("answer line is %d wide, block width %d: %q", w, m.blockWrapWidth(), line)
		}
	}
	if got := strings.Join(strings.Fields(answer), " "); got != operatorQuestion {
		t.Fatalf("answer words lost or split: %q", got)
	}
}

// TestChatPickerTimelineFitsShortTerminal: the answer below the timeline is
// what the operator acts on, so a wrapped detail gets only the rows the chat
// table and the whole answer leave. When they leave none it is cut to one line
// ending in an ellipsis, the chats and footer stay on screen, and an answer
// that still does not fit pages as before. A tall terminal still caps a step at timelineMaxStepRows.
func TestChatPickerTimelineFitsShortTerminal(t *testing.T) {
	long := strings.Repeat("Should we keep the behavior for every supported platform? ", 12) + "FINAL?"
	for _, tt := range []struct {
		name         string
		height, rows int
		question     string
		paged        bool
	}{
		{"long answer at 24 rows", 24, 1, long, true},
		{"short answer at 21 rows", 21, 1, operatorQuestion, true},
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
			if lines, _, size := m.neededWindow(); (len(lines) > size) != tt.paged || strings.Contains(screen, "pgup/pgdown") != tt.paged {
				t.Fatalf("answer paged = %v, want %v: %d lines in a %d-line window", len(lines) > size, tt.paged, len(lines), size)
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
				m.chats = append(m.chats, &pb.ClaudeChat{AgentSessionId: id, Title: id + " chat", CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
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
		{"fold drops the labelled step", 23, []string{
			"  … 2 earlier steps",
			"  " + day(1, 0, 0).Format("Jan _2") + " 23:50  ✓ B",
			"         23:55  ✓ B2",
			"  Today  00:01  ✓ C",
			"         00:05  ● Parked for you",
		}},
		{"fold leaves only today", 21, []string{
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
// wider continuation line must not make that step taller and fold it away.
func TestChatPickerTimelineKeepsStepsWhenUndatingWidensDetail(t *testing.T) {
	now := time.Now()
	day := func(back, hour, minute int) time.Time {
		d := now.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, time.Local)
	}
	for _, token := range []int{59, 60} {
		t.Run(fmt.Sprintf("token %d", token), func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 23)
			m.width = 80
			m.session.Attention.Since = timestamppb.New(day(0, 0, 9))
			m.timeline = []*pb.TimelineStep{
				{At: timestamppb.New(day(1, 23, 0)), Title: "Build started", Detail: strings.Repeat("word ", 20), State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(0, 0, 1)), Title: "Built", Detail: strings.Repeat("x", token) + " tail words", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: timestamppb.New(day(0, 0, 2)), Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{Title: "Parked", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
			}
			block := stripANSI(m.timelineBlock())
			t.Logf("timeline at 80x23:\n%s", block)
			rows := strings.Split(block, "\n")[1:]
			want := []string{"  … 1 earlier step", "  00:01  ✓ Built", strings.Repeat(" ", 11) + "x", "  00:02  ✓ CI passed", "  00:09  ● Parked for you"}
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

// TestWordWrapMoreWidthNeverAddsLines: a wider wrap, whether both widths grow
// together or only the first line's, never yields more lines, and every width
// keeps the text and stays within its limits once rest holds a wide character.
func TestWordWrapMoreWidthNeverAddsLines(t *testing.T) {
	r := rand.New(rand.NewPCG(3185, 1))
	strip := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for range 500 {
		text := randomWrapText(r)
		gap := r.IntN(40)
		previous := -1
		for rest := 0; rest <= 140; rest++ {
			first := rest - gap
			lines := wordWrap(text, first, rest)
			if previous >= 0 && len(lines) > previous {
				t.Fatalf("wordWrap(%q, %d, %d) = %d lines, one cell narrower gave %d", text, first, rest, len(lines), previous)
			}
			previous = len(lines)
			if got := strip(strings.Join(lines, "")); got != strip(text) {
				t.Fatalf("wordWrap(%q, %d, %d) lost text: %q", text, first, rest, lines)
			}
			if rest < 2 {
				continue
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
		previous = -1
		for first := -2; first <= rest; first++ {
			n := len(wordWrap(text, first, rest))
			if previous >= 0 && n > previous {
				t.Fatalf("wordWrap(%q, %d, %d) = %d lines, a first line one cell narrower gave %d", text, first, rest, n, previous)
			}
			previous = n
		}
	}
}

// TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept: relabelling the steps a
// fold keeps never costs one of them, at any width or budget.
func TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept(t *testing.T) {
	r := rand.New(rand.NewPCG(3185, 2))
	now := time.Now()
	for range 4000 {
		steps := make([]*pb.TimelineStep, 1+r.IntN(7))
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
		older, avail, budget := r.IntN(3), 10+r.IntN(120), 2+r.IntN(10)
		_, folded := fitTimelineRows(timelineStepRows(steps, timelineDayLabels(steps, now, false), titleWidth, avail), older, budget)
		rows, earlier := foldTimelineSteps(steps, older, now, titleWidth, avail, budget)
		if earlier != folded {
			t.Fatalf("avail %d, budget %d: first fold left out %d steps, relabelled fold left out %d:\n%s", avail, budget, folded, earlier, rows)
		}
	}
}
