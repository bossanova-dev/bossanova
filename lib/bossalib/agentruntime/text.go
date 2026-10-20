package agentruntime

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSummaryLen is the byte budget for transcript summaries, including Ellipsis.
const MaxSummaryLen = 80

// Ellipsis marks a summary truncated to fit MaxSummaryLen.
const Ellipsis = "…"

var xmlTagRe = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// StripXMLTags removes XML-like tags and trims surrounding whitespace.
func StripXMLTags(s string) string {
	return strings.TrimSpace(xmlTagRe.ReplaceAllString(s, ""))
}

// FirstLine returns the first line after trimming surrounding whitespace.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// TruncateSummary fits text to MaxSummaryLen without splitting UTF-8 runes.
func TruncateSummary(s string) string {
	if len(s) <= MaxSummaryLen {
		return s
	}
	// MaxSummaryLen is a byte budget and the Ellipsis marker is 3 bytes in
	// UTF-8, so the reservation is unchanged. Back the cut off to a rune
	// boundary first: a fixed byte offset can land inside a multi-byte rune
	// and emit invalid UTF-8.
	cut := MaxSummaryLen - len(Ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + Ellipsis
}

// NonEmptyFile reports whether path names a nonempty file rather than a directory.
// Stat errors are treated as absent files.
func NonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Size() > 0
}
