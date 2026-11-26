package main

import (
	"slices"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// An answer is entered one step at a time, each planned from a fresh capture
// of the pane, so a menu that changed between keys is never answered blind.
// The keys are the ones the captures under testdata/panes/askuserquestion
// recorded moving between shapes:
//
//   - a single-choice row answers by its digit, which on a tab of a call
//     with several questions also moves to the next tab;
//   - its free-text row takes its digit, the words and Enter;
//   - a several-choice row ticks or unticks with Space under the cursor;
//   - Down and Up move the cursor one row, through the options, the
//     free-text row (which takes typed words while the cursor is on it) and
//     the Submit row of a several-choice menu;
//   - Enter on that Submit row finishes the tab;
//   - the review tab's rows answer by digit.
//
// menuPositions counts a menu's rows for the cursor: options from 0, then the
// free-text row, then a several-choice menu's Submit row.

// wantedAnswer is the answer a request carries, read as the boxes to leave
// ticked and the words for the free-text row.
type wantedAnswer struct {
	options []uint32
	text    string
}

// readAnswer reads req's answer for a question with optionCount options, or
// says why the answer is malformed.
func readAnswer(req *bossanovav1.PlanQuestionAnswerRequest, optionCount int) (wantedAnswer, string) {
	var want wantedAnswer
	switch answer := req.GetAnswer().(type) {
	case *bossanovav1.PlanQuestionAnswerRequest_OptionIndex:
		want = wantedAnswer{options: []uint32{answer.OptionIndex}}
	case *bossanovav1.PlanQuestionAnswerRequest_Text:
		if strings.TrimSpace(answer.Text) == "" {
			return want, "a typed answer must be one line of words"
		}
		want = wantedAnswer{text: answer.Text}
	case *bossanovav1.PlanQuestionAnswerRequest_Choices:
		want = wantedAnswer{options: answer.Choices.GetOptionIndexes(), text: answer.Choices.GetText()}
		if len(want.options) == 0 && strings.TrimSpace(want.text) == "" {
			return want, "choose at least one option or type words"
		}
	default:
		return want, "the answer names neither an option nor words"
	}
	if strings.ContainsFunc(want.text, unicode.IsControl) || (want.text != "" && strings.TrimSpace(want.text) == "") {
		return want, "a typed answer must be one line of words"
	}
	for _, i := range want.options {
		if int(i) >= optionCount {
			return want, "the question has no such option"
		}
	}
	return want, ""
}

// planMenuStep plans the next step of req's answer to the question q that
// menu shows.
func planMenuStep(menu questionMenu, q *bossanovav1.ChatQuestion, req *bossanovav1.PlanQuestionAnswerRequest) *bossanovav1.PlanQuestionAnswerResponse {
	if !menu.isPlannable(q) {
		return stepRefused("answer this question in the terminal: its menu cannot take an answer from here, or someone is typing an answer there")
	}
	want, reason := readAnswer(req, len(q.GetOptions()))
	if reason != "" {
		return stepRefused(reason)
	}
	switch {
	case menu.isReview:
		return planReviewStep(want)
	case q.GetIsMultiSelect():
		return planChoicesStep(menu, q, want)
	default:
		return planSingleStep(menu, len(q.GetOptions()), want)
	}
}

// planReviewStep answers the review tab of a call with several questions:
// its rows, "Submit answers" and "Cancel", answer by digit.
func planReviewStep(want wantedAnswer) *bossanovav1.PlanQuestionAnswerResponse {
	if len(want.options) != 1 || want.text != "" {
		return stepRefused("the review tab takes one of its options: Submit answers or Cancel")
	}
	return plannedStep(true, paneKey(int(want.options[0])+1))
}

// planSingleStep answers a single-choice question with optionCount options:
// a row in reach of a digit key at once, a row past them by moving the cursor
// onto it and then pressing Enter.
func planSingleStep(menu questionMenu, optionCount int, want wantedAnswer) *bossanovav1.PlanQuestionAnswerResponse {
	if len(want.options)+min(len(want.text), 1) != 1 {
		return stepRefused("the question takes one choice: an option or typed words")
	}
	row, words := optionCount, []*bossanovav1.PaneInput{paneText(want.text), paneNamedKey("Enter")}
	if len(want.options) == 1 {
		row, words = int(want.options[0]), nil
	}
	if row+1 <= maxDigitMenuRow {
		return plannedStep(true, append([]*bossanovav1.PaneInput{paneKey(row + 1)}, words...)...)
	}
	at := menu.cursorAt()
	if at < 0 {
		return stepRefused("answer this question in the terminal: the menu shows no cursor to move to that row")
	}
	if at != row {
		return plannedStep(false, cursorMoves(at, row)...)
	}
	if words == nil {
		words = []*bossanovav1.PaneInput{paneNamedKey("Enter")}
	}
	return plannedStep(true, words...)
}

// planChoicesStep answers a several-choice question: tick or untick the next
// box that differs from the answer, then put the words in the free-text row,
// then move to the Submit row and press Enter. Each box step moves the cursor
// and presses Space together; the next capture shows which box it changed,
// so a key that went astray is put right before anything is submitted.
func planChoicesStep(menu questionMenu, q *bossanovav1.ChatQuestion, want wantedAnswer) *bossanovav1.PlanQuestionAnswerResponse {
	typed := menu.typedWords()
	if typed != "" && squash(typed) != squash(want.text) {
		return stepRefused("answer this question in the terminal: someone is typing an answer there")
	}
	at, options := menu.cursorAt(), len(q.GetOptions())
	for i := range options {
		isTicked := slices.Contains(q.GetSelectedOptionIndexes(), uint32(i))
		if isTicked != slices.Contains(want.options, uint32(i)) {
			return plannedStep(false, append(cursorMoves(at, i), paneNamedKey("Space"))...)
		}
	}
	freeTextRow, submitRow := options, options+1
	if want.text != "" && typed == "" {
		if at != freeTextRow {
			return plannedStep(false, cursorMoves(at, freeTextRow)...)
		}
		return plannedStep(false, paneText(want.text))
	}
	if at != submitRow {
		return plannedStep(false, cursorMoves(at, submitRow)...)
	}
	return plannedStep(true, paneNamedKey("Enter"))
}

// cursorMoves are the Down or Up keys that move the cursor from row at to row.
func cursorMoves(at, row int) []*bossanovav1.PaneInput {
	key, count := "Down", row-at
	if count < 0 {
		key, count = "Up", -count
	}
	moves := make([]*bossanovav1.PaneInput, 0, count)
	for range count {
		moves = append(moves, paneNamedKey(key))
	}
	return moves
}

func plannedStep(isFinal bool, inputs ...*bossanovav1.PaneInput) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs, IsFinalStep: proto.Bool(isFinal)}
}

// stepRefused keeps the question pending and types nothing, saying why.
func stepRefused(reason string) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Reason: reason}
}

func paneNamedKey(key string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Key{Key: key}}
}

func paneText(text string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Text{Text: text}}
}
