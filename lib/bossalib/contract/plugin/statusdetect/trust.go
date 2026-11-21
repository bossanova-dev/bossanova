package statusdetect

import (
	"bytes"
	"regexp"
)

var (
	folderTrustAcceptRowRe = regexp.MustCompile(`(?m)^[ \t]*(?:[❯>][ \t]*)?(?:[12]\.[ \t]*)?(?:Yes, I trust this folder|Do you trust the files in this folder\?)[ \t\r]*$`)
	folderTrustExitRowRe   = regexp.MustCompile(`(?m)^[ \t]*(?:[❯>][ \t]*)?(?:[12]\.[ \t]*)?No, exit[ \t\r]*$`)
)

// IsFolderTrustPrompt reports whether the pane is waiting for Claude's or
// Antigravity's folder trust decision (Antigravity marks the selection with
// ">"). Both whole-line anchors must appear near the bottom of the pane, after
// trailing blank rows, so a quoted phrase in ordinary output or old scrollback
// does not match. Older Claude builds use the trust question in place of the
// explicit yes row.
func IsFolderTrustPrompt(data []byte) bool {
	tail := LastNLines(bytes.TrimRight(StripANSI(data), " \t\r\n"), 40)
	return folderTrustAcceptRowRe.Match(tail) && folderTrustExitRowRe.Match(tail)
}
