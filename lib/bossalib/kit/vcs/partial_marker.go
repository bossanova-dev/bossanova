package vcs

import (
	"regexp"
	"strings"
)

// jsWhitespace is ECMAScript's whitespace and line terminator set, used by the
// canonical marker reader's \s and trim operations.
const jsWhitespace = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
const jsSpace = `[` + jsWhitespace + `]`

var (
	partialTitle      = regexp.MustCompile(`(?i)\(partial` + jsSpace + `+\d+` + jsSpace + `*/` + jsSpace + `*\d+\)`)
	partialLine       = regexp.MustCompile(`(?i)^do not merge` + jsSpace + `*[—–-]+` + jsSpace + `*partial:` + jsSpace + `*\d+` + jsSpace + `*/` + jsSpace + `*\d+`)
	partialFence      = regexp.MustCompile(`^` + jsSpace + `{0,3}(` + "`{3,}" + `|~{3,})`)
	partialFenceClose = regexp.MustCompile(`^` + jsSpace + `*(` + "`+" + `|~+)` + jsSpace + `*$`)
	partialHeading    = regexp.MustCompile(`^` + jsSpace + `{0,3}##` + jsSpace + `+([^\r\n\x{2028}\x{2029}]+?)` + jsSpace + `*#*` + jsSpace + `*$`)
	partialTrim       = regexp.MustCompile(`^` + jsSpace + `+|` + jsSpace + `+$`)
)

// IsPartialMarked implements isPartial in skills-toolbox/epic/merge-eligibility.mjs.
// Both readers are pinned to the same golden cases; only boss-build's explicit
// title marker or the first nonblank line of one Partial section holds a PR.
func IsPartialMarked(title, body string) bool {
	if partialTitle.MatchString(title) {
		return true
	}
	var fence, current, token string
	seen, duplicate := false, false
	for _, line := range strings.Split(body, "\n") {
		if marker := partialFence.FindStringSubmatch(line); marker != nil {
			if fence == "" {
				fence = marker[1]
			} else if marker[1][0] == fence[0] && len(marker[1]) >= len(fence) && partialFenceClose.MatchString(line) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if heading := partialHeading.FindStringSubmatch(line); heading != nil {
			current = partialTrim.ReplaceAllString(heading[1], "")
			if strings.EqualFold(current, "partial") {
				if seen {
					duplicate = true
				}
				seen = true
			}
		} else if strings.EqualFold(current, "partial") && token == "" {
			token = partialTrim.ReplaceAllString(line, "")
		}
	}
	return !duplicate && partialLine.MatchString(token)
}
