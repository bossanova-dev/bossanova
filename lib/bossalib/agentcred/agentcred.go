// Package agentcred parses and validates coding-agent account credentials
// captured by the boss account registration flows (BOS-161). It is pure
// (no I/O) so both the boss CLI and the credential materializer (epic 1.5)
// can share it.
package agentcred

import (
	"errors"
	"regexp"
)

var (
	// ErrInvalidClaudeToken is returned when a pasted/captured token does
	// not look like a Claude setup token.
	ErrInvalidClaudeToken = errors.New("agentcred: not a valid Claude setup token (expected sk-ant-… of at least 20 chars)")

	// ansiRE strips CSI sequences and OSC sequences (incl. OSC-8 hyperlinks).
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

	// claudeTokenRE matches the setup-token shape the claude CLI prints
	// (long-lived subscription OAuth token, scope user:inference). The token is
	// captured in group 1, bounded by a non-token character (or string edge) on
	// each side rather than \b: a trailing '-' is a valid token character but
	// not a word character, so a \b boundary would truncate the final '-' and
	// silently store a mangled credential that still passes ValidateClaudeToken.
	claudeTokenRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(sk-ant-[A-Za-z0-9_-]{20,})(?:[^A-Za-z0-9_-]|$)`)
	// claudeTokenExactRE is the whole-string form used for validation.
	claudeTokenExactRE = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{20,}$`)
)

// StripANSI removes ANSI escape sequences (colors, OSC-8 hyperlinks) from s.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// ParseClaudeSetupTokenOutput extracts a Claude setup token from raw CLI
// output. It returns the token and true, or ("", false) when none is found.
func ParseClaudeSetupTokenOutput(output string) (string, bool) {
	m := claudeTokenRE.FindStringSubmatch(StripANSI(output))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ValidateClaudeToken checks that token (the whole string) has the expected
// setup-token shape.
func ValidateClaudeToken(token string) error {
	if !claudeTokenExactRE.MatchString(token) {
		return ErrInvalidClaudeToken
	}
	return nil
}

// MaskToken renders a credential for display: prefix + last 4 chars.
func MaskToken(token string) string {
	if len(token) < 12 {
		return "…"
	}
	return "sk-ant-…" + token[len(token)-4:]
}
