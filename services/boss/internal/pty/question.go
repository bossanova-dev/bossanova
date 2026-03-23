package pty

import (
	"bytes"
	"regexp"
)

// cursorFwdRe matches CSI cursor-forward sequences: ESC[nC (move right n columns).
// Bubbletea uses these instead of spaces between words.
var cursorFwdRe = regexp.MustCompile(`\x1b\[([0-9]+)C`)

// cursorPosRe matches CSI cursor-position sequences: ESC[row;colH or ESC[H.
// These indicate line transitions in the TUI rendering.
var cursorPosRe = regexp.MustCompile(`\x1b\[[0-9;]*H`)

// ansiRe matches remaining ANSI escape sequences: CSI (ESC[...X), OSC (ESC]...ST),
// and two-byte sequences (ESC followed by a single character like ESC(B).
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|\(.|.)`)

// stripANSI converts raw PTY bytes to readable text by:
// 1. Replacing cursor-forward sequences (ESC[nC) with n spaces
// 2. Replacing cursor-position sequences (ESC[...H) with newlines
// 3. Normalizing \r\n and bare \r to \n
// 4. Stripping all remaining ANSI escape sequences
func stripANSI(data []byte) []byte {
	// Step 1: cursor-forward → spaces.
	out := cursorFwdRe.ReplaceAllFunc(data, func(m []byte) []byte {
		// Parse the number from ESC[nC.
		sub := cursorFwdRe.FindSubmatch(m)
		if len(sub) < 2 {
			return []byte(" ")
		}
		n := 0
		for _, c := range sub[1] {
			n = n*10 + int(c-'0')
		}
		if n <= 0 {
			n = 1
		}
		if n > 120 {
			n = 120 // cap to terminal width
		}
		return bytes.Repeat([]byte(" "), n)
	})

	// Step 2: cursor-position (ESC[...H) → newline.
	out = cursorPosRe.ReplaceAll(out, []byte("\n"))

	// Step 3: normalize line endings: \r\n → \n, bare \r → \n.
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))

	// Step 4: strip all remaining ANSI sequences.
	out = ansiRe.ReplaceAll(out, nil)

	return out
}

// selectorRe matches the bubbletea selection cursor at the start of a line.
// ❯ is U+276F (HEAVY RIGHT-POINTING ANGLE QUOTATION MARK).
var selectorRe = regexp.MustCompile(`(?m)^[^\S\n]*❯ `)

// optionRe matches indented option lines (2+ leading spaces followed by text).
var optionRe = regexp.MustCompile(`(?m)^[ ]{2,}\S`)

// hasQuestionPrompt checks whether the last portion of PTY output looks like
// a Claude Code question prompt. It detects two patterns:
//  1. AskUserQuestion/permission prompt: ❯ selector cursor + indented options
//  2. Conversational question: Claude response (⏺) ending with ?
//
// trailingQuestionRe matches a line ending with "?" (optional trailing whitespace).
var trailingQuestionRe = regexp.MustCompile(`\?[\s]*(?:\n|$)`)

func hasQuestionPrompt(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	clean := stripANSI(data)
	if len(clean) == 0 {
		return false
	}

	// Pattern 1: AskUserQuestion — ❯ selector + 2+ indented option lines.
	// Only check the last ~30 lines (enough for the question UI at screen bottom).
	tail := lastNLines(clean, 30)
	if selectorRe.Match(tail) {
		if matches := optionRe.FindAll(tail, -1); len(matches) >= 2 {
			return true
		}
	}

	// Pattern 2: Claude response ending with a question mark.
	// Find the last ⏺ marker (handles multiple response sections from tool calls)
	// and check if the text from there to the end contains a trailing "?".
	if idx := bytes.LastIndex(clean, []byte("⏺")); idx >= 0 {
		if trailingQuestionRe.Match(clean[idx:]) {
			return true
		}
	}

	return false
}

// lastNLines returns the last n lines of data as a single byte slice.
func lastNLines(data []byte, n int) []byte {
	// Walk backwards to find the start of the last n lines.
	count := 0
	i := len(data) - 1
	// Skip trailing newline.
	if i >= 0 && data[i] == '\n' {
		i--
	}
	for ; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}
