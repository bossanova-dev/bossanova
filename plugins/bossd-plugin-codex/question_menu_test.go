package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

func capturedCard(t *testing.T, name string) string {
	t.Helper()
	pane, err := os.ReadFile(filepath.Join("testdata", "panes", "requestuserinput", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(pane)
}

func readCard(t *testing.T, pane string) *bossanovav1.ChatQuestion {
	t.Helper()
	resp, err := newServer(nil, zerolog.Nop()).ReadQuestionMenu(context.Background(), &bossanovav1.ReadQuestionMenuRequest{PaneContent: []byte(pane)})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetQuestion()
}

func questionTabs(active int) []*bossanovav1.ChatQuestionTab {
	return []*bossanovav1.ChatQuestionTab{{Title: "Question 1", IsActive: active == 1}, {Title: "Question 2", IsActive: active == 2}}
}

func cardQuestion(words, notes string, tabs []*bossanovav1.ChatQuestionTab, options ...string) *bossanovav1.ChatQuestion {
	return &bossanovav1.ChatQuestion{
		Kind: "question", Text: words, Options: options, Id: codexQuestionID(words, options),
		IsAnswerable: notes == "", HasFreeText: true, FreeText: notes, Tabs: tabs,
	}
}

func TestReadQuestionMenu_ReadsEveryCapturedCard(t *testing.T) {
	colour := func(notes string, tabs []*bossanovav1.ChatQuestionTab) *bossanovav1.ChatQuestion {
		return cardQuestion("Which colour should the button be?", notes, tabs, "Red", "Green", "Blue")
	}
	size := func(notes string) *bossanovav1.ChatQuestion {
		return cardQuestion("Which size should the button be?", notes, questionTabs(2), "Small", "Large")
	}
	shape := func(notes string) *bossanovav1.ChatQuestion {
		return cardQuestion("Which shape should the badge be?", notes, questionTabs(1), "Circle", "Square")
	}
	cases := []struct {
		fixture string
		want    *bossanovav1.ChatQuestion
	}{
		{"single_untouched", colour("", nil)},
		{"single_down", colour("", nil)},
		{"single_notes_open", colour("", nil)},
		{"single_notes_typed", colour("make it pastel", nil)},
		{"two_q1", colour("", questionTabs(1))},
		{"two_q2", size("")},
		{"two_q1_back", colour("", questionTabs(1))},
		{"two_q2_notes_typed", size("rounded corners")},
		{"none_q1", shape("")},
		{"none_q1_on_none", shape("")},
		{"none_q1_notes_open", shape("")},
		{"none_q1_notes_typed", shape("a hexagon")},
		{"none_q2", cardQuestion("Which border should the badge have?", "", questionTabs(2), "Thin", "Thick")},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			if got := readCard(t, capturedCard(t, tc.fixture)); !proto.Equal(got, tc.want) {
				t.Fatalf("question = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadQuestionMenu_KeepsTheIdWhileSomeoneAnswersInTheTerminal(t *testing.T) {
	untouched := readCard(t, capturedCard(t, "single_untouched")).GetId()
	for _, fixture := range []string{"single_down", "single_notes_open", "single_notes_typed"} {
		if got := readCard(t, capturedCard(t, fixture)).GetId(); got != untouched {
			t.Errorf("%s: id = %q, want the untouched card's %q", fixture, got, untouched)
		}
	}
	if readCard(t, capturedCard(t, "two_q2")).GetId() == readCard(t, capturedCard(t, "two_q1")).GetId() {
		t.Error("two questions of one call read with the same id")
	}
}

func TestReadQuestionMenu_ReadsNoCardThatIsNotLive(t *testing.T) {
	card := strings.TrimRight(capturedCard(t, "single_untouched"), "\n ")
	cases := map[string]string{
		"answered":                     capturedCard(t, "single_answered"),
		"two questions answered":       capturedCard(t, "none_answered"),
		"above a finished session":     card + "\n\n• Session Complete\n\n› Ask Codex to do anything\n",
		"above a turn that resumed":    card + "\n\n• Working (3s • esc to interrupt)\n",
		"an approval menu":             "Allow command `git push --force`?\n\n  1. Yes\n  2. No\n\nPress 1-2 or esc\n",
		"a header without a live card": "  Question 1/1 (0 unanswered)\n  Pick\n\n  › 1. A\n    2. None of the above\n\n  tab to add notes | enter to submit answer | esc to interrupt\n",
		"an empty pane":                "",
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			if got := readCard(t, pane); got != nil {
				t.Fatalf("question = %v, want none", got)
			}
		})
	}
}

func optionAnswer(i uint32) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_OptionIndex{OptionIndex: i}
	}
}

func textAnswer(text string) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_Text{Text: text}
	}
}

func choicesAnswer(text string, indexes ...uint32) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_Choices{Choices: &bossanovav1.QuestionChoices{OptionIndexes: indexes, Text: text}}
	}
}

func planCard(t *testing.T, questionID, pane string, answer func(*bossanovav1.PlanQuestionAnswerRequest)) *bossanovav1.PlanQuestionAnswerResponse {
	t.Helper()
	req := &bossanovav1.PlanQuestionAnswerRequest{QuestionId: questionID, PaneContent: []byte(pane)}
	answer(req)
	resp, err := newServer(nil, zerolog.Nop()).PlanQuestionAnswer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func cardKeys(names ...string) []*bossanovav1.PaneInput {
	inputs := make([]*bossanovav1.PaneInput, 0, len(names))
	for _, n := range names {
		inputs = append(inputs, codexPaneKey(n))
	}
	return inputs
}

func cardStep(isFinal bool, inputs ...*bossanovav1.PaneInput) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs, IsFinalStep: proto.Bool(isFinal)}
}

func typedNotes(text string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Text{Text: text}}
}

func stepThroughCards(t *testing.T, answer func(*bossanovav1.PlanQuestionAnswerRequest), fixtures []string, want []*bossanovav1.PlanQuestionAnswerResponse) {
	t.Helper()
	id := readCard(t, capturedCard(t, fixtures[0])).GetId()
	for i, fixture := range fixtures {
		if got := planCard(t, id, capturedCard(t, fixture), answer); !proto.Equal(got, want[i]) {
			t.Fatalf("step %d (%s): plan = %v, want %v", i+1, fixture, got, want[i])
		}
	}
}

func TestPlanQuestionAnswer_AnOptionTakesItsDigit(t *testing.T) {
	stepThroughCards(t, optionAnswer(2), []string{"single_untouched"}, []*bossanovav1.PlanQuestionAnswerResponse{cardStep(true, codexPaneKey("3"))})
	stepThroughCards(t, optionAnswer(1), []string{"two_q1"}, []*bossanovav1.PlanQuestionAnswerResponse{cardStep(true, codexPaneKey("2"))})
	stepThroughCards(t, optionAnswer(1), []string{"none_q2"}, []*bossanovav1.PlanQuestionAnswerResponse{cardStep(true, codexPaneKey("2"))})
}

func TestPlanQuestionAnswer_WordsGoInTheNotesOfNoneOfTheAbove(t *testing.T) {
	stepThroughCards(t, textAnswer("a hexagon"),
		[]string{"none_q1", "none_q1_on_none", "none_q1_notes_open", "none_q1_notes_typed"},
		[]*bossanovav1.PlanQuestionAnswerResponse{
			cardStep(false, cardKeys("Down", "Down")...),
			cardStep(false, codexPaneKey("Tab")),
			cardStep(false, typedNotes("a hexagon")),
			cardStep(true, codexPaneKey("Enter")),
		})
}

func TestPlanQuestionAnswer_AnOptionWithWordsTakesThemAsItsNotes(t *testing.T) {
	stepThroughCards(t, choicesAnswer("make it pastel", 1),
		[]string{"single_untouched", "single_down", "single_notes_open", "single_notes_typed"},
		[]*bossanovav1.PlanQuestionAnswerResponse{
			cardStep(false, codexPaneKey("Down")),
			cardStep(false, codexPaneKey("Tab")),
			cardStep(false, typedNotes("make it pastel")),
			cardStep(true, codexPaneKey("Enter")),
		})
	stepThroughCards(t, choicesAnswer("rounded corners", 0), []string{"two_q2_notes_typed"},
		[]*bossanovav1.PlanQuestionAnswerResponse{cardStep(true, codexPaneKey("Enter"))})
}

func TestPlanQuestionAnswer_AnOptionWithTheNotesOpenMovesThenSubmits(t *testing.T) {
	stepThroughCards(t, optionAnswer(0), []string{"single_notes_open"},
		[]*bossanovav1.PlanQuestionAnswerResponse{cardStep(false, codexPaneKey("Up"))})
	stepThroughCards(t, optionAnswer(1), []string{"single_notes_open"},
		[]*bossanovav1.PlanQuestionAnswerResponse{cardStep(true, codexPaneKey("Enter"))})
}

func TestPlanQuestionAnswer_RefusesWhatTheCardCannotTake(t *testing.T) {
	colourID := readCard(t, capturedCard(t, "single_untouched")).GetId()
	cases := []struct {
		name    string
		fixture string
		answer  func(*bossanovav1.PlanQuestionAnswerRequest)
	}{
		{"notes someone else typed", "single_notes_typed", optionAnswer(0)},
		{"other words than those typed", "single_notes_typed", choicesAnswer("make it bold", 1)},
		{"an option the question lacks", "single_untouched", optionAnswer(3)},
		{"several options", "single_untouched", choicesAnswer("", 0, 1)},
		{"no option and no words", "single_untouched", choicesAnswer("")},
		{"blank words", "single_untouched", textAnswer("  ")},
		{"words over two lines", "single_untouched", textAnswer("one\ntwo")},
		{"no answer", "single_untouched", func(*bossanovav1.PlanQuestionAnswerRequest) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planCard(t, colourID, capturedCard(t, tc.fixture), tc.answer)
			if !got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
				t.Fatalf("plan = %v, want a refusal that types nothing", got)
			}
		})
	}
}

func TestPlanQuestionAnswer_RefusesACardItWasNotMeantFor(t *testing.T) {
	colourID := readCard(t, capturedCard(t, "single_untouched")).GetId()
	cases := map[string]string{
		"another question on screen": capturedCard(t, "two_q2"),
		"the card answered":          capturedCard(t, "single_answered"),
		"the card above a resumed turn": strings.TrimRight(capturedCard(t, "single_untouched"), "\n ") +
			"\n\n• Working (3s • esc to interrupt)\n",
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			got := planCard(t, colourID, pane, optionAnswer(0))
			if got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
				t.Fatalf("plan = %v, want not pending with no inputs", got)
			}
		})
	}
}
