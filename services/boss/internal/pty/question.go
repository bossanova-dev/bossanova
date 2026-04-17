package pty

import (
	"github.com/recurser/bossalib/statusdetect"
)

// hasQuestionPrompt delegates to the shared statusdetect library.
func hasQuestionPrompt(data []byte) bool {
	return statusdetect.HasQuestionPrompt(data)
}

// stripANSI delegates to the shared statusdetect library.
func stripANSI(data []byte) []byte {
	return statusdetect.StripANSI(data)
}

// lastNLines delegates to the shared statusdetect library.
func lastNLines(data []byte, n int) []byte {
	return statusdetect.LastNLines(data, n)
}
