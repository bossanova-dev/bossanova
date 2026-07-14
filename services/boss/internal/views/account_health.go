package views

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/agenterr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// accountUtilParts computes the shared usage-window rendering primitives used by
// BOTH the list UTIL cells (accountUtilCell) and the detail Usage rows
// (accountUsageWindowDetail): the utilization percentage string and, when known,
// a compact reset countdown. ok is false when the snapshot carries no usable
// utilization signal — nil, never probed, or an unsupported/unspecified
// rate-limit status — so every caller renders an em dash through the SAME
// honest-empty gate instead of a fabricated number. The two callers differ only
// in how they decorate the countdown, so keeping the gate + percent format here
// means a future change to either (e.g. the unsupported-status set) lands in one
// place rather than drifting across two near-identical copies.
func accountUtilParts(u *pb.UsageSnapshot, pct float64, reset *timestamppb.Timestamp, now time.Time) (pctStr, countdown string, ok bool) {
	if u == nil || u.GetFetchedAt() == nil || usageSnapshotUnsupported(u.GetStatus()) {
		return "", "", false
	}
	pctStr = fmt.Sprintf("%.0f%%", pct*100)
	if reset != nil {
		if d := reset.AsTime().Sub(now); d > 0 {
			countdown = compactFutureDuration(d)
		}
	}
	return pctStr, countdown, true
}

// accountUsageWindowDetail renders a detail-screen usage-window line: the
// utilization percentage plus a reset countdown, e.g. "93% · resets in 5d".
// Like accountUtilCell it returns an em dash when the snapshot carries no usable
// signal (nil, never probed, or an unsupported/unspecified rate-limit status), so
// a fabricated number is never shown. It shares the honest-empty gate and percent
// format with accountUtilCell via accountUtilParts, differing only in the
// countdown decoration ("· resets in <dur>" vs the list cell's "(<dur>)").
func accountUsageWindowDetail(u *pb.UsageSnapshot, pct float64, reset *timestamppb.Timestamp, now time.Time) string {
	pctStr, countdown, ok := accountUtilParts(u, pct, reset, now)
	if !ok {
		return "—"
	}
	if countdown != "" {
		return pctStr + " · resets in " + countdown
	}
	return pctStr
}

// accountUsageAgeCell renders the list AGE column: a compact relative age since
// the usage snapshot was last fetched (e.g. "4m", "2h"), the TUI counterpart of
// the CLI's AGE column (fmtUsageAge, cmd/account.go). It reuses
// compactFutureDuration (m/h/d granularity, sub-minute → "<1m"), so it is
// deliberately coarser than the CLI's fmtDurationShort, which renders seconds
// for sub-minute ages — the plan mandates reusing this formatter rather than
// adding a new time primitive. It returns an em dash when the snapshot is nil or
// never probed. The age is INDEPENDENT of the util windows' unsupported gate: a
// probe that ran but could not determine utilization still has a known fetch
// time, exactly as the CLI treats it.
func accountUsageAgeCell(u *pb.UsageSnapshot, now time.Time) string {
	if u == nil || u.GetFetchedAt() == nil {
		return "—"
	}
	return compactFutureDuration(now.Sub(u.GetFetchedAt().AsTime()))
}

// accountUsageAgeDetail renders the detail-screen "Usage age" row: a
// "fetched <relative> ago" freshness line built on relTimeAgo, or an em dash when
// the snapshot is nil / never probed. As with accountUsageAgeCell the age is
// independent of the util windows' unsupported gate.
func accountUsageAgeDetail(u *pb.UsageSnapshot) string {
	if u == nil || u.GetFetchedAt() == nil {
		return "—"
	}
	return "fetched " + relTimeAgo(u.GetFetchedAt().AsTime())
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
