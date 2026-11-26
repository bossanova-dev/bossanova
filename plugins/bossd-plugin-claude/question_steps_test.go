package main

import (
	"fmt"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func choices(text string, indexes ...uint32) func(*bossanovav1.PlanQuestionAnswerRequest) {
	return func(r *bossanovav1.PlanQuestionAnswerRequest) {
		r.Answer = &bossanovav1.PlanQuestionAnswerRequest_Choices{Choices: &bossanovav1.QuestionChoices{OptionIndexes: indexes, Text: text}}
	}
}

func typed(text string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Text{Text: text}}
}

func keys(names ...string) []*bossanovav1.PaneInput {
	out := make([]*bossanovav1.PaneInput, 0, len(names))
	for _, n := range names {
		out = append(out, key(n))
	}
	return out
}

func wantStep(isFinal bool, inputs ...*bossanovav1.PaneInput) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs, IsFinalStep: proto.Bool(isFinal)}
}

// moveCursor redraws pane with the cursor on the line that starts with to
// (after its indent), for a capture the recordings passed through between
// two keys but did not save.
func moveCursor(t *testing.T, pane, to string) string {
	t.Helper()
	lines := strings.Split(pane, "\n")
	moved := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(line, "❯ ") && strings.Contains(line, ". "):
			lines[i] = "  " + strings.TrimPrefix(line, "❯ ")
		case strings.HasPrefix(line, "❯    "):
			lines[i] = "     " + strings.TrimPrefix(line, "❯    ")
		}
		if !moved && strings.HasPrefix(trimmed, to) && strings.HasPrefix(line, " ") {
			lines[i] = "❯ " + strings.TrimPrefix(line, "  ")
			moved = true
		}
	}
	if !moved {
		t.Fatalf("no line starts with %q", to)
	}
	return strings.Join(lines, "\n")
}

// stepThrough plans answer against each pane in turn, as the daemon does
// after each step, and checks every step it plans.
func stepThrough(t *testing.T, questionID string, answer func(*bossanovav1.PlanQuestionAnswerRequest), panes []string, want []*bossanovav1.PlanQuestionAnswerResponse) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for i, pane := range panes {
		got := plan(t, planRequest(questionID, pane, answer))
		if !proto.Equal(got, want[i]) {
			t.Fatalf("step %d: plan = %v, want %v", i+1, got, want[i])
		}
	}
}

func TestPlanQuestionAnswer_StepsThroughSeveralChoices_Chat_AC277701(t *testing.T) {
	fruitID := readMenu(t, capturedMenu(t, "multi_untouched")).GetId()
	onFreeText := moveCursor(t, capturedMenu(t, "multi_space2"), "4. [ ] Type something")
	// The recorded keys: Space; Down, Space; Down, Down and the words; Down; Enter.
	stepThrough(t, fruitID, choices("Durian", 0, 1),
		[]string{
			capturedMenu(t, "multi_untouched"),
			capturedMenu(t, "multi_space1"),
			capturedMenu(t, "multi_space2"),
			onFreeText,
			capturedMenu(t, "multi_freetext_typed"),
			capturedMenu(t, "multi_submit_row"),
		},
		[]*bossanovav1.PlanQuestionAnswerResponse{
			wantStep(false, key("Space")),
			wantStep(false, keys("Down", "Space")...),
			wantStep(false, keys("Down", "Down")...),
			wantStep(false, typed("Durian")),
			wantStep(false, key("Down")),
			wantStep(true, key("Enter")),
		})
}

func TestPlanQuestionAnswer_SetsTheBoxesToTheAnswer(t *testing.T) {
	fruitID := readMenu(t, capturedMenu(t, "multi_untouched")).GetId()
	cases := []struct {
		name    string
		fixture string
		answer  func(*bossanovav1.PlanQuestionAnswerRequest)
		want    *bossanovav1.PlanQuestionAnswerResponse
	}{
		// multi_space2 has Apple and Banana ticked, the cursor on Banana.
		{"unticks a box the answer leaves out", "multi_space2", choices("", 1, 2), wantStep(false, keys("Up", "Space")...)},
		{"ticks the next box once the earlier ones match", "multi_space2", choices("", 0, 1, 2), wantStep(false, keys("Down", "Space")...)},
		{"boxes that match go to Submit", "multi_space2", choices("", 0, 1), wantStep(false, keys("Down", "Down", "Down")...)},
		{"one option ticks only that box", "multi_space2", option(0), wantStep(false, keys("Space")...)},
		{"words alone untick the boxes first", "multi_space1", words("Durian"), wantStep(false, key("Space"))},
		{"the answer's own words already typed go on to Submit", "multi_freetext_typed", choices("Durian", 0, 1), wantStep(false, key("Down"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if got := plan(t, planRequest(fruitID, capturedMenu(t, tc.fixture), tc.answer)); !proto.Equal(got, tc.want) {
				t.Fatalf("plan = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanQuestionAnswer_AnswersEachTabThenSubmits(t *testing.T) {
	sizeID := readMenu(t, capturedMenu(t, "two_tab1")).GetId()
	toppingsID := readMenu(t, capturedMenu(t, "two_tab2")).GetId()
	reviewID := readMenu(t, capturedMenu(t, "two_review")).GetId()
	cheese := strings.Replace(capturedMenu(t, "two_tab2"), "[ ] Cheese", "[✔] Cheese", 1)
	t.Run("a single-choice tab takes its digit, which moves to the next tab", func(t *testing.T) {
		stepThrough(t, sizeID, option(0), []string{capturedMenu(t, "two_tab1")},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(true, key("1"))})
	})
	t.Run("a tab the menu went back to takes its digit again", func(t *testing.T) {
		stepThrough(t, sizeID, option(1), []string{capturedMenu(t, "two_tab1_back")},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(true, key("2"))})
	})
	t.Run("a several-choice tab ticks, then finishes on its Submit row", func(t *testing.T) {
		stepThrough(t, toppingsID, choices("", 0),
			[]string{capturedMenu(t, "two_tab2"), cheese, moveCursor(t, cheese, "Submit")},
			[]*bossanovav1.PlanQuestionAnswerResponse{
				wantStep(false, key("Space")),
				wantStep(false, keys("Down", "Down", "Down")...),
				wantStep(true, key("Enter")),
			})
	})
	t.Run("the review tab submits by digit", func(t *testing.T) {
		stepThrough(t, reviewID, option(0), []string{capturedMenu(t, "two_review")},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(true, key("1"))})
		multiReview := readMenu(t, capturedMenu(t, "multi_review")).GetId()
		stepThrough(t, multiReview, option(0), []string{capturedMenu(t, "multi_review")},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(true, key("1"))})
	})
}

// longMenu is a single-choice menu with count options, so its free-text row
// is count+1 and the rows past 9 have no digit key.
func longMenu(count int) (string, []string) {
	var b strings.Builder
	b.WriteString(" ☐ Pick\n\nPick one\n\n")
	labels := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		cursor := "  "
		if i == 1 {
			cursor = "❯ "
		}
		label := fmt.Sprintf("Choice %d", i)
		labels = append(labels, label)
		fmt.Fprintf(&b, "%s%d. %s\n", cursor, i, label)
	}
	fmt.Fprintf(&b, "  %d. Type something.\n──────\n  %d. Chat about this\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n", count+1, count+2)
	return b.String(), labels
}

func TestPlanQuestionAnswer_ReachesRowsPastNineByCursor(t *testing.T) {
	pane, labels := longMenu(10)
	id := questionID("Pick", "Pick one", labels)
	if !readMenu(t, pane).GetIsAnswerable() {
		t.Fatal("a menu with rows past 9 should read as answerable by cursor")
	}
	downs := make([]string, 9)
	for i := range downs {
		downs[i] = "Down"
	}
	t.Run("an option past 9", func(t *testing.T) {
		stepThrough(t, id, option(9), []string{pane, moveCursor(t, pane, "10. Choice 10")},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(false, keys(downs...)...), wantStep(true, key("Enter"))})
	})
	t.Run("an option within reach of a digit", func(t *testing.T) {
		stepThrough(t, id, option(8), []string{pane}, []*bossanovav1.PlanQuestionAnswerResponse{wantStep(true, key("9"))})
	})
	t.Run("words for a free-text row past 9", func(t *testing.T) {
		onFreeText := moveCursor(t, pane, "11. Type something.")
		downs := append(downs, "Down")
		stepThrough(t, id, words("Teal"), []string{pane, onFreeText},
			[]*bossanovav1.PlanQuestionAnswerResponse{wantStep(false, keys(downs...)...), wantStep(true, typed("Teal"), key("Enter"))})
	})
}

func TestPlanQuestionAnswer_StepsRefuseWhatTheMenuCannotTake(t *testing.T) {
	fruitID := readMenu(t, capturedMenu(t, "multi_untouched")).GetId()
	reviewID := readMenu(t, capturedMenu(t, "two_review")).GetId()
	sizeID := readMenu(t, capturedMenu(t, "two_tab1")).GetId()
	noCursor := strings.Replace(capturedMenu(t, "multi_untouched"), "❯ 1. [ ] Apple", "  1. [ ] Apple", 1)
	cases := []struct {
		name       string
		questionID string
		pane       string
		answer     func(*bossanovav1.PlanQuestionAnswerRequest)
	}{
		{"no option and no words", fruitID, capturedMenu(t, "multi_untouched"), choices("")},
		{"an option past the end", fruitID, capturedMenu(t, "multi_untouched"), choices("", 3)},
		{"two lines of words", fruitID, capturedMenu(t, "multi_untouched"), choices("a\nb", 0)},
		{"blank words with an option", fruitID, capturedMenu(t, "multi_untouched"), choices("   ", 0)},
		{"someone else's words in the free-text row", fruitID, capturedMenu(t, "multi_freetext_typed"), choices("Kiwi", 0, 1)},
		{"no cursor on screen", fruitID, noCursor, choices("", 0)},
		{"words on the review tab", reviewID, capturedMenu(t, "two_review"), words("yes")},
		{"several choices for a single-choice tab", sizeID, capturedMenu(t, "two_tab1"), choices("", 0, 1)},
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

func TestPlanQuestionAnswer_PlansNothingOnceTheMenuMovesOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fruitID := readMenu(t, capturedMenu(t, "multi_untouched")).GetId()
	sizeID := readMenu(t, capturedMenu(t, "two_tab1")).GetId()
	for name, tc := range map[string]struct {
		id, fixture string
	}{
		"the several-choice tab gave way to the review tab": {fruitID, "multi_review"},
		"tab 1 gave way to tab 2":                           {sizeID, "two_tab2"},
	} {
		t.Run(name, func(t *testing.T) {
			got := plan(t, planRequest(tc.id, capturedMenu(t, tc.fixture), choices("", 0)))
			if got.GetIsQuestionPending() || len(got.GetInputs()) != 0 {
				t.Fatalf("plan = %v, want not pending with no inputs", got)
			}
		})
	}
}
