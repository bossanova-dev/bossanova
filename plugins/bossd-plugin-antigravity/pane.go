package main

import (
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/agenterr"
)

// paneState is what agy's TUI shows at the bottom of a captured pane.
//
// agy draws its composer between two full-width rules. Everything after the
// last rule is the live status region: the footer, or an approval menu or
// trust modal that replaced the composer. While a background command, a
// timer or a subagent runs, agy adds a tray between the composer and the
// footer, ruled off on both sides, one "●" line per job, and counts them in
// the footer ("1 task(s)", "1 subagent(s)"). The turn has ended but the agent
// has not: it resumes when the job finishes, so a job in the tray is work.
// Earlier user turns also start with "> ", and tool calls in the scrollback
// with "●", so the grammar below reads only the live region, the tray, and
// the line directly above the composer.
type paneState struct {
	question   bool
	trustModal bool
	working    bool
	footer     string
	status     []string
}

var (
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	menuOption   = regexp.MustCompile(`^(?:>\s*)?\d+\.\s+\S`)
	spinnerGlyph = regexp.MustCompile(`^[\x{2800}-\x{28FF}]\s+\S`)
	jobCount     = regexp.MustCompile(`(?:^|\s)[1-9]\d* (?:task|subagent)\(s\)`)
)

const (
	menuNavigation   = "↑/↓ Navigate"
	trustQuestion    = "Do you trust the contents of this project?"
	workingFooter    = "esc to cancel"
	minimumRuleWidth = 20
)

func isRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	return len([]rune(trimmed)) >= minimumRuleWidth && strings.Trim(trimmed, "─") == ""
}

func parsePane(content []byte) paneState {
	text := ansiEscape.ReplaceAllString(string(content), "")
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, " \t\r"))
		}
	}
	var rules []int
	for i, line := range lines {
		if isRule(line) {
			rules = append(rules, i)
		}
	}
	lastRule, composerBottom, tray := -1, -1, false
	if len(rules) > 0 {
		lastRule = rules[len(rules)-1]
		composerBottom, tray = trayAbove(lines, rules)
	}
	var st paneState
	st.status = lines[lastRule+1:]
	if len(st.status) > 0 {
		st.footer = strings.TrimSpace(st.status[len(st.status)-1])
	}
	var navigation, option, trust bool
	for _, line := range st.status {
		trimmed := strings.TrimSpace(line)
		navigation = navigation || strings.Contains(trimmed, menuNavigation)
		option = option || menuOption.MatchString(trimmed)
		trust = trust || strings.Contains(trimmed, trustQuestion)
	}
	st.trustModal = trust && navigation
	st.question = !trust && navigation && option
	st.working = !navigation && (strings.HasPrefix(st.footer, workingFooter) ||
		tray || jobCount.MatchString(st.footer) || spinnerAboveComposer(lines, composerBottom))
	return st
}

// trayAbove finds agy's job tray: the last two rules enclosing only "●" job
// lines, below a composer, itself between rules, that starts with "> ". It
// returns the composer's bottom rule, which is the last rule when there is no
// tray, and whether a tray with a job shows.
func trayAbove(lines []string, rules []int) (composerBottom int, tray bool) {
	last := rules[len(rules)-1]
	if len(rules) < 3 {
		return last, false
	}
	top, bottom := rules[len(rules)-3], rules[len(rules)-2]
	jobs := lines[bottom+1 : last]
	if bottom-top < 2 || !strings.HasPrefix(strings.TrimSpace(lines[top+1]), ">") || len(jobs) == 0 {
		return last, false
	}
	for _, line := range jobs {
		if !strings.HasPrefix(strings.TrimSpace(line), "●") {
			return last, false
		}
	}
	return bottom, true
}

// spinnerAboveComposer reports a braille spinner on the last line before the
// composer's top rule, where agy draws "⣯  Generating..." while a turn runs.
func spinnerAboveComposer(lines []string, composerBottom int) bool {
	topRule := -1
	for i := composerBottom - 1; i >= 0; i-- {
		if isRule(lines[i]) {
			topRule = i
			break
		}
	}
	if topRule <= 0 {
		return false
	}
	return spinnerGlyph.MatchString(strings.TrimSpace(lines[topRule-1]))
}

// usageBanner finds an agy quota banner in the live status region and parses
// its reset time when it carries one.
func usageBanner(content []byte, now time.Time) (limited bool, resetAt *time.Time) {
	for _, line := range parsePane(content).status {
		if !containsAny(line, usageMarkers) && agenterr.Classify(line, now).Kind != agenterr.KindUsageExhausted {
			continue
		}
		if at, ok := agenterr.ParseResetTime(line, now); ok {
			return true, &at
		}
		return true, nil
	}
	return false, nil
}
