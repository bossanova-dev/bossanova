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
	}{{"review", pb.AttentionKind_ATTENTION_KIND_REVIEW, 40}, {"review-24", pb.AttentionKind_ATTENTION_KIND_REVIEW, 24}, {"fix", pb.AttentionKind_ATTENTION_KIND_FIX, 24}, {"legacy", pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, 24}, {"rotations", pb.AttentionKind_ATTENTION_KIND_FIX, 24}} {
		t.Run(tt.name, func(t *testing.T) {
			m := detailFixture(tt.kind, tt.height)
			if tt.name == "rotations" {
				m = withRotations(m)
			}
			screen := stripANSI(renderBanner(ViewChatPicker, bannerOpts{session: m.session, width: m.width}) + "\n" + m.View().Content)
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

func TestChatPickerAttentionDetailsContracts(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 40)
	m.fullQuestion = strings.Repeat("Should I retain this behavior across all supported platforms? ", 10)
	block := stripANSI(m.neededBlock())
	if strings.Join(strings.Fields(block[strings.Index(block, "Needed from you:")+len("Needed from you:"):]), " ") != strings.TrimSpace(m.fullQuestion) {
		t.Fatal("full question truncated")
	}
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

// TestChatPickerNeededAnswerIsEmphasised: the answer under "Needed from you"
// is the one line the details screen asks the operator to act on, so every
// line of it renders bold in the primary foreground, never faint, while the
// heading stays plain.
func TestChatPickerNeededAnswerIsEmphasised(t *testing.T) {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_QUESTION, 40)
	m.fullQuestion = strings.Repeat("Should I retain this behavior across all supported platforms? ", 4)
	block := m.neededBlock()
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
	block := stripANSI(m.neededBlock())
	t.Logf("details at 80 columns:\n%s", block)
	answer, ok := strings.CutPrefix(block, "Needed from you:\n")
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
		{"short answer at 19 rows", 19, 1, operatorQuestion, true},
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

// TestWordWrapWiderFirstLineNeverAddsLines: a wider first line never yields
// more lines, every width keeps the text and stays within its limits once rest
// holds a wide character, and a word no wider than rest is never split. A
// wider rest can add a line, since a word it stops overflowing moves whole off
// the first line; foldTimelineSteps carries the fold invariant that used to
// lean on rest monotonicity.
func TestWordWrapWiderFirstLineNeverAddsLines(t *testing.T) {
	r := rand.New(rand.NewPCG(3185, 1))
	strip := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for range 500 {
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

// TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept: relabelling the steps a
// fold keeps never costs one of them, at any width or budget, wherever the
// pinned step sits.
func TestFoldTimelineStepsKeepsEveryStepTheFirstFoldKept(t *testing.T) {
	r := rand.New(rand.NewPCG(3185, 2))
	now := time.Now()
	for range 4000 {
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
		"23:09  ✗ Account rotation       auth-wedge respawn cap reached",
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
	m := withRotations(detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 24))
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
				m.chats = append(m.chats, &pb.ClaudeChat{AgentSessionId: id, Title: id + " chat", CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
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
// waiting step.
func assertTimelineFold(t *testing.T, m ChatPickerModel) bool {
	t.Helper()
	block := stripANSI(m.timelineBlock())
	if block == "" {
		return false
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
		for height := 18; height <= 60; height++ {
			m := waitAmongLaterSteps(height, later)
			if !assertTimelineFold(t, m) && height >= 21 {
				t.Fatalf("%d later steps at 80x%d: no timeline:\n%s", later, height, stripANSI(m.View().Content))
			}
		}
	}
	for _, tt := range []struct {
		name          string
		height, later int
		want          []string
	}{
		{"two later at 20 rows", 20, 2, nil},
		{"two later at 21 rows", 21, 2, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 2 later steps"}},
		{"two later at 22 rows", 22, 2, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{"two later at 23 rows", 23, 2, []string{"  … 3 earlier steps", "  10:04  ✓ Repair started", "  10:05  ● Waiting for a fix", "  10:10  ✓ Resumed 0", "  10:11  ✓ Resumed 1"}},
		{"three later at 22 rows", 22, 3, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 2 later steps", "  10:12  ✓ Resumed 2"}},
		{"six later at 40 rows", 40, 6, []string{"  … 4 earlier steps", "  10:05  ● Waiting for a fix", "  … 1 later step", "  10:11  ✓ Resumed 1", "  10:12  ✓ Resumed 2", "  10:13  ✓ Resumed 3", "  10:14  ✓ Resumed 4", "  10:15  ✓ Resumed 5"}},
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

// A random sweep over timelines, waiting positions, rotations, kinds and
// terminal sizes: whenever the timeline shows, it shows the waiting step, its
// rows keep time order, its folds account for every step, and the view stays
// within the terminal.
func TestChatPickerTimelineAlwaysShowsTheWait(t *testing.T) {
	r := rand.New(rand.NewPCG(3200, 1))
	kinds := []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_MERGE}
	shown := 0
	for range 3000 {
		height, width := 14+r.IntN(50), 40+r.IntN(111)
		m := detailFixture(kinds[r.IntN(len(kinds))], height)
		m.width = width
		day := m.session.GetAttention().GetSince().AsTime().Local()
		n := 1 + r.IntN(14)
		current := r.IntN(n + 1)
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
			m.chats = append(m.chats, &pb.ClaudeChat{AgentSessionId: id, Title: id, CreatedAt: m.chats[0].GetCreatedAt(), AgentName: "claude"})
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
	if shown < 1500 {
		t.Fatalf("the timeline showed in only %d of 3000 cases", shown)
	}
	t.Logf("the timeline showed in %d of 3000 cases", shown)
}
