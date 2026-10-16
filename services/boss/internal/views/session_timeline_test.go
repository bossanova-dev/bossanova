package views

import (
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/attention"
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

// TestChatPickerUnverifiedReviewIsNotAPark explains the REVIEW a merge-ready
// PR nothing verified gets as a review to merge, never as a verify park or an
// approval.
func TestChatPickerUnverifiedReviewIsNotAPark(t *testing.T) {
	mergeable := true
	m := NewChatPickerModel(nil, context.Background(), "s", "")
	m.width = 100
	m.height = 40
	m.session = &pb.Session{Id: "s", PrDisplayHeadSha: "2bbaede9000", Attention: attention.Compute(attention.Input{
		HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 2, ChecksDone: 2, Mergeable: &mergeable, StateSince: time.Now().Add(-time.Hour),
	})}
	m.timeline = []*pb.TimelineStep{{Title: "CI passed, 2 checks", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT}}
	if got := attentionDetailStatus(m.session); !strings.HasPrefix(got, "review · ready ") {
		t.Fatalf("detail status = %q, want a ready review", got)
	}
	if got := m.neededQuestion(); got != "Is this unverified PR safe to merge?" {
		t.Fatalf("needed question = %q", got)
	}
	block := stripANSI(m.timelineBlock())
	if !strings.Contains(block, "Waiting for your review") || !strings.Contains(block, "Not verified") {
		t.Fatalf("timeline block does not explain the unverified review:\n%s", block)
	}
	if strings.Contains(block, "Parked") || strings.Contains(strings.ToLower(m.neededQuestion()), "approve") {
		t.Fatalf("unverified review explained as a park:\n%s", block)
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
		m.timeline = append(m.timeline, &pb.TimelineStep{At: timestamppb.New(time.Date(2026, 10, 16, 23, 4+i, 0, 0, time.Local)), Title: title, State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE})
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
	if strings.Join(strings.Fields(block[strings.Index(block, "Needed from you")+len("Needed from you"):]), " ") != strings.TrimSpace(m.fullQuestion) {
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
		if stripANSI(line) == "Needed from you" {
			heading = i
		}
	}
	if heading < 0 || heading == len(lines)-1 {
		t.Fatalf("no answer under the heading:\n%s", stripANSI(block))
	}
	if lines[heading] != "Needed from you" {
		t.Fatalf("heading is styled: %q", lines[heading])
	}
	for _, line := range lines[heading+1:] {
		text := stripANSI(line)
		if want := styleEmphasis.Render(text); line != want {
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
