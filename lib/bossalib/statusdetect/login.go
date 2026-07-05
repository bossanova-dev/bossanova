package statusdetect

import "regexp"

// pleaseRunLoginRe matches Claude Code's credential-error call to action,
// rendered on the terminal when the agent cannot proceed without
// re-authenticating, e.g.
//
//	"Invalid API key · Please run /login"
//	"OAuth token has expired · Please run /login"
//	"Login expired · Please run /login"
//
// It is anchored to a line and requires the exact "Please run /login"
// instruction (a literal slash command). Requiring the verbatim CTA — not just
// the substring "login" — is what makes this fail toward NOT flagging: the
// phrase appearing inside ordinary agent prose or a pasted code block is
// vanishingly unlikely to reproduce this exact UI-chrome instruction.
var pleaseRunLoginRe = regexp.MustCompile(`(?mi)^[^\n]*\bplease run /login\b`)

// notLoggedInLineRe matches a standalone "Not logged in" status line, optionally
// followed by a "·"-separated call to action. It is anchored to a WHOLE line
// (leading whitespace only, and either end-of-line or a "·" CTA after the
// phrase) so a mid-sentence occurrence in agent prose — e.g. "Not logged in yet,
// I'll retry" — does NOT match. Only the terminal status banner, which renders
// the phrase as the entire line, fires.
var notLoggedInLineRe = regexp.MustCompile(`(?mi)^[ \t]*not logged in[ \t]*(?:·[^\n]*)?$`)

// loginTailLines is how many trailing (stripped) lines to inspect for the
// login-required shape. The credential-error banner renders at the bottom of
// the pane where the current prompt/status lives; scanning only the tail keeps
// a stray earlier mention (e.g. scrolled-back history) from firing.
const loginTailLines = 20

// IsLoginRequired reports whether the captured pane content shows the
// login-required terminal shape — the agent is blocked awaiting re-authentication
// ("Please run /login" or a standalone "Not logged in" banner).
//
// It deliberately keys on the narrow, whole-line UI shape rather than a loose
// substring, and inspects only the last few lines, so a live long-turn or the
// string appearing in unrelated agent prose never flags. A false auth-failed is
// worse than a missed one, so this fails toward NOT flagging.
func IsLoginRequired(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	clean := StripANSI(data)
	if len(clean) == 0 {
		return false
	}
	tail := LastNLines(clean, loginTailLines)
	return pleaseRunLoginRe.Match(tail) || notLoggedInLineRe.Match(tail)
}
