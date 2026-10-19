package views

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

var (
	markdownRule        = regexp.MustCompile(`^\s*(?:[-*_]\s*){3,}$`)
	markdownBlockMarker = regexp.MustCompile(`^\s*(?:(?:#{1,6}|>|[-*+])(?:\s+|$))+`)
	markdownNumbered    = regexp.MustCompile(`^\s*\d+[.)]\s`)
	markdownFence       = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
	markdownTableRule   = regexp.MustCompile(`^\s*\|?(?:\s*:?-+:?\s*\|)+\s*:?-*:?\s*$`)
	// markdownHeadingCloser is a heading's closing hashes and any quotes or
	// brackets after them.
	markdownHeadingCloser = regexp.MustCompile(`\s+#+(["'”’)\]」』）]*)\s*$`)
	// markdownEmphasis pairs, longest delimiter first so "***x***" is one pair.
	markdownEmphasis = []*regexp.Regexp{
		regexp.MustCompile(`\*\*\*(\S(?:.*?\S)?)\*\*\*`),
		regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*`),
		regexp.MustCompile(`__(\S(?:.*?\S)?)__`),
		regexp.MustCompile(`\*([^\s*](?:[^*]*?[^\s*])?)\*`),
	}
)

// sentenceStops end a sentence: the ASCII ones before a space or the end, the
// CJK full-width ones wherever they fall, since CJK puts no space after them.
const (
	sentenceStops     = ".?!。？！"
	sentenceClosers   = `"'”’)]」』）`
	questionMarks     = "?？"
	quoteOpenerRunes  = `"“「『`
	ellipsisDangling  = " ,;:—-•"
	codeSpanCharStart = 0xE000
	// signOffMaxWidth is the widest a closing line after the ask can be, such
	// as "Let me know how you'd like to proceed."
	signOffMaxWidth = 60
)

// plainParagraph is one blank-line-separated paragraph of a markdown message
// as plain words.
type plainParagraph struct {
	text string
	kind paragraphKind
	// code is where each inline code span sits in text, in order.
	code []byteRange
}

type paragraphKind int

const (
	proseParagraph paragraphKind = iota
	// listParagraph opens with a bullet or a number.
	listParagraph
	tableParagraph
	// headingParagraph is a heading that ends in a question mark.
	headingParagraph
)

// signOff reports whether p is short running text that may close a message
// after its ask, such as "Thanks!" or "Never mind, I fixed it." It does not
// end the message for the ask, and it shows with an ask that ends just
// before it, so a line that withdraws the questions is never hidden.
func (p plainParagraph) signOff() bool {
	return p.kind == proseParagraph && ansi.StringWidth(p.text) <= signOffMaxWidth
}

// byteRange is text[from:to].
type byteRange struct{ from, to int }

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
// summary. Quote markers, rules and fenced code go, and an unclosed fence
// drops only its own line; headings go too unless they are all the message
// has or ask a question; bullets become "•", numbered items keep their numbers
// and a table's rows read as bullets of their cells; emphasis loses its
// asterisks and underscores and code its backticks, while the text inside
// backticks and a word such as __init__.py stay as written.
func plainParagraphs(text string) []plainParagraph {
	var paragraphs []plainParagraph
	var headings, words []string
	kind := proseParagraph
	flush := func() {
		if plain, code := plainInline(strings.Join(words, " ")); plain != "" {
			paragraphs = append(paragraphs, plainParagraph{plain, kind, code})
		}
		words = nil
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence := markdownFence.FindStringSubmatch(line); fence != nil {
			if end := closingFence(lines[i+1:], fence[1]); end >= 0 {
				i += end + 1
			}
			flush()
			continue
		}
		if markdownRule.MatchString(line) || markdownTableRule.MatchString(line) {
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if cells, ok := tableCells(line); ok {
			if len(words) == 0 {
				kind = tableParagraph
			}
			words = append(words, "•", strings.Join(cells, ", "))
			continue
		}
		marker := markdownBlockMarker.FindString(line)
		if strings.Contains(marker, "#") {
			heading := strings.Fields(markdownHeadingCloser.ReplaceAllString(line[len(marker):], "$1"))
			headings = append(headings, heading...)
			if len(heading) > 0 && strings.ContainsAny(lastRune(strings.TrimRight(heading[len(heading)-1], sentenceClosers)), questionMarks) {
				flush()
				words, kind = heading, headingParagraph
				flush()
			}
			continue
		}
		bullet := strings.ContainsAny(marker, "-*+")
		if len(words) == 0 {
			kind = proseParagraph
			if bullet || markdownNumbered.MatchString(line) {
				kind = listParagraph
			}
		}
		if bullet {
			words = append(words, "•")
		}
		words = append(words, strings.Fields(line[len(marker):])...)
	}
	flush()
	if len(paragraphs) == 0 {
		words, kind = headings, proseParagraph
		if len(words) == 0 {
			for _, line := range lines {
				if !markdownFence.MatchString(line) && !markdownRule.MatchString(line) {
					words = append(words, strings.Fields(line)...)
				}
			}
		}
		flush()
	}
	return paragraphs
}

// closingFence is the index in lines of the fence that closes one opened with
// open, or -1 when the message never closes it.
func closingFence(lines []string, open string) int {
	for i, line := range lines {
		if fence := markdownFence.FindStringSubmatch(line); fence != nil && fence[1][0] == open[0] && len(fence[1]) >= len(open) {
			return i
		}
	}
	return -1
}

// tableCells is a markdown table row's non-empty cells.
func tableCells(line string) ([]string, bool) {
	row := strings.TrimSpace(line)
	if !strings.HasPrefix(row, "|") {
		return nil, false
	}
	var cells []string
	for _, cell := range strings.Split(strings.Trim(row, "|"), "|") {
		if cell = strings.TrimSpace(cell); cell != "" {
			cells = append(cells, cell)
		}
	}
	return cells, true
}

// plainInline drops emphasis's asterisks and underscores and code's
// backticks, leaving the text inside backticks as written and a space where
// the backticks held one, and collapses runs of spaces. An emphasis pair
// counts only outside a word, so __init__.py and *.go stay; a stray "**"
// goes. code is where each code span sits in the text.
func plainInline(text string) (string, []byteRange) {
	spans := strings.Split(text, "`")
	var code []codeSpan
	var b strings.Builder
	for i, span := range spans {
		if i%2 == 1 && i != len(spans)-1 {
			words := strings.Join(strings.Fields(span), " ")
			if words == "" {
				if span != "" {
					b.WriteByte(' ')
				}
				continue
			}
			b.WriteRune(rune(codeSpanCharStart + len(code)))
			code = append(code, codeSpan{words, strings.HasPrefix(span, " "), strings.HasSuffix(span, " ")})
			continue
		}
		b.WriteString(span)
	}
	plain := b.String()
	for _, pair := range markdownEmphasis {
		plain = replaceEmphasis(plain, pair)
	}
	return expandCodeSpans(strings.Join(strings.Fields(strings.ReplaceAll(plain, "**", "")), " "), code)
}

// codeSpan is an inline code span's words, and whether a space stood inside
// its opening or closing backtick.
type codeSpan struct {
	words       string
	lead, trail bool
}

// expandCodeSpans puts each code span back where its placeholder rune stands,
// with a space for the padding inside its backticks unless one is there
// already, and says where each now sits. The padding stays out of the text
// until now so emphasis around the backticks still pairs.
func expandCodeSpans(text string, code []codeSpan) (string, []byteRange) {
	if len(code) == 0 {
		return text, nil
	}
	var b strings.Builder
	var at []byteRange
	pad := false
	for _, r := range text {
		if pad && r != ' ' {
			b.WriteByte(' ')
		}
		pad = false
		if i := int(r) - codeSpanCharStart; i >= 0 && i < len(code) {
			if s := b.String(); code[i].lead && s != "" && !strings.HasSuffix(s, " ") {
				b.WriteByte(' ')
			}
			at = append(at, byteRange{b.Len(), b.Len() + len(code[i].words)})
			b.WriteString(code[i].words)
			pad = code[i].trail
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), at
}

// replaceEmphasis drops the delimiters of each pair that sits outside a word:
// nothing word-like before it, and after it only the end, a space, or
// punctuation that is itself followed by the end or a space.
func replaceEmphasis(text string, pair *regexp.Regexp) string {
	var b strings.Builder
	last := 0
	for _, m := range pair.FindAllStringSubmatchIndex(text, -1) {
		if !emphasisBoundary(text[:m[0]], text[m[1]:]) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(text[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func emphasisBoundary(before, after string) bool {
	if r, _ := utf8.DecodeLastRuneInString(before); before != "" && isWordRune(r) {
		return false
	}
	rest := strings.TrimLeftFunc(after, unicode.IsPunct)
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || unicode.IsSpace(r) || (rest == after && !isWordRune(r))
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || unicode.Is(unicode.Co, r)
}

// plainSummary is a markdown message as one run of plain words, with the
// places a summary may cut it and the span of the question it asks.
type plainSummary struct {
	text string
	cuts []summaryCut
	// asks are where each sentence starts in the run of questions that ends at
	// askEnd, the message's last question mark outside a list or table. A
	// message that ends in a list asking a question asks that list instead,
	// from the colon-ended introduction just before it when it has one, and a
	// heading that asks counts only when it ends the message or introduces
	// its closing list. A short closing line does not end a message for
	// either, and askEnd takes it in when the ask ends just before it. A
	// message that asks nothing has none.
	asks   []int
	askEnd int
}

// summaryCut is a place a summary may end: text[:at] is what it shows.
type summaryCut struct {
	at int
	// sentence: text[:at] ends a sentence.
	sentence bool
	// paragraph: text[:at] ends a paragraph that can close a summary, one
	// that is neither a list, a table, a heading nor a colon-ended
	// introduction to what follows.
	paragraph bool
}

func newPlainSummary(markdown string) plainSummary {
	var s plainSummary
	var b strings.Builder
	paragraphs := plainParagraphs(markdown)
	last := len(paragraphs) - 1
	if last > 0 && paragraphs[last].signOff() {
		last--
	}
	// A heading question asks when it ends the message or introduces the
	// closing list.
	headingAsks := func(n int) bool {
		return n == last || n == last-1 && paragraphs[last].kind == listParagraph
	}
	introduction, lastEnd := -1, 0
	for n, p := range paragraphs {
		if n > 0 {
			b.WriteByte(' ')
		}
		start := b.Len()
		b.WriteString(p.text)
		ends := sentenceEnds(p.text, p.code)
		next := 0
		for i := 0; i < len(p.text); i++ {
			sentence := next < len(ends) && ends[next].at == i
			if sentence {
				next++
			}
			if sentence || (p.text[i] == ' ' && !afterBullet(p.text[:i])) {
				s.cuts = append(s.cuts, summaryCut{at: start + i, sentence: sentence})
			}
		}
		s.cuts = append(s.cuts, summaryCut{at: b.Len(), sentence: next < len(ends), paragraph: p.kind == proseParagraph && !strings.HasSuffix(p.text, ":")})
		from, asking, listAsks := start, false, false
		for _, end := range ends {
			question := end.question && (p.kind == proseParagraph || p.kind == headingParagraph && headingAsks(n))
			listAsks = listAsks || end.question
			if question {
				if !asking {
					s.asks = nil
				}
				s.asks, s.askEnd = append(s.asks, from), start+end.at
			}
			asking = question
			from = start + end.at + len(p.text[end.at:]) - len(strings.TrimLeft(p.text[end.at:], " "))
		}
		if p.kind == listParagraph && listAsks && n == last {
			s.asks, s.askEnd = []int{start}, b.Len()
			if introduction >= 0 {
				s.asks = []int{introduction, start}
			}
		}
		introduction = -1
		if p.kind == proseParagraph && strings.HasSuffix(p.text, ":") {
			introduction = from
		}
		if n == last {
			lastEnd = b.Len()
		}
	}
	s.text = b.String()
	if len(s.asks) > 0 && s.askEnd == lastEnd {
		s.askEnd = len(s.text)
	}
	return s
}

// afterBullet reports whether text ends in a bullet standing as its own word.
func afterBullet(text string) bool {
	rest, ok := strings.CutSuffix(text, "•")
	return ok && (rest == "" || strings.HasSuffix(rest, " "))
}

// sentenceEnd is where a sentence ends: text[:at] runs to its stop and any
// closing quotes or brackets after it.
type sentenceEnd struct {
	at       int
	question bool
}

// quoteOpeners pairs each closing double quotation mark with its opener.
var quoteOpeners = map[rune]rune{'"': '"', '”': '“', '」': '「', '』': '『'}

// sentenceEnds is each sentence end in text, whose code spans sit at code. A
// stop inside code ends none, nor does a numbered list marker such as "2.", an
// ASCII stop followed by anything but a space or the end, or a stop inside a
// quotation that opened partway through its sentence, as in `asked "retry?"
// and`. A "?" ends none when a space comes before it, as in "ok ? a : b", or
// it ends a URL.
func sentenceEnds(text string, code []byteRange) []sentenceEnd {
	var ends []sentenceEnd
	// wordStart is where the word at i starts, firstWord the first word rune
	// at or after start, and opened where each quote opener last stood.
	start, wordStart, firstWord := 0, 0, -1
	opened := map[rune]int{}
	// quotedMidSentence reports whether the stop that closers follow sits
	// inside a quotation opened after a word of the sentence.
	quotedMidSentence := func(closers string) bool {
		for _, r := range closers {
			if open, ok := quoteOpeners[r]; ok {
				at := opened[open]
				return at > start && firstWord >= start && firstWord < at
			}
		}
		return false
	}
	for i, r := range text {
		switch {
		case r == ' ':
			wordStart = i + 1
		case strings.ContainsRune(quoteOpenerRunes, r):
			opened[r] = i
		case firstWord < start && i >= start && isWordRune(r):
			firstWord = i
		}
		for len(code) > 0 && code[0].to <= i {
			code = code[1:]
		}
		if !strings.ContainsRune(sentenceStops, r) || (len(code) > 0 && code[0].from <= i) {
			continue
		}
		end := i + utf8.RuneLen(r)
		if next, _ := utf8.DecodeRuneInString(text[end:]); strings.ContainsRune(sentenceStops, next) {
			continue
		}
		closers := text[end : len(text)-len(strings.TrimLeft(text[end:], sentenceClosers))]
		end += len(closers)
		word := text[wordStart:i]
		switch {
		case r < utf8.RuneSelf && end < len(text) && text[end] != ' ':
		case r == '.' && strings.Trim(word, "0123456789.") == "" && closers == "":
		case r == '?' && (word == "" || strings.Contains(word, "://") || strings.HasPrefix(word, "www.")):
		case quotedMidSentence(closers):
		default:
			ends = append(ends, sentenceEnd{end, strings.ContainsRune(questionMarks, r)})
			start = end + len(text[end:]) - len(strings.TrimLeft(text[end:], " "))
		}
	}
	return ends
}

func lastRune(text string) string {
	_, size := utf8.DecodeLastRuneInString(text)
	return text[len(text)-size:]
}

// summaryLines is a markdown message as plain words wrapped at width in at
// most most lines. A message too long for them keeps its question: when the
// opening that fits leaves out the run of question sentences that ends at its
// last question mark, or the closing list it asks, the summary is as much of
// the opening as the rows above the question hold, ending in an ellipsis, then
// the question on its own rows. The question is as many of the run's last
// sentences, or of the list and its introduction, as leave the opening a row;
// a last sentence or list too long for that is cut to the rows on its own.
func summaryLines(text string, width, most int) []string {
	s := newPlainSummary(text)
	if s.text == "" {
		return nil
	}
	lines, shown := s.lines(width, most)
	if len(s.asks) == 0 || s.askEnd <= shown {
		return lines
	}
	from := s.asks[len(s.asks)-1]
	for i := len(s.asks) - 1; i >= 0 && len(wordWrap(s.text[s.asks[i]:s.askEnd], width, width)) < most; i-- {
		from = s.asks[i]
	}
	ask := s.slice(from, s.askEnd)
	asked := wordWrap(ask.text, width, width)
	if len(asked) >= most {
		lines, _ = ask.lines(width, most)
		return lines
	}
	opening := s.slice(0, from)
	if opening.text == "" {
		return asked
	}
	lines, _ = opening.lines(width, most-len(asked))
	return append(lines, asked...)
}

// slice is the summary of text[from:to], trimmed of spaces at either end.
func (s plainSummary) slice(from, to int) plainSummary {
	to = from + len(strings.TrimRight(s.text[from:to], " "))
	part := plainSummary{text: s.text[from:to]}
	for _, c := range s.cuts {
		if c.at > from && c.at <= to {
			c.at -= from
			part.cuts = append(part.cuts, c)
		}
	}
	return part
}

// lines is the summary wrapped at width in at most most lines, and how many
// bytes of text they show. Text too long for them is cut after the last
// paragraph that fits and can close a summary; failing that, at the last
// sentence end that keeps at least half the width that fits, else after the
// last word that fits. Every measure is the wrapped rows, so a path or URL
// broken across rows counts as the rows it takes. The ellipsis goes on the
// last row of text, never on a row of its own.
func (s plainSummary) lines(width, most int) ([]string, int) {
	whole := wordWrap(s.text, width, width)
	if len(whole) <= most {
		return whole, len(s.text)
	}
	rowsTo := func(at int) []string { return wordWrap(s.text[:at], width, width) }
	fits := func(c summaryCut) bool {
		rows := rowsTo(c.at)
		last := rows[len(rows)-1]
		return len(rows) <= most && strings.TrimRight(last, ellipsisDangling+sentenceStops+sentenceClosers) != "" && ansi.StringWidth(ellipsize(last, c.sentence)) <= width
	}
	best := sort.Search(len(s.cuts), func(i int) bool { return len(rowsTo(s.cuts[i].at)) > most }) - 1
	for best >= 0 && !fits(s.cuts[best]) {
		best--
	}
	if best < 0 {
		return cutLines(whole, most, width), 0
	}
	pick := func(ok func(summaryCut) bool) int {
		for i := best; i >= 0; i-- {
			if ok(s.cuts[i]) && fits(s.cuts[i]) {
				return i
			}
		}
		return -1
	}
	choice := pick(func(c summaryCut) bool { return c.paragraph })
	if choice < 0 {
		half := ansi.StringWidth(s.text[:s.cuts[best].at]) / 2
		choice = pick(func(c summaryCut) bool { return c.sentence && ansi.StringWidth(s.text[:c.at]) >= half })
	}
	if choice < 0 {
		choice = best
	}
	rows := rowsTo(s.cuts[choice].at)
	rows[len(rows)-1] = ellipsize(rows[len(rows)-1], s.cuts[choice].sentence)
	return rows, s.cuts[choice].at
}

// endsSentence reports whether word ends in a sentence stop, ignoring closing
// quotes and brackets. A numbered list marker such as "2." ends none.
func endsSentence(word string) bool {
	word = strings.TrimRight(word, sentenceClosers)
	return word != "" && strings.ContainsAny(lastRune(word), sentenceStops) && strings.Trim(word, "0123456789.") != ""
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
// or bullet, set apart from a sentence its last word ends.
func withEllipsis(text string) string {
	fields := strings.Fields(text)
	return ellipsize(text, len(fields) > 0 && endsSentence(fields[len(fields)-1]))
}

// ellipsize ends text with an ellipsis in place of any dangling separator or
// bullet, set apart by a space when text ends a sentence in a script that
// spaces its sentences.
func ellipsize(text string, sentence bool) string {
	text = strings.TrimRight(text, ellipsisDangling)
	if r, _ := utf8.DecodeLastRuneInString(text); sentence && r < utf8.RuneSelf {
		return text + " …"
	}
	return text + "…"
}
