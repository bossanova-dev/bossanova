package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agy's ask_question menu, as `tmux capture-pane -p` draws it in the live
// status region below the last rule (the captures under
// testdata/panes/askquestion):
//
//	Question 2/2: Which effects should the button have?
//
//	> 1. [x] Shadow (depth)
//	  2. [ ] Glow (light)
//	  3. Write-in...
//
//	  ↑/↓ Navigate · ← Back · space Toggle · enter Submit All · esc Skip
//	esc to cancel
//
// A call with several questions shows one at a time, counted in its first
// line. A several-choice question draws a box on each option; a
// single-choice question the menu went back to marks its choice "✓". Enter on
// "Write-in..." opens a "Your answer:" field under the rows, with the footer
// "enter Submit · esc Back". agy's approval menu draws in the same region
// but starts with the file or command it asks about, never "Question i/N:",
// so it is not read here.
var (
	askHeaderRe = regexp.MustCompile(`^Question\s+([0-9]+)/([0-9]+):\s+(\S.*)$`)
	askRowRe    = regexp.MustCompile(`^(?:>\s*)?([0-9]+)\.\s+(\S.*)$`)
	askBoxRe    = regexp.MustCompile(`^\[(.)\]\s+`)
)

const (
	askWriteInRow     = "Write-in..."
	askAnswerLabel    = "Your answer:"
	askWriteInFooter  = "enter Submit · esc Back"
	askChosenMark     = "✓ "
	askQuestionKind   = "question"
	askStatusTailRows = 2
)

type askMenu struct {
	number, count int
	words         string
	question      *bossanovav1.ChatQuestion
}

// ReadQuestionMenu reads the ask_question menu the pane shows. See the RPC in
// plugin.proto.
func (s *Server) ReadQuestionMenu(_ context.Context, req *bossanovav1.ReadQuestionMenuRequest) (*bossanovav1.ReadQuestionMenuResponse, error) {
	if menu, ok := parseAskMenu(parsePane(req.GetPaneContent()).status); ok {
		return &bossanovav1.ReadQuestionMenuResponse{Question: menu.chatQuestion()}, nil
	}
	return &bossanovav1.ReadQuestionMenuResponse{}, nil
}

func parseAskMenu(status []string) (askMenu, bool) {
	if len(status) < askStatusTailRows+2 {
		return askMenu{}, false
	}
	m := askHeaderRe.FindStringSubmatch(strings.TrimSpace(status[0]))
	footer := strings.TrimSpace(status[len(status)-askStatusTailRows])
	isMenuFooter := strings.HasPrefix(footer, menuNavigation) || footer == askWriteInFooter
	if m == nil || !isMenuFooter {
		return askMenu{}, false
	}
	menu := askMenu{question: &bossanovav1.ChatQuestion{Kind: askQuestionKind}}
	menu.number, _ = strconv.Atoi(m[1])
	menu.count, _ = strconv.Atoi(m[2])
	words := []string{m[3]}
	body := status[1 : len(status)-askStatusTailRows]
	for i, line := range body {
		line = strings.TrimSpace(line)
		row := askRowRe.FindStringSubmatch(line)
		switch {
		case row != nil && row[1] == strconv.Itoa(len(menu.question.Options)+1):
			menu.addRow(row[2])
		case line == askAnswerLabel:
			menu.question.FreeText = joinTrimmed(body[i+1:])
			return menu.finish(words)
		case len(menu.question.Options) == 0:
			words = append(words, line)
		}
	}
	return menu.finish(words)
}

func (m *askMenu) addRow(label string) {
	q := m.question
	if label == askWriteInRow {
		q.HasFreeText = true
		return
	}
	index := uint32(len(q.Options))
	if box := askBoxRe.FindStringSubmatch(label); box != nil {
		q.IsMultiSelect = true
		label = label[len(box[0]):]
		if box[1] != " " {
			q.SelectedOptionIndexes = append(q.SelectedOptionIndexes, index)
		}
	} else if chosen, ok := strings.CutPrefix(label, askChosenMark); ok {
		label = chosen
		q.SelectedOptionIndexes = append(q.SelectedOptionIndexes, index)
	}
	q.Options = append(q.Options, label)
}

func (m askMenu) finish(words []string) (askMenu, bool) {
	m.words = strings.Join(words, " ")
	return m, len(m.question.GetOptions()) > 0 && m.number >= 1 && m.number <= m.count
}

func (m askMenu) chatQuestion() *bossanovav1.ChatQuestion {
	q := m.question
	q.Text = m.words
	q.Id = askQuestionID(m.words, q.GetOptions())
	if m.count > 1 {
		for i := 1; i <= m.count; i++ {
			q.Tabs = append(q.Tabs, &bossanovav1.ChatQuestionTab{Title: "Question " + strconv.Itoa(i), IsActive: i == m.number})
		}
	}
	return q
}

func joinTrimmed(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

func askQuestionID(words string, options []string) string {
	sum := sha256.New()
	for _, part := range append([]string{words}, options...) {
		sum.Write([]byte(strings.Join(strings.Fields(part), " ")))
		sum.Write([]byte{0})
	}
	return "menu-" + hex.EncodeToString(sum.Sum(nil))[:16]
}
