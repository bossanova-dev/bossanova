package vcs

import (
	"fmt"
	"strings"
	"unicode"
)

// CheckVerdictState is the aggregate state of a PR's check-run set.
type CheckVerdictState int

const (
	CheckVerdictUnknown CheckVerdictState = iota + 1
	CheckVerdictGreen
	CheckVerdictFailing
	CheckVerdictPending
)

func (s CheckVerdictState) String() string {
	switch s {
	case CheckVerdictGreen:
		return "Green"
	case CheckVerdictFailing:
		return "Failing"
	case CheckVerdictPending:
		return "Pending"
	case CheckVerdictUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("CheckVerdictState(%d)", int(s))
	}
}

const (
	CheckVerdictReasonOK           = "ok"
	CheckVerdictReasonNoGateRan    = "no-gate-ran"
	CheckVerdictReasonFailed       = "failed"
	CheckVerdictReasonPending      = "pending"
	CheckVerdictReasonNoChecks     = "no-checks"
	CheckVerdictReasonUnclassified = "unclassified"
	CheckVerdictReasonUnreadable   = "unreadable"
	CheckVerdictReasonStaleSHA     = "stale-sha"
	// CheckVerdictReasonNotRequested: a required check has no run on the head
	// (absent, or present only as skipped/neutral). Nothing will start it on its
	// own, so it is neither pending nor green.
	CheckVerdictReasonNotRequested = "not-requested"
)

// RequiredChecks is a repository's configured list of check names that must
// have run on a head before it can be green (.boss-skills.json ci.required).
type RequiredChecks struct {
	// Names are exact check-run names.
	Names []string
	// ExemptBases lists PR base branches the requirement does not apply to.
	ExemptBases []string
}

// For returns the names required on a PR into base, or nil when base is exempt
// or nothing is required.
func (r RequiredChecks) For(base string) []string {
	if len(r.Names) == 0 {
		return nil
	}
	for _, exempt := range r.ExemptBases {
		if exempt == base {
			return nil
		}
	}
	return r.Names
}

// CheckVerdict is the single aggregate definition of whether a check-run set is green.
type CheckVerdict struct {
	State        CheckVerdictState
	Reason       string
	HeadSHA      string
	Total        int
	Passed       int
	Failed       int
	Pending      int
	Skipped      int
	Unclassified int
	// NotRequested lists the required names with no run on the head, in
	// requirement order. Empty when nothing is required or every name ran.
	NotRequested []string
}

// EvaluateChecks classifies a check-run set without consulting external state.
// It requires no named checks; see EvaluateChecksRequiring.
//
// A non-completed boss/verify status, park or cannot-assess hold (see IsVerifyHold) is not
// ordinary CI: it is the verify stage holding a head whose CI already settled,
// so it is skipped before counting and Total counts only the judged entries.
// Every "is ordinary CI settled" caller (display, the state poller's
// ChecksPassed, callback triggers, dependabot) inherits that exclusion here.
// A completed boss/verify without a hold is still an ordinary check.
func EvaluateChecks(headSHA string, checks []CheckResult, readErr error) CheckVerdict {
	return EvaluateChecksRequiring(headSHA, checks, readErr, nil)
}

// EvaluateChecksRequiring is EvaluateChecks with a list of check names that
// must have run on the head. A required name has run when at least one
// ordinary check with exactly that name is pending, unclassified, or completed
// with a conclusion other than skipped/neutral. Precedence:
//
//	unreadable > failed > unclassified > pending > not-requested >
//	no-checks > no-gate-ran > ok
func EvaluateChecksRequiring(headSHA string, checks []CheckResult, readErr error, required []string) CheckVerdict {
	v := CheckVerdict{
		State:   CheckVerdictUnknown,
		Reason:  CheckVerdictReasonNoChecks,
		HeadSHA: headSHA,
	}

	ran := make(map[string]bool, len(required))
	for _, c := range checks {
		if IsVerifyHold(c) {
			continue
		}
		if v.count(c) {
			ran[c.Name] = true
		}
	}
	for _, name := range required {
		if !ran[name] {
			v.NotRequested = append(v.NotRequested, name)
		}
	}

	v.State, v.Reason = v.classify(readErr)
	return v
}

// count tallies one ordinary check and reports whether it counts as a run.
func (v *CheckVerdict) count(c CheckResult) bool {
	v.Total++
	if c.Status != CheckStatusCompleted {
		v.Pending++
		return true
	}
	if c.Unclassified || c.Conclusion == nil {
		v.Unclassified++
		return true
	}
	switch *c.Conclusion {
	case CheckConclusionSuccess:
		v.Passed++
	case CheckConclusionFailure, CheckConclusionCancelled, CheckConclusionTimedOut:
		v.Failed++
	case CheckConclusionNeutral, CheckConclusionSkipped:
		v.Skipped++
		return false
	}
	return true
}

func (v CheckVerdict) classify(readErr error) (CheckVerdictState, string) {
	switch {
	case readErr != nil:
		return CheckVerdictUnknown, CheckVerdictReasonUnreadable
	case v.Failed > 0:
		return CheckVerdictFailing, CheckVerdictReasonFailed
	case v.Unclassified > 0:
		return CheckVerdictUnknown, CheckVerdictReasonUnclassified
	case v.Pending > 0:
		return CheckVerdictPending, CheckVerdictReasonPending
	case len(v.NotRequested) > 0:
		return CheckVerdictUnknown, CheckVerdictReasonNotRequested
	case v.Total == 0:
		return CheckVerdictUnknown, CheckVerdictReasonNoChecks
	case v.Passed == 0:
		return CheckVerdictGreen, CheckVerdictReasonNoGateRan
	default:
		return CheckVerdictGreen, CheckVerdictReasonOK
	}
}

// At returns this verdict when observed at sha, or an unknown stale verdict on mismatch.
func (v CheckVerdict) At(sha string) CheckVerdict {
	if v.HeadSHA != "" && sha != "" && v.HeadSHA == sha {
		return v
	}
	v.State = CheckVerdictUnknown
	v.Reason = CheckVerdictReasonStaleSHA
	return v
}

func (v CheckVerdict) IsGreen() bool {
	return v.State == CheckVerdictGreen
}

func (v CheckVerdict) DemonstratedPass() bool {
	return v.IsGreen() && v.Passed > 0
}

// BuildReceiptContext records a successful boss-build hand-off on one commit.
// It is provenance, never a CI gate.
const BuildReceiptContext = "boss/build"

// VerifyStatusContext is the commit-status context the verify stage
// (skills-toolbox/verify/verify-gate.mjs VERIFY_CONTEXT) writes on a PR head. GitHub
// contexts are case-sensitive, so it is matched exactly.
const VerifyStatusContext = "boss/verify"

// verifyNeedsHumanPrefix is the description prefix verify-gate.mjs
// (VERIFY_DESCRIPTIONS.needsHuman, without its colon) uses to park a head for
// a human.
const verifyNeedsHumanPrefix = "needs human"

// verifyDefectPrefix mirrors VERIFY_DESCRIPTIONS.defect in
// skills-toolbox/verify/verify-vocabulary.mjs.
const verifyDefectPrefix = "defect:"

// verifyBlockedPrefix mirrors VERIFY_DESCRIPTIONS.blocked in
// skills-toolbox/verify/verify-vocabulary.mjs.
const verifyBlockedPrefix = "blocked:"

// verifyCannotAssessPrefix mirrors VERIFY_DESCRIPTIONS.cannotAssess in
// skills-toolbox/verify/verify-vocabulary.mjs.
const verifyCannotAssessPrefix = "cannot assess:"

// verifyWaitingPrefix mirrors VERIFY_DESCRIPTIONS.waiting in
// skills-toolbox/verify/verify-vocabulary.mjs.
const verifyWaitingPrefix = "waiting:"

// verifyParkedSuffix mirrors VERIFY_DESCRIPTIONS.defectParkedSuffix in
// skills-toolbox/verify/verify-vocabulary.mjs.
const verifyParkedSuffix = ", needs human"

// verifyParkLabelSeparator separates a park's codes from its operator-written
// label (verify-gate.mjs PARK_LABEL_SEPARATOR). Codes never contain it, so the
// first occurrence ends the code set.
const verifyParkLabelSeparator = " · "

// VerifyPhaseKind is the verify stage's hold on a PR head, if any.
type VerifyPhaseKind int

const (
	// VerifyPhaseNone means no verify hold is on the head.
	VerifyPhaseNone VerifyPhaseKind = iota
	// VerifyPhaseVerifying means a pending boss/verify status that is neither
	// a park nor a wait: a live claim ("verifying…"), a bare "waiting:" with no
	// reason, or an empty/unrecognised description; also a cannot-assess hold
	// in any state.
	VerifyPhaseVerifying
	// VerifyPhaseNeedsHuman means a park in any state whose description
	// starts "needs human".
	VerifyPhaseNeedsHuman
	// VerifyPhaseWaiting means a pending boss/verify status "waiting: <reason>"
	// with a non-empty reason: verify judged the head and chose to wait.
	VerifyPhaseWaiting
)

// VerifyPhase is ClassifyVerify's result. Reason carries the park's codes for
// VerifyPhaseNeedsHuman (empty when the description names none) or the wait's
// trimmed reason code for VerifyPhaseWaiting (for example
// "merge-state-unknown"). Label is the operator-written text after a park's
// codes (empty when the park carries none). Both are empty otherwise, so a
// live claim never carries a reason.
type VerifyPhase struct {
	Kind   VerifyPhaseKind
	Reason string
	Label  string
}

// IsVerifyPark reports whether c is a boss/verify park in any state.
func IsVerifyPark(c CheckResult) bool {
	if c.Name != VerifyStatusContext {
		return false
	}
	_, _, ok := parseNeedsHuman(c.Description)
	return ok
}

// IsVerifyCannotAssess reports whether c is a cannot-assess hold in any state.
func IsVerifyCannotAssess(c CheckResult) bool {
	return c.Name == VerifyStatusContext && strings.HasPrefix(c.Description, verifyCannotAssessPrefix)
}

// IsVerifyHold reports whether c is a not-yet-completed boss/verify status,
// or a park or cannot-assess status in any state. Holds are excluded from ordinary CI.
func IsVerifyHold(c CheckResult) bool {
	return c.Name == VerifyStatusContext && (c.Status != CheckStatusCompleted || IsVerifyPark(c) || IsVerifyCannotAssess(c))
}

// IsVerifyRepairing reports whether verify handed this head back to repair.
// Parked send-backs are waiting for a human instead.
func IsVerifyRepairing(c CheckResult) bool {
	return c.Name == VerifyStatusContext && c.Status == CheckStatusCompleted &&
		c.Conclusion != nil && *c.Conclusion == CheckConclusionFailure &&
		(strings.HasPrefix(c.Description, verifyDefectPrefix) || strings.HasPrefix(c.Description, verifyBlockedPrefix)) &&
		!strings.HasSuffix(c.Description, verifyParkedSuffix)
}

// HasVerifyRepairing reports whether checks contain an unparked verify send-back.
func HasVerifyRepairing(checks []CheckResult) bool {
	for _, c := range checks {
		if IsVerifyRepairing(c) {
			return true
		}
	}
	return false
}

// VerifyPassed reports whether checks carry a completed, successful
// boss/verify status: the verify stage passed the head the checks were read
// on, itself or through an operator's approval of its park.
func VerifyPassed(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Name == VerifyStatusContext && c.Status == CheckStatusCompleted &&
			c.Conclusion != nil && *c.Conclusion == CheckConclusionSuccess && !IsVerifyHold(c) {
			return true
		}
	}
	return false
}

// VerifyJudged reports whether verify posted a verdict (park, pass, defect or
// blocked) on the head the checks were read on. A claim or wait is not a verdict.
func VerifyJudged(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Name == VerifyStatusContext && (IsVerifyPark(c) ||
			(c.Status == CheckStatusCompleted && !IsVerifyHold(c) && c.Conclusion != nil &&
				(*c.Conclusion == CheckConclusionSuccess || *c.Conclusion == CheckConclusionFailure))) {
			return true
		}
	}
	return false
}

// ClassifyVerify derives the verify phase from a check set. A park in any
// state is NeedsHuman with its reason and label; a pending "waiting: <reason>"
// status with a non-empty reason is Waiting with that reason; any other
// pending boss/verify or cannot-assess hold is Verifying with no reason.
// Completed statuses without a hold yield VerifyPhaseNone. If several holds
// are present, NeedsHuman wins, then Waiting.
func ClassifyVerify(checks []CheckResult) VerifyPhase {
	phase := VerifyPhase{Kind: VerifyPhaseNone}
	for _, c := range checks {
		if !IsVerifyHold(c) {
			continue
		}
		if reason, label, ok := parseNeedsHuman(c.Description); ok {
			return VerifyPhase{Kind: VerifyPhaseNeedsHuman, Reason: reason, Label: label}
		}
		if reason, ok := parseVerifyWait(c); ok {
			phase = VerifyPhase{Kind: VerifyPhaseWaiting, Reason: reason}
			continue
		}
		if phase.Kind == VerifyPhaseNone {
			phase.Kind = VerifyPhaseVerifying
		}
	}
	return phase
}

// parseVerifyWait returns the reason of a pending "waiting: <reason>" status.
// A completed status, or a bare "waiting:" with no reason, is not a wait.
func parseVerifyWait(c CheckResult) (string, bool) {
	if c.Status == CheckStatusCompleted || !strings.HasPrefix(c.Description, verifyWaitingPrefix) {
		return "", false
	}
	reason := strings.TrimSpace(strings.TrimPrefix(c.Description, verifyWaitingPrefix))
	return reason, reason != ""
}

// parseNeedsHuman reports whether description is a "needs human" park and
// returns its reason (the code set) and label. The prefix must end the
// description or be followed by a colon or whitespace, so "needs humans" is not
// a park. The label is whatever follows the first verifyParkLabelSeparator; a
// park without one has an empty label and parses exactly as before.
func parseNeedsHuman(description string) (reason, label string, ok bool) {
	d := strings.TrimLeftFunc(description, unicode.IsSpace)
	n := len(verifyNeedsHumanPrefix)
	if len(d) < n || !strings.EqualFold(d[:n], verifyNeedsHumanPrefix) {
		return "", "", false
	}
	rest := d[n:]
	if rest != "" && rest[0] != ':' && !unicode.IsSpace(rune(rest[0])) {
		return "", "", false
	}
	rest = strings.TrimPrefix(strings.TrimSpace(rest), ":")
	if codes, text, found := strings.Cut(rest, verifyParkLabelSeparator); found {
		return expandParkCodes(codes), strings.TrimSpace(text), true
	}
	return expandParkCodes(rest), "", true
}

// Oversized verify descriptions abbreviate human-review: as + without dropping codes.
// Decode at the status boundary so every attention and approval consumer sees canonical codes.
// apiversion-skip: this new status encoding has no historical response to restore;
// older daemons keep their generic unknown-code fallback. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
func expandParkCodes(raw string) string {
	codes := strings.Split(strings.TrimSpace(raw), ",")
	for i, code := range codes {
		if suffix, ok := strings.CutPrefix(strings.TrimSpace(code), "+"); ok {
			codes[i] = "human-review:" + suffix
		}
	}
	return strings.Join(codes, ",")
}
