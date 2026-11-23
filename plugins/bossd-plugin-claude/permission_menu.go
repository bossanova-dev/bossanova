package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Claude Code's tool-approval (permission) dialog, as `tmux capture-pane -p`
// draws it:
//
//	────────────────────────────────────────────────────────
//	 Bash command
//	 Tip: auto mode handles these prompts for you …
//	 Create test file and write text to it
//	╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
//	 │ mkdir -p /tmp/x && rm -rf /tmp/x
//	╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
//	 │ Claude Code can't be sure it read this command …
//
//	 Do you want to proceed?
//	 ❯ 1. Yes
//	   2. No
//
//	 Esc to cancel · Tab to amend
//
// A digit key selects its numbered row at once; there is no free-text row.
var (
	permFooterRe      = regexp.MustCompile(`^\s*Esc to cancel(?:\s*·.*)?\s*$`)
	permRuleRe        = regexp.MustCompile(`^\s*[─╌]{3,}\s*$`)
	permGutterRe      = regexp.MustCompile(`^\s*│(?: |$)`)
	permSelectorRowRe = regexp.MustCompile(`^\s*❯\s*(\d+)\.\s+(\S.*?)\s*$`)
)

const (
	questionKindQuestion   = "question"
	questionKindPermission = "permission"
	maxPermHeaderLines     = 12
)

// permissionMenu is a tool-approval dialog at the bottom of a pane.
type permissionMenu struct {
	title     string
	detail    string
	warning   string
	words     string
	rows      []menuRow
	truncated bool
}

// parsePermissionMenu reads a Claude Code permission dialog at the bottom of
// the pane. It returns false when the pane ends on anything else.
func parsePermissionMenu(pane string) (permissionMenu, bool) {
	lines := strings.Split(strings.TrimRight(pane, " \t\r\n"), "\n")
	footer := len(lines) - 1
	if footer < 0 || !permFooterRe.MatchString(normalizeSpaces(lines[footer])) {
		return permissionMenu{}, false
	}
	bottomRule := -1
	for i := footer - 1; i >= 0; i-- {
		if permRuleRe.MatchString(normalizeSpaces(lines[i])) {
			bottomRule = i
			break
		}
	}
	if bottomRule < 0 {
		return permissionMenu{}, false
	}

	body := lines[bottomRule+1 : footer]
	firstRow := findFirstNumberedRow(body)
	if firstRow < 0 {
		return permissionMenu{}, false
	}
	rows, hasSelector := parsePermissionRows(body[firstRow:])
	if len(rows) < 2 || !hasSelector {
		return permissionMenu{}, false
	}
	words, warning, ok := splitQuestionAndWarning(body[:firstRow])
	if !ok {
		return permissionMenu{}, false
	}

	title, detail, truncated := extractPermissionHeaderAndDetail(lines[:bottomRule])
	return permissionMenu{
		title:     title,
		detail:    detail,
		warning:   warning,
		words:     words,
		rows:      rows,
		truncated: truncated,
	}, true
}

func normalizeSpaces(s string) string {
	return strings.ReplaceAll(s, "\u00a0", " ")
}

func findFirstNumberedRow(lines []string) int {
	for i, raw := range lines {
		m := menuRowRe.FindStringSubmatch(normalizeSpaces(raw))
		if m != nil && m[1] == "1" {
			return i
		}
	}
	return -1
}

func parsePermissionRows(lines []string) ([]menuRow, bool) {
	var rows []menuRow
	hasSelector := false
	for _, raw := range lines {
		norm := normalizeSpaces(raw)
		trimmed := strings.TrimSpace(norm)
		if trimmed == "" {
			continue
		}
		if m := menuRowRe.FindStringSubmatch(norm); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n == len(rows)+1 {
				if permSelectorRowRe.MatchString(norm) {
					hasSelector = true
				}
				rows = append(rows, menuRow{first: m[2], block: m[2]})
				continue
			}
		}
		if len(rows) > 0 && !permRuleRe.MatchString(norm) {
			rows[len(rows)-1].block += " " + trimmed
		}
	}
	return rows, hasSelector
}

// splitQuestionAndWarning splits the lines between the bottom rule and row 1
// into the question (the last contiguous non-blank block, ending in "?") and
// any caution text above it.
func splitQuestionAndWarning(lines []string) (words, warning string, ok bool) {
	end := len(lines)
	for end > 0 && strings.TrimSpace(normalizeSpaces(lines[end-1])) == "" {
		end--
	}
	if end == 0 {
		return "", "", false
	}
	start := end
	for start > 0 && strings.TrimSpace(normalizeSpaces(lines[start-1])) != "" {
		start--
	}
	var qParts []string
	for _, raw := range lines[start:end] {
		qParts = append(qParts, strings.TrimSpace(normalizeSpaces(raw)))
	}
	words = strings.Join(qParts, " ")
	if !strings.HasSuffix(words, "?") {
		return "", "", false
	}
	warning = formatWarningLines(lines[:start])
	return words, warning, true
}

func formatWarningLines(lines []string) string {
	var paragraphs []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			paragraphs = append(paragraphs, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, raw := range lines {
		line := normalizeSpaces(raw)
		line = permGutterRe.ReplaceAllString(line, "")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		cur = append(cur, trimmed)
	}
	flush()
	return strings.Join(paragraphs, "\n")
}

func extractPermissionHeaderAndDetail(aboveBottomRule []string) (title, detail string, truncated bool) {
	topInnerRule := -1
	for i := len(aboveBottomRule) - 1; i >= 0; i-- {
		if permRuleRe.MatchString(normalizeSpaces(aboveBottomRule[i])) {
			topInnerRule = i
			break
		}
	}
	if topInnerRule < 0 {
		return "", formatPermissionDetail(aboveBottomRule), true
	}

	detail = formatPermissionDetail(aboveBottomRule[topInnerRule+1:])
	if topInnerRule == 0 {
		return "", detail, true
	}

	outerTopRule := -1
	floor := max(0, topInnerRule-maxPermHeaderLines)
	for i := topInnerRule - 1; i >= floor; i-- {
		if permRuleRe.MatchString(normalizeSpaces(aboveBottomRule[i])) {
			outerTopRule = i
			break
		}
	}
	var headerLines []string
	if outerTopRule >= 0 {
		headerLines = aboveBottomRule[outerTopRule+1 : topInnerRule]
	} else {
		headerLines = aboveBottomRule[floor:topInnerRule]
	}
	title = formatPermissionHeader(headerLines)
	if title == "" {
		return "", detail, true
	}
	return title, detail, false
}

func formatPermissionHeader(lines []string) string {
	var parts []string
	for _, raw := range lines {
		trimmed := strings.TrimSpace(normalizeSpaces(raw))
		if trimmed == "" || strings.HasPrefix(trimmed, "Tip:") {
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, " · ")
}

func formatPermissionDetail(lines []string) string {
	start := 0
	for start < len(lines) && strings.TrimSpace(normalizeSpaces(lines[start])) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(normalizeSpaces(lines[end-1])) == "" {
		end--
	}
	if start >= end {
		return ""
	}
	trimmed := lines[start:end]

	allGutter := true
	for _, raw := range trimmed {
		norm := normalizeSpaces(raw)
		if strings.TrimSpace(norm) == "" {
			continue
		}
		if !permGutterRe.MatchString(norm) {
			allGutter = false
			break
		}
	}
	if allGutter {
		out := make([]string, len(trimmed))
		for i, raw := range trimmed {
			out[i] = strings.TrimRight(permGutterRe.ReplaceAllString(normalizeSpaces(raw), ""), " \t\r")
		}
		return strings.Join(out, "\n")
	}

	minIndent := -1
	for _, raw := range trimmed {
		norm := normalizeSpaces(raw)
		if strings.TrimSpace(norm) == "" {
			continue
		}
		indent := len(norm) - len(strings.TrimLeft(norm, " "))
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent < 0 {
		minIndent = 0
	}
	out := make([]string, len(trimmed))
	for i, raw := range trimmed {
		norm := strings.TrimRight(normalizeSpaces(raw), " \t\r")
		if len(norm) >= minIndent {
			norm = norm[minIndent:]
		}
		out[i] = norm
	}
	return strings.Join(out, "\n")
}

func (m permissionMenu) question() *bossanovav1.ChatQuestion {
	opts := make([]string, len(m.rows))
	for i, row := range m.rows {
		opts[i] = row.block
	}
	return &bossanovav1.ChatQuestion{
		Kind:              questionKindPermission,
		Title:             m.title,
		Text:              m.words,
		Detail:            m.detail,
		Warning:           m.warning,
		IsDetailTruncated: m.truncated,
		Options:           opts,
		Id:                permissionID(m.title, m.detail, m.warning, m.words, opts, m.truncated),
		IsAnswerable:      len(opts) > 0 && len(opts) <= maxDigitMenuRow,
	}
}

// permissionID names a permission prompt by everything the user sees to decide:
// its title, command/diff detail, warning, question, truncation state and
// options, so an answer meant for one command is refused if another appears.
func permissionID(title, detail, warning, words string, options []string, truncated bool) string {
	sum := sha256.New()
	trunc := "0"
	if truncated {
		trunc = "1"
	}
	parts := append([]string{questionKindPermission, title, detail, warning, words, trunc}, options...)
	for _, part := range parts {
		sum.Write([]byte(squash(part)))
		sum.Write([]byte{0})
	}
	return "perm-" + hex.EncodeToString(sum.Sum(nil))[:16]
}

// planPermissionAnswer plans the digit key that selects an option in a
// permission dialog, refusing typed text or rows that open a terminal editor.
func planPermissionAnswer(menu permissionMenu, req *bossanovav1.PlanQuestionAnswerRequest) *bossanovav1.PlanQuestionAnswerResponse {
	question := menu.question()
	if question.GetId() != req.GetQuestionId() {
		return notPending("the agent is no longer waiting on this permission prompt")
	}
	if !question.GetIsAnswerable() {
		return &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Reason:            "answer this permission prompt in the terminal",
		}
	}
	switch answer := req.GetAnswer().(type) {
	case *bossanovav1.PlanQuestionAnswerRequest_OptionIndex:
		idx := int(answer.OptionIndex)
		if idx >= len(question.GetOptions()) {
			return &bossanovav1.PlanQuestionAnswerResponse{
				IsQuestionPending: true,
				Reason:            "the permission prompt has no such option",
			}
		}
		if rowRequiresTerminalInput(question.GetOptions()[idx]) {
			return &bossanovav1.PlanQuestionAnswerResponse{
				IsQuestionPending: true,
				Reason:            "answer this option in the terminal: it waits for typed instructions",
			}
		}
		return &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Inputs:            []*bossanovav1.PaneInput{paneKey(idx + 1)},
		}
	case *bossanovav1.PlanQuestionAnswerRequest_Text:
		return &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Reason:            "choose one of the permission options; this prompt does not take typed text",
		}
	default:
		return &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Reason:            "the answer names neither an option nor words",
		}
	}
}

// rowRequiresTerminalInput reports whether selecting a permission row opens an
// inline instruction input in the terminal (e.g. after pressing Tab to amend).
func rowRequiresTerminalInput(label string) bool {
	return strings.Contains(strings.ToLower(label), "tell claude what to do next")
}
