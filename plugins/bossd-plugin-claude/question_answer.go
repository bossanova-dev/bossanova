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
//
// A call with several questions, or a question with several choices, draws a
// tab bar instead of the ☐ title: "←  ☐ Size  ☒ Toppings  ✔ Submit  →", where
// ☐ is a question not yet answered, ☒ one answered and ✔ Submit the review
// tab. Only the shown tab's question is drawn under it. A several-choice
// question draws "[ ]" boxes that tick to "[✔]", its free-text row as
// "[ ] Type something" and an unnumbered Submit row under it; a single-choice
// tab the menu went back to marks the choice made with a trailing " ✔". The
// review tab lists the answers over "1. Submit answers" and "2. Cancel" and
// draws no footer. A plain capture marks the shown tab only by colour, so
// with several question tabs the shown one cannot be named. Claude Code
// 2.1.329 to 2.1.332 write the tool call to the transcript only once it is
// answered, so while the menu waits the pane is the only record of it.
var (
	menuFooterRe = regexp.MustCompile(`^\s*Enter to select\b.*\bEsc to cancel\b`)
	menuTitleRe  = regexp.MustCompile(`^\s*☐\s+(\S.*?)\s*$`)
	menuTabsRe   = regexp.MustCompile(`^\s*←\s+([☐☒✔]\s.*?)\s+→\s*$`)
	menuTabRe    = regexp.MustCompile(`[☐☒✔]`)
	menuRowRe    = regexp.MustCompile(`^\s*(?:❯\s*)?(\d+)\.\s+(\S.*?)\s*$`)
	menuRuleRe   = regexp.MustCompile(`^\s*─+\s*$`)
	menuBoxRe    = regexp.MustCompile(`^\[(.)\]\s+`)
)

const (
	freeTextRowLabel = "Type something."
	chatRowLabel     = "Chat about this"
	tabAnswered      = "☒"
	tabSubmit        = "✔"
	chosenMark       = " ✔"
)

// questionMenu is the question menu at the bottom of a pane: the question the
// shown tab asks, under the menu's tab bar when it draws one.
type questionMenu struct {
	title    string
	words    string
	rows     []menuRow
	tabs     []*bossanovav1.ChatQuestionTab
	isReview bool
}

// menuRow is one numbered row of the menu: its first line, and that line with
// the lines under it up to the next row (a wrapped label, then its description).
type menuRow struct {
	first string
	block string
}

// ReadQuestionMenu reads the question or permission dialog the pane's menu
// asks. See the RPC in plugin.proto.
func (s *Server) ReadQuestionMenu(_ context.Context, req *bossanovav1.ReadQuestionMenuRequest) (*bossanovav1.ReadQuestionMenuResponse, error) {
	pane := string(req.GetPaneContent())
	if menu, ok := parseQuestionMenu(pane); ok {
		return &bossanovav1.ReadQuestionMenuResponse{Question: menu.question()}, nil
	}
	if perm, ok := parsePermissionMenu(pane); ok {
		return &bossanovav1.ReadQuestionMenuResponse{Question: perm.question()}, nil
	}
	return &bossanovav1.ReadQuestionMenuResponse{}, nil
}

// PlanQuestionAnswer plans the keys that answer the question or permission
// prompt the agent waits on, after checking that prompt is the one named and
// that its menu is on screen untouched. See the RPC in plugin.proto.
func (s *Server) PlanQuestionAnswer(_ context.Context, req *bossanovav1.PlanQuestionAnswerRequest) (*bossanovav1.PlanQuestionAnswerResponse, error) {
	pane := string(req.GetPaneContent())
	menu, ok := parseQuestionMenu(pane)
	if !ok {
		if perm, ok := parsePermissionMenu(pane); ok {
			return planPermissionAnswer(perm, req), nil
		}
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

// parseQuestionMenu reads the question menu drawn at the bottom of the pane:
// the ☐-titled menu of one single-choice question, a tabbed menu's question
// tab, or its review tab. It answers false when the pane ends on anything
// else.
func parseQuestionMenu(pane string) (questionMenu, bool) {
	lines := strings.Split(strings.TrimRight(pane, " \t\r\n"), "\n")
	end := len(lines)
	hasFooter := menuFooterRe.MatchString(lines[end-1])
	if hasFooter {
		end--
	}
	menu, top := questionMenu{}, -1
	for i := end - 1; i >= 0 && top < 0; i-- {
		switch {
		case menuTabsRe.MatchString(lines[i]):
			menu.tabs, top = parseTabBar(lines[i]), i
		case !hasFooter && menuRuleRe.MatchString(lines[i]):
			return questionMenu{}, false
		case hasFooter:
			if m := menuTitleRe.FindStringSubmatch(lines[i]); m != nil {
				menu.title, top = m[1], i
			}
		}
	}
	if top < 0 {
		return questionMenu{}, false
	}
	// Only the review tab draws no footer; it must end the pane on its rows.
	if !hasFooter && !menuRowRe.MatchString(lines[end-1]) {
		return questionMenu{}, false
	}
	menu.isReview = !hasFooter
	var words []string
	for _, line := range lines[top+1 : end] {
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
	if len(menu.tabs) > 0 {
		menu.title = menu.markShownTab()
	}
	return menu, len(menu.rows) > 0
}

// parseTabBar reads the tabs of a "←  ☐ A  ☒ B  ✔ Submit  →" bar.
func parseTabBar(line string) []*bossanovav1.ChatQuestionTab {
	inner := menuTabsRe.FindStringSubmatch(line)[1]
	marks := menuTabRe.FindAllStringIndex(inner, -1)
	tabs := make([]*bossanovav1.ChatQuestionTab, 0, len(marks))
	for i, mark := range marks {
		next := len(inner)
		if i+1 < len(marks) {
			next = marks[i+1][0]
		}
		glyph := inner[mark[0]:mark[1]]
		tabs = append(tabs, &bossanovav1.ChatQuestionTab{
			Title:      strings.TrimSpace(inner[mark[1]:next]),
			IsAnswered: glyph == tabAnswered,
			IsSubmit:   glyph == tabSubmit,
		})
	}
	return tabs
}

// markShownTab marks the tab the menu shows, when the plain pane tells which,
// and returns its title for the question's id; empty when it cannot tell.
// The review tab is the one with no footer; otherwise a bar with one
// question tab shows that tab.
func (m questionMenu) markShownTab() string {
	var shown *bossanovav1.ChatQuestionTab
	questions := 0
	for _, tab := range m.tabs {
		switch {
		case tab.GetIsSubmit() && m.isReview:
			shown = tab
		case !tab.GetIsSubmit() && !m.isReview:
			questions++
			shown = tab
		}
	}
	if shown == nil || (!m.isReview && questions != 1) {
		return ""
	}
	shown.IsActive = true
	return shown.GetTitle()
}

// question is the ChatQuestion the menu asks: the option rows' labels, what
// is ticked or typed on screen, and answerable when it is one question taking
// a single choice by a digit key with its free-text row untouched.
func (m questionMenu) question() *bossanovav1.ChatQuestion {
	q := &bossanovav1.ChatQuestion{Kind: questionKindQuestion, Text: m.words, Tabs: m.tabs}
	optionRows := m.rows
	if !m.isReview {
		optionRows = m.rows[:max(len(m.rows)-2, 0)]
		if len(m.rows) >= 2 && m.rows[len(m.rows)-1].first == chatRowLabel {
			q.HasFreeText = true
			if typed, _, _ := optionLabel(m.rows[len(m.rows)-2].first); !isFreeTextPlaceholder(typed) {
				q.FreeText = typed
			}
		}
	}
	for i, row := range optionRows {
		label, isBox, isChosen := optionLabel(row.first)
		q.Options = append(q.Options, label)
		q.IsMultiSelect = q.IsMultiSelect || isBox
		if isChosen {
			q.SelectedOptionIndexes = append(q.SelectedOptionIndexes, uint32(i))
		}
	}
	q.Id = questionID(m.title, m.words, q.GetOptions())
	options := len(q.GetOptions())
	q.IsAnswerable = len(m.tabs) == 0 && !q.GetIsMultiSelect() && options > 0 && options+1 <= maxDigitMenuRow && m.shows(q.GetOptions())
	return q
}

// optionLabel is a row's label without its box or chosen mark, whether the
// row draws a box, and whether that box is ticked or the row marked chosen.
func optionLabel(first string) (label string, isBox, isChosen bool) {
	if m := menuBoxRe.FindStringSubmatch(first); m != nil {
		return first[len(m[0]):], true, m[1] != " "
	}
	if label, ok := strings.CutSuffix(first, chosenMark); ok {
		return label, false, true
	}
	return first, false, false
}

// isFreeTextPlaceholder reports whether a free-text row shows its
// placeholder: "Type something." for one choice, "Type something" in a box.
func isFreeTextPlaceholder(label string) bool {
	return label == freeTextRowLabel || label == strings.TrimSuffix(freeTextRowLabel, ".")
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
