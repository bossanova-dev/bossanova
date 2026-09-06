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

// --- Credential-check state (BOS-1142) ---
//
// The daemon owns credential verification and records its verdict durably on
// the account row (Account.auth_check). These helpers are the only path that
// verdict takes to a screen.

// Auth-check outcome tokens. They mirror models.AuthCheckOutcome in
// lib/bossalib/models/account.go, which this module cannot import across the
// module boundary; the daemon projects them onto Account.auth_check verbatim
// (services/bossd/internal/server/convert.go).
const (
	authCheckNever       = ""
	authCheckHealthy     = "healthy"
	authCheckAuthInvalid = "auth_invalid"
	authCheckTransient   = "transient"
	authCheckUnavailable = "unavailable"
	// authCheckRefreshChainUnproven (BOS-1174): the verification ran cleanly,
	// but on a credential whose access token says a refresh should already have
	// happened and which produced no credential write. A warning about an
	// unobserved refresh chain — NOT a provider rejection, and the account
	// stays selectable.
	authCheckRefreshChainUnproven = "refresh_chain_unproven"
)

// AuthCheckClassSuperseded mirrors accountwiring's authFailureCredentialSuperseded
// (services/bossd/internal/accountwiring/credcheck.go), which this module cannot
// import across the module boundary. It is the one failure class that qualifies a
// HEALTHY verdict: the provider accepted the stored credential, but an ambient
// codex login for the same provider account holds a DIFFERENT refresh token, so
// the stored refresh chain is already dead and the account will stop working when
// its access token expires (BOS-1175).
//
// The remedy is `boss account reauth`, which an operator can only reach for if
// the state is on the screen — hence BOS-1142's rule that a state no operator can
// see is not a report.
const AuthCheckClassSuperseded = "credential_superseded"

// supersededLabel is what the CHECK column renders for a superseded credential.
// It keeps "ok" as its head because the verdict genuinely is ok — eligibility is
// unaffected and the account is still selectable — and carries the class after it
// because "ok" alone would lose the entire warning.
const supersededLabel = "ok:" + AuthCheckClassSuperseded

// checkSeverity classifies how much weight the last credential check carries.
// It is the Go twin of the web surface's checkSeverity
// (services/web/src/lib/accountRows.ts) — the two surfaces fold the same two
// sources into one indicator, so they must classify the same verdict the same
// way or an operator gets a different answer depending on where they look.
type checkSeverity int

const (
	// checkSeverityNone: never checked. Not the same as a clean check (BOS-892).
	checkSeverityNone checkSeverity = iota
	// checkSeverityOK: the last check passed.
	checkSeverityOK
	// checkSeverityUndetermined: the check ran but could not reach a verdict
	// (transient provider failure, no runner wired, an outcome this build does
	// not know). Never grounds for a credential accusation — but never evidence
	// of health either.
	checkSeverityUndetermined
	// checkSeverityInvalid: the provider confirmed the stored credential is
	// unusable.
	checkSeverityInvalid
)

// accountCheckSeverity classifies a's most recent credential-check verdict.
//
// An outcome this build does not recognise is UNDETERMINED, never invalid: a
// newer daemon must not be able to make an older TUI accuse a credential it
// cannot classify — and must not be able to make it vouch for one either.
func accountCheckSeverity(a *pb.Account) checkSeverity {
	switch a.GetAuthCheck().GetOutcome() {
	case authCheckNever:
		return checkSeverityNone
	case authCheckHealthy:
		// Superseded stays OK here on purpose. This severity drives ELIGIBILITY
		// framing — the HEALTH veto — and a superseded refresh chain is a warning
		// about the future, not a present rejection. The state is reported in the
		// CHECK cell, which is where a warning about the future belongs; vetoing
		// the health green would claim the provider had refused a credential it
		// just accepted.
		return checkSeverityOK
	case authCheckAuthInvalid:
		return checkSeverityInvalid
	case authCheckTransient, authCheckUnavailable:
		return checkSeverityUndetermined
	case authCheckRefreshChainUnproven:
		// Undetermined by an explicit case rather than by falling through to
		// the unknown-outcome default: this build DOES know the outcome, and
		// the classification is a deliberate choice about what it means. The
		// check reached a verdict, so it is not "I could not ask" — but what it
		// established is that the refresh chain is UNPROVEN, which is the
		// absence of the confidence a green claims. Warning tier, never green,
		// never an accusation.
		return checkSeverityUndetermined
	default:
		return checkSeverityUndetermined
	}
}

// accountCheckLabel renders what the last daemon-owned credential verification
// concluded, as plain uncolored text.
//
// BOS-892: "never checked" and "checked and found nothing wrong" are different
// facts about a credential and must not collapse into one cell. An operator who
// reads "ok" on an account that was never verified treats an unproven
// credential as proven, which is exactly the wrong conclusion for the account
// that is about to be bound to a session. So the never-checked state gets its
// own words rather than an empty cell or a borrowed "ok".
//
// A non-healthy outcome carries its failure class inline rather than after it,
// because the class is the load-bearing half: "failed" alone does not tell an
// operator whether to reauthenticate or to wait. Ordering it behind a longer
// field is the BOS-892 truncation trap — the column budget would eat precisely
// the part that decides the remedy.
func accountCheckLabel(a *pb.Account) string {
	ac := a.GetAuthCheck()
	outcome := ac.GetOutcome()
	switch outcome {
	case authCheckNever:
		return "never checked"
	case authCheckHealthy:
		// A healthy verdict normally has no failure to classify, so a stale or
		// defaulted class must never turn a clean result into "ok:something".
		// credential_superseded is the sole exception, and it is matched by
		// EXACT VALUE rather than by non-emptiness for exactly that reason.
		if ac.GetFailureClass() == AuthCheckClassSuperseded {
			return supersededLabel
		}
		return "ok"
	case authCheckAuthInvalid:
		if cls := ac.GetFailureClass(); cls != "" {
			return "failed:" + cls
		}
		return "failed"
	case authCheckRefreshChainUnproven:
		// The outcome alone, deliberately WITHOUT its failure class. Here the
		// outcome token is the whole message — "refresh_not_observed" restates
		// it rather than naming a different remedy — and the pair would be 43
		// columns against a CHECK budget of 24 (rebuildTable), so appending it
		// would spend the truncation on the identifying half of the label. That
		// is the BOS-892 trap the auth_invalid branch above orders around.
		return outcome
	default:
		// transient / unavailable / anything the daemon adds later: report the
		// outcome verbatim rather than folding an unrecognised verdict into
		// either "ok" or "failed". A verdict this surface does not understand
		// is not evidence of health.
		if cls := ac.GetFailureClass(); cls != "" {
			return outcome + ":" + cls
		}
		return outcome
	}
}

// accountCheckFailed reports whether the last credential verification concluded
// that the provider rejected the stored credential. It is deliberately narrow:
// a transient or unavailable verdict is NOT a credential fault (BOS-881), and
// reporting one as such would send an operator to reauthenticate a credential
// that is fine.
func accountCheckFailed(a *pb.Account) bool {
	return a.GetAuthCheck().GetOutcome() == authCheckAuthInvalid
}

// accountCheckCell colors the CHECK column: green only for a verified-clean
// check, red for a confirmed credential fault, uncolored for everything else —
// including "never checked", which must not read as reassurance.
func accountCheckCell(label string) string {
	switch {
	case label == "ok":
		return styleStatusSuccess.Render(label)
	case label == supersededLabel:
		// Warning, not danger and not the clean green: the credential works
		// right now, and its refresh chain does not. Colour is only the
		// reinforcement — the label already carries the whole state in words, so
		// a monochrome or NO_COLOR run loses nothing (WCAG 1.4.1).
		return styleStatusWarning.Render(label)
	case strings.HasPrefix(label, "failed"):
		return styleStatusDanger.Render(label)
	default:
		return label
	}
}

// accountCheckAgeCell renders how long ago the last check completed, or "—"
// when none ever did. A staleness age that cannot be claimed for every row is
// worse than no age, so a never-checked row shows the em dash rather than
// borrowing another timestamp.
func accountCheckAgeCell(a *pb.Account) string {
	ts := a.GetAuthCheck().GetCheckedAt()
	if ts == nil || ts.AsTime().IsZero() {
		return "—"
	}
	return relTimeAgo(ts.AsTime())
}

// accountCheckedDetail renders the detail-screen line for the credential check:
// the verdict, when it was reached, and — for a failure — the redacted
// diagnostic. CREDENTIAL SAFETY: the diagnostic reaches the screen only through
// maskTestError, the single masking path in this package.
func accountCheckedDetail(a *pb.Account) string {
	label := accountCheckLabel(a)
	if label == "never checked" {
		return label
	}
	out := label
	if age := accountCheckAgeCell(a); age != "—" {
		out += " · " + age
	}
	if accountCheckFailed(a) {
		if e := a.GetLastTestError(); e != "" {
			out += " · " + maskTestError(e)
		}
	}
	return out
}

// Marks the health cell wears when the credential check vetoes its green. They
// are the NON-COLOUR half of the veto: CHECK is a lower-priority column than
// HEALTH (accounts_list.go rebuildTable), so CHECK drops first at narrow widths
// and the veto would otherwise be carried by colour alone — a distinction no
// colour-blind reader, no monochrome terminal and no NO_COLOR run can make
// (WCAG 1.4.1). They are one rune because the HEALTH column is only as wide as
// its own header; a word would be truncated away by the very widths that make
// the mark necessary.
const (
	// healthVetoInvalidMark: health says ok, the provider says the credential is
	// not.
	healthVetoInvalidMark = "!"
	// healthVetoUnprovenMark: health says ok, but the last check reached no
	// verdict — nothing has established the confidence the green would claim.
	healthVetoUnprovenMark = "?"
	// healthSupersededMark: health says ok and the check PASSED, but the stored
	// refresh chain has been superseded by an ambient codex login (BOS-1175).
	//
	// It is not a veto — eligibility is genuinely unaffected and
	// accountCheckSeverity deliberately keeps this case at checkSeverityOK — but
	// it still needs its own channel here, because CHECK is priority 4 in
	// fitColumnsIndexed while HEALTH is priority 3: once the terminal narrows
	// enough to drop CHECK, the CHECK cell that carries the whole warning is
	// gone and an unmarked green would make a superseded row byte-identical to a
	// clean one. That is the "a state no operator can see is not a report" rule
	// the two marks above exist for, reached by width rather than by colour.
	healthSupersededMark = "~"
)

// accountHealthCellFor renders the HEALTH label for a, refusing green whenever
// the last credential check did not pass.
//
// Health and the credential check are two sources feeding one indicator, and
// the fold has to take both: health is written by several writers (usage caps,
// injection failures, operator tests) and a stale ok from any of them would
// otherwise paint a dominant green over an account the provider has just
// rejected — the "green while broken" state this whole change exists to remove.
// The check verdict is authoritative in one direction only: it can veto an ok,
// never manufacture one.
//
// An UNDETERMINED check vetoes too, matching the web fold's "Unverified" amber
// (accountHealthStatus, services/web/src/lib/accountRows.ts). "I could not ask"
// is not an accusation, but it is not proof of health either, and a green here
// would claim a confidence no check established — the same dominant green over
// an unproven credential the acceptance criterion rules out. Only NEVER-checked
// keeps the green, exactly as on the web: a freshly registered account is the
// normal case, and spending the warning tier on it would spend it until it
// meant nothing.
//
// A vetoed ok renders in the warning tier rather than the danger tier: the
// health field genuinely does say ok, and repainting its own text red would
// assert a health verdict this fold did not make. The adjacent CHECK cell
// carries the red and the remedy.
func accountHealthCellFor(a *pb.Account, health string) string {
	if health != "ok" {
		return accountHealthCell(health)
	}
	switch accountCheckSeverity(a) {
	case checkSeverityInvalid:
		return styleStatusWarning.Render(health + healthVetoInvalidMark)
	case checkSeverityUndetermined:
		return styleStatusWarning.Render(health + healthVetoUnprovenMark)
	case checkSeverityOK:
		// A superseded credential passed its check, so it falls through the veto
		// arms above by design. Marked by EXACT failure-class value, mirroring
		// accountCheckLabel: an unrecognised class on a healthy outcome keeps the
		// plain green, so a newer daemon cannot make this build mark a row it
		// cannot explain.
		//
		// This arm is checkSeverityOK SPECIFICALLY, not a default. Severity OK is
		// reached only from a HEALTHY outcome, which is the same gate
		// accountCheckLabel applies before it renders the class — and sharing a
		// default arm with checkSeverityNone broke it: a never-checked row
		// (outcome "") carrying a stale class would paint the mark in HEALTH
		// while CHECK read "never checked", so the two cells contradicted each
		// other about whether the account had ever been asked.
		if a.GetAuthCheck().GetFailureClass() == AuthCheckClassSuperseded {
			return styleStatusWarning.Render(health + healthSupersededMark)
		}
		return accountHealthCell(health)
	default:
		// checkSeverityNone: never checked. Nothing was asked, so no class this
		// row happens to carry describes an answer, and none is rendered.
		return accountHealthCell(health)
	}
}
