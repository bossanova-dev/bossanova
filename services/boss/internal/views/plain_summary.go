package views

import (
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

var (
	markdownRule        = regexp.MustCompile(`^\s*(?:[-*_]\s*){3,}$`)
	markdownBlockMarker = regexp.MustCompile(`^\s*(?:(?:#{1,6}|>|[-*+])(?:\s+|$))+`)
	markdownNumbered    = regexp.MustCompile(`^\s*\d+[.)]\s`)
	markdownEmphasis    = strings.NewReplacer("**", "", "__", "")
)

// plainParagraph is one blank-line-separated paragraph of a markdown message
// as plain words. A list is a paragraph that opens with a bullet or a number.
type plainParagraph struct {
	text string
	list bool
}

// plainMarkdown is an agent's markdown message as one run of plain words.
func plainMarkdown(text string) string {
	paragraphs := plainParagraphs(text)
	texts := make([]string, 0, len(paragraphs))
	for _, p := range paragraphs {
		texts = append(texts, p.text)
	}
	return strings.Join(texts, " ")
}

// plainParagraphs is an agent's markdown message as plain words for a terminal
// summary. Quote markers, rules and fenced code go; headings go too unless
// they are all the message has; bullets become "•" and numbered items keep
// their numbers; bold loses its asterisks and underscores and code its
// backticks, while the text inside backticks stays as written.
func plainParagraphs(text string) []plainParagraph {
	var paragraphs []plainParagraph
	var headings, words []string
	list := false
	flush := func() {
		if len(words) > 0 {
			paragraphs = append(paragraphs, plainParagraph{plainInline(strings.Join(words, " ")), list})
		}
		words = nil
	}
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced || markdownRule.MatchString(line) {
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		marker := markdownBlockMarker.FindString(line)
		if strings.Contains(marker, "#") {
			headings = append(headings, strings.Fields(line[len(marker):])...)
			continue
		}
		bullet := strings.ContainsAny(marker, "-*+")
		if len(words) == 0 {
			list = bullet || markdownNumbered.MatchString(line)
		}
		if bullet {
			words = append(words, "•")
		}
		words = append(words, strings.Fields(line[len(marker):])...)
	}
	flush()
	if len(paragraphs) == 0 {
		words, list = headings, false
		if len(words) == 0 {
			words = strings.Fields(text)
		}
		flush()
	}
	return paragraphs
}

// plainInline drops bold's asterisks and underscores and code's backticks,
// leaving the text inside backticks as written.
func plainInline(text string) string {
	spans := strings.Split(text, "`")
	for i := range spans {
		if i%2 == 0 || i == len(spans)-1 {
			spans[i] = markdownEmphasis.Replace(spans[i])
		}
	}
	return strings.Join(spans, "")
}

// summaryLines is a markdown message as plain words wrapped at width in at
// most most lines. A message too long for them is cut after the last whole
// paragraph that fits, unless it is a list or ends in a colon and so only
// introduces what was cut; failing that, at the last sentence end that keeps
// at least half the words that fit, else at a word. A cut message ends in an
// ellipsis.
func summaryLines(text string, width, most int) []string {
	lines := wordWrap(plainMarkdown(text), width, width)
	if len(lines) <= most {
		return lines
	}
	words := strings.Fields(strings.Join(lines[:most], " "))
	end, count := -1, 0
	for _, p := range plainParagraphs(text) {
		if count += len(strings.Fields(p.text)); count > len(words) {
			break
		}
		if !p.list && !strings.HasSuffix(p.text, ":") {
			end = count - 1
		}
	}
	if end < 0 {
		if sentence := lastSentenceEnd(words); sentence >= len(words)/2 {
			end = sentence
		}
	}
	if end >= 0 {
		words = words[:end+1]
	}
	for n := len(words); n > 0; n-- {
		if cut := wordWrap(withEllipsis(strings.Join(words[:n], " ")), width, width); len(cut) <= most {
			return cut
		}
	}
	return cutLines(lines, most, width)
}

// lastSentenceEnd is the index of the last word that ends a sentence, or -1. A
// numbered list marker such as "2." ends none.
func lastSentenceEnd(words []string) int {
	for i := len(words) - 1; i >= 0; i-- {
		if endsSentence(words[i]) {
			return i
		}
	}
	return -1
}

func endsSentence(word string) bool {
	word = strings.TrimRight(word, `"'”’)]`)
	return word != "" && strings.ContainsAny(word[len(word)-1:], ".?!") && strings.Trim(word, "0123456789.") != ""
}

// cutLines keeps the first n lines, wrapped at most width wide, and ends the
// last with an ellipsis when it dropped any, shortening that line to a word
// boundary when the ellipsis would not fit.
func cutLines(lines []string, n, width int) []string {
	if len(lines) <= n {
		return lines
	}
	lines = slices.Clone(lines[:n])
	last := lines[n-1]
	if ansi.StringWidth(withEllipsis(last)) > width {
		last = ansi.Truncate(last, max(0, width-2), "")
		if i := strings.LastIndex(last, " "); i > 0 && strings.TrimSpace(last[:i]) != "" {
			last = last[:i]
		}
	}
	lines[n-1] = withEllipsis(last)
	return lines
}

// withEllipsis ends text with an ellipsis in place of any dangling separator
// or bullet, set apart from a sentence it follows.
func withEllipsis(text string) string {
	text = strings.TrimRight(text, " ,;:—-•")
	if fields := strings.Fields(text); len(fields) > 0 && endsSentence(fields[len(fields)-1]) {
		return text + " …"
	}
	return text + "…"
}
