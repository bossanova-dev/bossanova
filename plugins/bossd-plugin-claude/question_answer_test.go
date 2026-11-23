package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

const (
	answerWorkDir   = "/Users/dave/wt/answer"
	answerSessionID = "11111111-2222-4333-8444-555555555555"
)

// colourMenu is Claude Code 2.1.329's AskUserQuestion menu as `tmux
// capture-pane -p` returned it, for a single question with three options.
const colourMenu = `❯ Call the AskUserQuestion tool exactly once.
────────────────────────────────────────────────────────────────
 ☐ Colour

Pick a colour

❯ 1. Red
     Choose Red
  2. Green
     Choose Green
  3. Blue
     Choose Blue
  4. Type something.
────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

// questionCall is an assistant transcript line calling AskUserQuestion.
func questionCall(t *testing.T, id string, questions ...map[string]any) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": "2026-11-20T00:00:00.000Z",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": id, "name": "AskUserQuestion", "input": map[string]any{"questions": questions}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func question(text string, multi bool, labels ...string) map[string]any {
	opts := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		opts = append(opts, map[string]any{"label": l, "description": "Choose " + l})
	}
	return map[string]any{"question": text, "header": "Q", "multiSelect": multi, "options": opts}
}

// writeTranscript puts lines where the plugin reads the chat's transcript.
func writeTranscript(t *testing.T, lines ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := transcriptPath(answerWorkDir, answerSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func planRequest(questionID, pane string, answer func(*bossanovav1.PlanQuestionAnswerRequest)) *bossanovav1.PlanQuestionAnswerRequest {
	req := &bossanovav1.PlanQuestionAnswerRequest{
		WorkDir: answerWorkDir, AgentSessionId: answerSessionID, QuestionId: questionID, PaneContent: []byte(pane),
	}
	answer(req)
	return req
}

func option(i uint32) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_OptionIndex{OptionIndex: i}
	}
}

func words(text string) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_Text{Text: text}
	}
}

func key(k string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Key{Key: k}}
}

// colourID is the id the colour menu goes by.
var colourID = questionID("Colour", "Pick a colour", []string{"Red", "Green", "Blue"})

func plan(t *testing.T, req *bossanovav1.PlanQuestionAnswerRequest) *bossanovav1.PlanQuestionAnswerResponse {
	t.Helper()
	got, err := (&Server{}).PlanQuestionAnswer(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlanQuestionAnswer_PressesWhatTheMenuTakes(t *testing.T) {
	cases := []struct {
		name   string
		answer func(*bossanovav1.PlanQuestionAnswerRequest)
		want   []*bossanovav1.PaneInput
	}{
		{"an option is its row's digit", option(1), []*bossanovav1.PaneInput{key("2")}},
		{"words go into the free-text row, then Enter", words("Teal, please"), []*bossanovav1.PaneInput{
			key("4"), {Input: &bossanovav1.PaneInput_Text{Text: "Teal, please"}}, key("Enter"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			got := plan(t, planRequest(colourID, colourMenu, tc.answer))
			want := &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: tc.want}
			if !proto.Equal(got, want) {
				t.Fatalf("plan = %v, want %v", got, want)
			}
		})
	}
}

func TestPlanQuestionAnswer_TypesNothingIntoAQuestionItWasNotMeantFor(t *testing.T) {
	tabs := strings.Replace(colourMenu, " ☐ Colour", "←  ☐ Colour  ☐ Size  ✔ Submit  →", 1)
	cyan := strings.Replace(colourMenu, "3. Blue", "3. Cyan", 1)
	sizeID := questionID("Size", "Pick a size", []string{"S", "L"})
	cases := []struct {
		name       string
		questionID string
		pane       string
	}{
		{"the answer names another question", sizeID, colourMenu},
		{"the terminal shows the composer", colourID, "❯ \n────\n  Opus | Context: 90% remaining\n"},
		{"the menu offers other options", colourID, cyan},
		{"the terminal shows a tabbed menu", colourID, tabs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			got := plan(t, planRequest(tc.questionID, tc.pane, option(0)))
			if got.GetIsQuestionPending() || len(got.GetInputs()) != 0 {
				t.Fatalf("plan = %v, want not pending with no inputs", got)
			}
		})
	}
}

func TestPlanQuestionAnswer_RefusesAnswersTheMenuCannotTake(t *testing.T) {
	typing := strings.Replace(colourMenu, "  4. Type something.", "❯ 4. Teal", 1)
	boxes := strings.NewReplacer("1. Red", "1. [ ] Red", "2. Green", "2. [ ] Green", "3. Blue", "3. [ ] Blue").Replace(colourMenu)
	boxesID := questionID("Colour", "Pick a colour", []string{"Red", "Green", "Blue"})
	cases := []struct {
		name       string
		questionID string
		pane       string
		answer     func(*bossanovav1.PlanQuestionAnswerRequest)
	}{
		{"someone is typing into the free-text row", colourID, typing, option(0)},
		{"several choices", boxesID, boxes, option(0)},
		{"an option past the end", colourID, colourMenu, option(3)},
		{"two lines of words", colourID, colourMenu, words("teal\nor blue")},
		{"blank words", colourID, colourMenu, words("  ")},
		{"an escape byte in the words", colourID, colourMenu, words("teal\x1b[2J")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			got := plan(t, planRequest(tc.questionID, tc.pane, tc.answer))
			if !got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
				t.Fatalf("plan = %v, want pending with a reason and no inputs", got)
			}
		})
	}
}

// wrappedMenu shows a label too long for one line, so the id read from the
// pane differs from the one the transcript's record goes by.
const wrappedMenu = ` ☐ Highlight

How should the selected row be highlighted?

❯ 1. Background tint and a
     bold label
     Matches Linear
  2. Bold blue text
     Matches the TUI
  3. Type something.
──────────
  4. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel`

func TestPlanQuestionAnswer_AcceptsTheQuestionTheTranscriptEndsOn(t *testing.T) {
	highlight := map[string]any{
		"question": "How should the selected row be highlighted?", "header": "Highlight", "multiSelect": false,
		"options": []map[string]any{{"label": "Background tint and a bold label"}, {"label": "Bold blue text"}},
	}
	writeTranscript(t, questionCall(t, "toolu_highlight", highlight))
	recorded := questionID("Highlight", "How should the selected row be highlighted?", []string{"Background tint and a bold label", "Bold blue text"})
	got := plan(t, planRequest(recorded, wrappedMenu, option(0)))
	want := &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: []*bossanovav1.PaneInput{key("1")}}
	if !proto.Equal(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

func TestReadQuestionMenu_ReadsTheQuestionOnScreen(t *testing.T) {
	got, err := (&Server{}).ReadQuestionMenu(t.Context(), &bossanovav1.ReadQuestionMenuRequest{PaneContent: []byte(colourMenu)})
	if err != nil {
		t.Fatal(err)
	}
	want := &bossanovav1.ChatQuestion{Kind: questionKindQuestion, Text: "Pick a colour", Options: []string{"Red", "Green", "Blue"}, Id: colourID, IsAnswerable: true}
	if !proto.Equal(got.GetQuestion(), want) {
		t.Fatalf("question = %v, want %v", got.GetQuestion(), want)
	}
	none, err := (&Server{}).ReadQuestionMenu(t.Context(), &bossanovav1.ReadQuestionMenuRequest{PaneContent: []byte("❯ \n")})
	if err != nil || none.GetQuestion() != nil {
		t.Fatalf("composer read as question %v (err %v)", none.GetQuestion(), err)
	}
}

func TestQuestionID_TheTranscriptAndTheMenuAgree(t *testing.T) {
	input, err := json.Marshal(map[string]any{"questions": []map[string]any{{
		"question": "Pick a colour", "header": "Colour", "multiSelect": false,
		"options": []map[string]any{{"label": "Red"}, {"label": "Green"}, {"label": "Blue"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	recorded := askedQuestion(contentBlock{Name: "AskUserQuestion", ID: "toolu_colour", Input: input})
	menu, ok := parseQuestionMenu(colourMenu)
	if !ok || recorded.GetId() != menu.question().GetId() {
		t.Fatalf("transcript id %q, menu id %q: want them equal", recorded.GetId(), menu.question().GetId())
	}
}

func TestParseQuestionMenu_ReadsWrappedLabels(t *testing.T) {
	menu, ok := parseQuestionMenu(wrappedMenu)
	if !ok {
		t.Fatal("menu not read")
	}
	if !menu.shows([]string{"Background tint and a bold label", "Bold blue text"}) {
		t.Fatalf("wrapped menu rows %q do not show their labels", menu.rows)
	}
}

func TestAskedQuestion_AnswerableOnlyAsOneChoice(t *testing.T) {
	cases := []struct {
		name      string
		questions []map[string]any
		want      bool
	}{
		{"one question, one choice", []map[string]any{question("Pick", false, "A", "B")}, true},
		{"several choices", []map[string]any{question("Pick", true, "A", "B")}, false},
		{"several questions", []map[string]any{question("Pick", false, "A", "B"), question("Size", false, "S", "L")}, false},
		{"more rows than digit keys", []map[string]any{question("Pick", false, "1", "2", "3", "4", "5", "6", "7", "8", "9")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{"questions": tc.questions})
			if err != nil {
				t.Fatal(err)
			}
			q := askedQuestion(contentBlock{Name: "AskUserQuestion", ID: "toolu_x", Input: input})
			if q.GetIsAnswerable() != tc.want {
				t.Fatalf("question = %v, want is_answerable %v", q, tc.want)
			}
		})
	}
}
