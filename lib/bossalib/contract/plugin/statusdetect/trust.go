package statusdetect

import "regexp"

var (
	folderTrustAcceptRowRe = regexp.MustCompile(`(?m)^[ \t]*(?:❯[ \t]*)?(?:[12]\.[ \t]*)?(?:Yes, I trust this folder|Do you trust the files in this folder\?)[ \t\r]*$`)
	folderTrustExitRowRe   = regexp.MustCompile(`(?m)^[ \t]*(?:❯[ \t]*)?(?:[12]\.[ \t]*)?No, exit[ \t\r]*$`)
)

// IsFolderTrustPrompt reports whether the pane is waiting for Claude's folder
// trust decision. Both whole-line anchors must appear near the bottom of the
// pane so a quoted phrase in ordinary output or old scrollback does not match.
// Older Claude builds use the trust question in place of the explicit yes row.
func IsFolderTrustPrompt(data []byte) bool {
	tail := LastNLines(StripANSI(data), 40)
	return folderTrustAcceptRowRe.Match(tail) && folderTrustExitRowRe.Match(tail)
}
