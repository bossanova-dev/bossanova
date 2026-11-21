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
// trust modal that replaced the composer. Earlier user turns also start with
// "> ", so the grammar below reads only the live region and the line directly
// above the composer.
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
	lastRule := -1
	for i, line := range lines {
		if isRule(line) {
			lastRule = i
		}
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
	st.working = !navigation &&
		(strings.HasPrefix(st.footer, workingFooter) || spinnerAboveComposer(lines, lastRule))
	return st
}

// spinnerAboveComposer reports a braille spinner on the last line before the
// composer's top rule, where agy draws "⣯  Generating..." while a turn runs.
func spinnerAboveComposer(lines []string, lastRule int) bool {
	topRule := -1
	for i := lastRule - 1; i >= 0; i-- {
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
