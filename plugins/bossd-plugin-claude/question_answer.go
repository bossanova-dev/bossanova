package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// maxDigitMenuRow is the last menu row a single digit key selects.
const maxDigitMenuRow = 9

// Claude Code's AskUserQuestion menu, as a plain pane capture draws it:
//
//	 ☐ Colour
//	Pick a colour
//	❯ 1. Red
//	     Choose Red
//	  2. Green
//	  3. Type something.
//	──────────────
//	  4. Chat about this
//	Enter to select · ↑/↓ to navigate · Esc to cancel
//
// A digit selects its row. On an option row that answers at once; on the
// free-text row it focuses the row's input, which takes typed words and Enter.
// A call with several questions draws a "← ☐ A ☐ B ✔ Submit →" tab bar
// instead of the ☐ title, and a question with several choices draws "[ ]"
// boxes on its rows. Claude Code 2.1.329 writes the tool call to its
// transcript only once it is answered, so while the menu waits the pane is
// the only record of the question.
var (
	menuFooterRe = regexp.MustCompile(`^\s*Enter to select\b.*\bEsc to cancel\b`)
	menuTitleRe  = regexp.MustCompile(`^\s*☐\s+(\S.*?)\s*$`)
	menuTabsRe   = regexp.MustCompile(`^\s*←\s.*→\s*$`)
	menuRowRe    = regexp.MustCompile(`^\s*(?:❯\s*)?(\d+)\.\s+(\S.*?)\s*$`)
	menuRuleRe   = regexp.MustCompile(`^\s*─+\s*$`)
	menuBoxRe    = regexp.MustCompile(`^\[.\]\s`)
)

const (
	freeTextRowLabel = "Type something."
	chatRowLabel     = "Chat about this"
)

// questionMenu is the single-question menu at the bottom of a pane.
type questionMenu struct {
	title string
	words string
	rows  []menuRow
}

// menuRow is one numbered row of the menu: its first line, and that line with
// the lines under it up to the next row (a wrapped label, then its description).
type menuRow struct {
	first string
	block string
}

// ReadQuestionMenu reads the question the pane's menu asks. See the RPC in
// plugin.proto.
func (s *Server) ReadQuestionMenu(_ context.Context, req *bossanovav1.ReadQuestionMenuRequest) (*bossanovav1.ReadQuestionMenuResponse, error) {
	menu, ok := parseQuestionMenu(string(req.GetPaneContent()))
	if !ok {
		return &bossanovav1.ReadQuestionMenuResponse{}, nil
	}
	return &bossanovav1.ReadQuestionMenuResponse{Question: menu.question()}, nil
}

// PlanQuestionAnswer plans the keys that answer the question the agent waits
// on, after checking that question is the one named and that its menu is on
// screen untouched. See the RPC in plugin.proto.
func (s *Server) PlanQuestionAnswer(_ context.Context, req *bossanovav1.PlanQuestionAnswerRequest) (*bossanovav1.PlanQuestionAnswerResponse, error) {
	menu, ok := parseQuestionMenu(string(req.GetPaneContent()))
	if !ok {
		return notPending("the terminal does not show a question menu"), nil
	}
	question := menu.question()
	if question.GetId() != req.GetQuestionId() {
		recorded, err := s.recordedQuestion(req)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read transcript: %v", err)
		}
		if recorded == nil || !menu.shows(recorded.GetOptions()) {
			return notPending("the agent is no longer waiting on this question"), nil
		}
		question = recorded
	}
	if !question.GetIsAnswerable() || !menu.shows(question.GetOptions()) {
		return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Reason: "answer this question in the terminal: it has several parts or choices, or someone is typing an answer there"}, nil
	}
	inputs, reason := answerKeys(len(question.GetOptions()), req)
	return &bossanovav1.PlanQuestionAnswerResponse{IsQuestionPending: true, Inputs: inputs, Reason: reason}, nil
}

func notPending(reason string) *bossanovav1.PlanQuestionAnswerResponse {
	return &bossanovav1.PlanQuestionAnswerResponse{Reason: reason}
}

// recordedQuestion is the question the transcript ends on when it names
// req's question, for an agent that records a question before it is answered.
func (s *Server) recordedQuestion(req *bossanovav1.PlanQuestionAnswerRequest) (*bossanovav1.ChatQuestion, error) {
	path, err := transcriptPath(req.GetWorkDir(), req.GetAgentSessionId())
	if err != nil {
		return nil, nil
	}
	messages, _, exists, err := readActivity(path, req.GetWorkDir(), 0)
	if err != nil || !exists || len(messages) == 0 {
		return nil, err
	}
	last := messages[len(messages)-1]
	if last.GetKind() != "question" || last.GetQuestion().GetId() != req.GetQuestionId() {
		return nil, nil
	}
	return last.GetQuestion(), nil
}

// parseQuestionMenu reads the single-question menu drawn at the bottom of the
// pane. It answers false when the pane ends on anything else, including the
// tabbed menu of a call with several questions.
func parseQuestionMenu(pane string) (questionMenu, bool) {
	lines := strings.Split(strings.TrimRight(pane, " \t\r\n"), "\n")
	footer := len(lines) - 1
	if footer < 0 || !menuFooterRe.MatchString(lines[footer]) {
		return questionMenu{}, false
	}
	menu, top := questionMenu{}, -1
	for i := footer - 1; i >= 0; i-- {
		if menuTabsRe.MatchString(lines[i]) {
			return questionMenu{}, false
		}
		if m := menuTitleRe.FindStringSubmatch(lines[i]); m != nil {
			menu.title, top = m[1], i
			break
		}
	}
	if top < 0 {
		return questionMenu{}, false
	}
	var words []string
	for _, line := range lines[top+1 : footer] {
		line = strings.TrimSpace(line)
		if m := menuRowRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n == len(menu.rows)+1 {
				menu.rows = append(menu.rows, menuRow{first: m[2], block: m[2]})
				continue
			}
		}
		switch {
		case len(menu.rows) == 0 && line != "":
			words = append(words, line)
		case len(menu.rows) > 0 && !menuRuleRe.MatchString(line):
			menu.rows[len(menu.rows)-1].block += " " + line
		}
	}
	menu.words = strings.Join(words, " ")
	return menu, len(menu.rows) > 0
}

// question is the ChatQuestion the menu asks: the option rows' labels, and
// answerable when it takes a single choice by a digit key with its free-text
// row untouched.
func (m questionMenu) question() *bossanovav1.ChatQuestion {
	q := &bossanovav1.ChatQuestion{Text: m.words}
	options := len(m.rows) - 2
	for _, row := range m.rows[:max(options, 0)] {
		q.Options = append(q.Options, menuBoxRe.ReplaceAllString(row.first, ""))
	}
	q.Id = questionID(m.title, m.words, q.GetOptions())
	q.IsAnswerable = options > 0 && options+1 <= maxDigitMenuRow && m.shows(q.GetOptions())
	for _, row := range m.rows {
		if menuBoxRe.MatchString(row.first) {
			q.IsAnswerable = false
		}
	}
	return q
}

// shows reports whether the menu offers options, untouched: those options in
// order, then the free-text row still empty, then the chat row. A label may
// wrap, so each option row's text must begin with its label.
func (m questionMenu) shows(options []string) bool {
	if len(m.rows) != len(options)+2 {
		return false
	}
	for i, label := range options {
		if !strings.HasPrefix(squash(m.rows[i].block), squash(label)) {
			return false
		}
	}
	return m.rows[len(options)].first == freeTextRowLabel && m.rows[len(options)+1].first == chatRowLabel
}

// questionID names a question by its title, words and options, however a
// pane wrapped them, so a transcript's record and the menu on screen agree.
func questionID(title, words string, options []string) string {
	sum := sha256.New()
	for _, part := range append([]string{title, words}, options...) {
		sum.Write([]byte(squash(part)))
		sum.Write([]byte{0})
	}
	return "menu-" + hex.EncodeToString(sum.Sum(nil))[:16]
}

// squash drops whitespace, so text compares equal however the pane wrapped it.
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// answerKeys is what to press for the answer req carries, given how many
// options the menu offers; or why it cannot be given.
func answerKeys(optionCount int, req *bossanovav1.PlanQuestionAnswerRequest) ([]*bossanovav1.PaneInput, string) {
	switch answer := req.GetAnswer().(type) {
	case *bossanovav1.PlanQuestionAnswerRequest_OptionIndex:
		if int(answer.OptionIndex) >= optionCount {
			return nil, "the question has no such option"
		}
		return []*bossanovav1.PaneInput{paneKey(int(answer.OptionIndex) + 1)}, ""
	case *bossanovav1.PlanQuestionAnswerRequest_Text:
		text := answer.Text
		if strings.TrimSpace(text) == "" || strings.ContainsFunc(text, unicode.IsControl) {
			return nil, "a typed answer must be one line of words"
		}
		return []*bossanovav1.PaneInput{
			paneKey(optionCount + 1),
			{Input: &bossanovav1.PaneInput_Text{Text: text}},
			{Input: &bossanovav1.PaneInput_Key{Key: "Enter"}},
		}, ""
	default:
		return nil, "the answer names neither an option nor words"
	}
}

func paneKey(row int) *bossanovav1.PaneInput {
	return &bossanovav1.PaneInput{Input: &bossanovav1.PaneInput_Key{Key: strconv.Itoa(row)}}
}
