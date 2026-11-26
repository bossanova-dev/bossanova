package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Codex's request_user_input card, as `tmux capture-pane -p` draws it (the
// captures under testdata/panes/requestuserinput):
//
//	Question 1/2 (2 unanswered)
//	Which colour should the button be?
//
//	› 1. Red                warm
//	  2. Green              calm
//	  3. None of the above  Optionally, add details in notes (tab)
//
//	› make it pastel
//
//	tab or esc to clear notes | enter to submit answer
//
// The card replaces the composer, so a live card's footer is the pane's last
// line. "›" marks the row the cursor is on, which is also the choice Enter
// submits. Tab opens a notes field under the rows, drawn "› Add notes" while
// empty; while it is open the footer reads "tab or esc to clear notes", and
// Tab or Esc clears it. Codex appends "None of the above" to every question's
// options. A call with several questions shows one at a time, counted in the
// header; on the last one the footer says "submit all". The card draws no
// mark on a choice already made, so a question the card went back to reads
// with nothing selected.
var (
	codexCardHeaderRe = regexp.MustCompile(`^` + codexBoxBorderClass + `*\bQuestion\s+([0-9]+)/([0-9]+)\s+\(` + codexQuestionCardLiveCount + `\s+unanswered\)\s*$`)
	codexCardFooterRe = regexp.MustCompile(`^\s*(tab to add notes|tab or esc to clear notes)\s+\|\s+enter to submit (?:answer|all)\b`)
	codexCardRowRe    = regexp.MustCompile(`^\s*(›\s+)?([0-9]+)\.\s+(\S.*?)\s*$`)
	codexCardColumnRe = regexp.MustCompile(`\s{2,}`)
)

const (
	codexNoneRowLabel      = "None of the above"
	codexNotesPlaceholder  = "Add notes"
	codexNotesOpenFooter   = "tab or esc to clear notes"
	codexQuestionKind      = "question"
	codexMaxDigitRow       = 9
	codexUnknownCursorMark = -1
)

type codexCard struct {
	number, count int
	words         string
	// labels are every row's label, "None of the above" included.
	labels []string
	// cursor is the index into labels of the row "›" marks, or -1.
	cursor      int
	isNotesOpen bool
	// notes is what the open notes field holds, however the pane wrapped it;
	// empty while it shows its placeholder.
	notes string
}

// ReadQuestionMenu reads the request_user_input card the pane ends on. See
// the RPC in plugin.proto.
func (s *Server) ReadQuestionMenu(_ context.Context, req *bossanovav1.ReadQuestionMenuRequest) (*bossanovav1.ReadQuestionMenuResponse, error) {
	if card, ok := parseCodexCard(req.GetPaneContent()); ok {
		return &bossanovav1.ReadQuestionMenuResponse{Question: card.question()}, nil
	}
	return &bossanovav1.ReadQuestionMenuResponse{}, nil
}

func parseCodexCard(pane []byte) (codexCard, bool) {
	active := codexUnicodeSpace.ReplaceAll(codexQuestionActivePane(pane), []byte(" "))
	lines := strings.Split(strings.TrimRight(string(active), " \t\r\n"), "\n")
	footer := codexCardFooterRe.FindStringSubmatch(lines[len(lines)-1])
	if footer == nil {
		return codexCard{}, false
	}
	for top := len(lines) - 2; top >= 0; top-- {
		if m := codexCardHeaderRe.FindStringSubmatch(lines[top]); m != nil {
			card := codexCard{cursor: codexUnknownCursorMark, isNotesOpen: footer[1] == codexNotesOpenFooter}
			card.number, _ = strconv.Atoi(m[1])
			card.count, _ = strconv.Atoi(m[2])
			return card, card.readBody(lines[top+1:len(lines)-1]) && card.number <= card.count
		}
	}
	return codexCard{}, false
}

func (c *codexCard) readBody(lines []string) bool {
	var words, notes []string
	stage, sawBlank := 0, false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		row := codexCardRowRe.FindStringSubmatch(line)
		switch {
		case stage == 0 && row != nil && row[2] == "1" && sawBlank && len(words) > 0:
			stage = 1
			c.addRow(row)
		case stage == 0 && line != "":
			words = append(words, line)
		case stage == 1 && row != nil && row[2] == strconv.Itoa(len(c.labels)+1):
			c.addRow(row)
		case stage == 1 && line == "":
			stage = 2
		case stage == 2 && line != "":
			notes = append(notes, line)
		}
		sawBlank = line == ""
	}
	c.words = strings.Join(words, " ")
	if c.isNotesOpen {
		c.notes = strings.TrimSpace(strings.TrimPrefix(strings.Join(notes, " "), "›"))
		if c.notes == codexNotesPlaceholder {
			c.notes = ""
		}
	}
	return len(c.labels) > 0 && (len(notes) == 0 || c.isNotesOpen)
}

func (c *codexCard) addRow(row []string) {
	if row[1] != "" {
		c.cursor = len(c.labels)
	}
	c.labels = append(c.labels, codexCardColumnRe.Split(row[3], 2)[0])
}

func (c codexCard) options() []string {
	if c.hasNoneRow() {
		return c.labels[:len(c.labels)-1]
	}
	return c.labels
}

func (c codexCard) hasNoneRow() bool {
	return len(c.labels) > 1 && c.labels[len(c.labels)-1] == codexNoneRowLabel
}

func (c codexCard) question() *bossanovav1.ChatQuestion {
	q := &bossanovav1.ChatQuestion{
		Kind: codexQuestionKind, Text: c.words, Options: c.options(),
		Id: codexQuestionID(c.words, c.options()), HasFreeText: c.hasNoneRow(), FreeText: c.notes,
	}
	if c.count > 1 {
		for i := 1; i <= c.count; i++ {
			q.Tabs = append(q.Tabs, &bossanovav1.ChatQuestionTab{Title: "Question " + strconv.Itoa(i), IsActive: i == c.number})
		}
	}
	q.IsAnswerable = c.notes == "" && len(c.labels) <= codexMaxDigitRow
	return q
}

func codexQuestionID(words string, options []string) string {
	sum := sha256.New()
	for _, part := range append([]string{words}, options...) {
		sum.Write([]byte(codexSquash(part)))
		sum.Write([]byte{0})
	}
	return "menu-" + hex.EncodeToString(sum.Sum(nil))[:16]
}

func codexSquash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// The keys are the ones the captures recorded moving between shapes:
//
//   - a digit picks its row and submits it at once: the next question of a
//     call with several, or the whole call on its last question;
//   - Down and Up move the cursor, with the notes field open or closed;
//   - Tab opens the notes field for the row under the cursor, which then
//     takes typed words;
//   - Enter submits the row under the cursor with the notes typed.

type codexAnswer struct {
	row   int
	notes string
}

// PlanQuestionAnswer plans the next keys of the answer to the card the agent
// waits on, after checking the card on screen is the question named. See the
// RPC in plugin.proto.
func (s *Server) PlanQuestionAnswer(_ context.Context, req *bossanovav1.PlanQuestionAnswerRequest) (*bossanovav1.PlanQuestionAnswerResponse, error) {
	card, ok := parseCodexCard(req.GetPaneContent())
	if !ok {
		return codexNotPending("the terminal does not show a question card"), nil
	}
	if card.question().GetId() != req.GetQuestionId() {
		return codexNotPending("the agent is no longer waiting on this question"), nil
	}
	want, reason := card.readAnswer(req)
	if reason != "" {
		return codexStepRefused(reason), nil
	}
	return card.planStep(want), nil
}

func (c codexCard) readAnswer(req *bossanovav1.PlanQuestionAnswerRequest) (codexAnswer, string) {
	var indexes []uint32
	var notes string
	switch answer := req.GetAnswer().(type) {
	case *bossanovav1.PlanQuestionAnswerRequest_OptionIndex:
		indexes = []uint32{answer.OptionIndex}
	case *bossanovav1.PlanQuestionAnswerRequest_Text:
		notes = answer.Text
		if strings.TrimSpace(notes) == "" {
			return codexAnswer{}, "a typed answer must be one line of words"
		}
	case *bossanovav1.PlanQuestionAnswerRequest_Choices:
		indexes, notes = answer.Choices.GetOptionIndexes(), answer.Choices.GetText()
	default:
		return codexAnswer{}, "the answer names neither an option nor words"
	}
	switch {
	case strings.ContainsFunc(notes, unicode.IsControl) || (notes != "" && strings.TrimSpace(notes) == ""):
		return codexAnswer{}, "a typed answer must be one line of words"
	case len(indexes) > 1:
		return codexAnswer{}, "the question takes one choice"
	case len(indexes) == 1 && int(indexes[0]) >= len(c.options()):
		return codexAnswer{}, "the question has no such option"
	case len(indexes) == 1:
		return codexAnswer{row: int(indexes[0]), notes: notes}, ""
	case notes == "":
		return codexAnswer{}, "choose an option or type words"
	case !c.hasNoneRow():
		return codexAnswer{}, "the question offers no row for typed words"
	}
	return codexAnswer{row: len(c.labels) - 1, notes: notes}, ""
}

func (c codexCard) planStep(want codexAnswer) *bossanovav1.PlanQuestionAnswerResponse {
	if c.notes != "" && codexSquash(c.notes) != codexSquash(want.notes) {
		return codexStepRefused("answer this question in the terminal: someone is typing notes there")
	}
	if want.notes == "" && !c.isNotesOpen && want.row+1 <= codexMaxDigitRow {
		return codexPlannedStep(true, codexPaneKey(strconv.Itoa(want.row+1)))
	}
	if c.cursor < 0 {
		return codexStepRefused("answer this question in the terminal: the card shows no cursor")
	}
	switch {
	case c.cursor != want.row:
		return codexPlannedStep(false, codexCursorMoves(c.cursor, want.row)...)
	case want.notes != "" && !c.isNotesOpen:
		return codexPlannedStep(false, codexPaneKey("Tab"))
	case want.notes != "" && c.notes == "":
		return codexPlannedStep(false, &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Text{Text: want.notes}})
	}
	return codexPlannedStep(true, codexPaneKey("Enter"))
}

func codexCursorMoves(at, row int) []*bossanovav1.PaneInput {
	key, count := "Down", row-at
	if count < 0 {
		key, count = "Up", -count
	}
	moves := make([]*bossanovav1.PaneInput, 0, count)
	for range count {
		moves = append(moves, codexPaneKey(key))
	}
	return moves
}

func codexPlannedStep(isFinal bool, inputs ...*bossanovav1.PaneInput) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs, IsFinalStep: &isFinal}
}

func codexStepRefused(reason string) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Reason: reason}
}

func codexNotPending(reason string) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{Reason: reason}
}

func codexPaneKey(key string) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Key{Key: key}}
}
