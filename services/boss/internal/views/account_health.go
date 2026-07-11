package views

import (
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/agenterr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// maskedTestErrorWidth bounds a masked last-test error summary to a safe rune
// width for the detail screen. The list cell (accountLastTestCell) truncates
// further to its own narrower column, so this is the widest a masked error is
// ever rendered.
const maskedTestErrorWidth = 60

// reWhitespaceRun collapses any run of whitespace (spaces, tabs, CR/LF) to a
// single space so a multi-line daemon error renders as one tidy line.
var reWhitespaceRun = regexp.MustCompile(`\s+`)

// maskTestError turns a raw last_test_error into a display-safe, single-line,
// length-bounded summary. It is the ONLY path any last-test error text may take
// to the screen (list cell and detail screen alike): the raw daemon string can
// carry secret-shaped material (auth headers, provider API keys, env
// assignments, PEM blocks, emails), so it is first run through
// agenterr.Redact — which replaces every known secret shape with a redaction
// sentinel — then collapsed to one line and truncated. Empty in → empty out, so
// callers can distinguish "no error" from a redacted one.
func maskTestError(raw string) string {
	if raw == "" {
		return ""
	}
	s := string(agenterr.Redact([]byte(raw)))
	s = strings.TrimSpace(reWhitespaceRun.ReplaceAllString(s, " "))
	return truncateDisplay(s, maskedTestErrorWidth)
}

// accountCooldownDetail renders the cooldown line for the detail screen: a
// "cooling · resets <relative>" summary while the account is still cooling
// down, else "active". This is the detail-screen framing; the list COOLDOWN
// column uses accountCooldownCell (a bare relative time / em dash) to stay
// narrow.
func accountCooldownDetail(a *pb.Account, now time.Time) string {
	ts := a.GetCooldownUntil()
	if ts == nil || !ts.AsTime().After(now) {
		return "active"
	}
	return "cooling · resets " + relTimeFuture(ts.AsTime())
}

// accountLastUsedDetail renders the last-used line: a relative time since the
// account was last used, else an em dash when it has never been used.
func accountLastUsedDetail(a *pb.Account) string {
	if ts := a.GetLastUsedAt(); ts != nil && !ts.AsTime().IsZero() {
		return relTimeAgo(ts.AsTime())
	}
	return "—"
}

// accountLastTestedDetail renders the last-tested line for the detail screen:
// "failed · <masked error>" when the last test failed (error text always via
// maskTestError — never raw), else "ok · <relative>" when a passing test is
// recorded, else "never". CREDENTIAL SAFETY: the failed branch is the only
// place a last_test_error reaches the screen, and it always passes through
// maskTestError first.
func accountLastTestedDetail(a *pb.Account) string {
	if e := a.GetLastTestError(); e != "" {
		return "failed · " + maskTestError(e)
	}
	if ts := a.GetLastTestOkAt(); ts != nil && !ts.AsTime().IsZero() {
		return "ok · " + relTimeAgo(ts.AsTime())
	}
	return "never"
}
