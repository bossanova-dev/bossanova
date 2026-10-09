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
)

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
}

// EvaluateChecks classifies a check-run set without consulting external state.
//
// A non-completed boss/verify commit status (see IsPendingVerify) is not
// ordinary CI: it is the verify stage holding a head whose CI already settled,
// so it is skipped before counting and Total counts only the judged entries.
// Every "is ordinary CI settled" caller (display, the state poller's
// ChecksPassed, callback triggers, dependabot) inherits that exclusion here.
// A completed boss/verify (success/failure) is still an ordinary check.
func EvaluateChecks(headSHA string, checks []CheckResult, readErr error) CheckVerdict {
	v := CheckVerdict{
		State:   CheckVerdictUnknown,
		Reason:  CheckVerdictReasonNoChecks,
		HeadSHA: headSHA,
	}

	for _, c := range checks {
		if IsPendingVerify(c) {
			continue
		}
		v.Total++
		if c.Status != CheckStatusCompleted {
			v.Pending++
			continue
		}
		if c.Unclassified || c.Conclusion == nil {
			v.Unclassified++
			continue
		}
		switch *c.Conclusion {
		case CheckConclusionSuccess:
			v.Passed++
		case CheckConclusionFailure, CheckConclusionCancelled, CheckConclusionTimedOut:
			v.Failed++
		case CheckConclusionNeutral, CheckConclusionSkipped:
			v.Skipped++
		}
	}

	switch {
	case readErr != nil:
		v.State = CheckVerdictUnknown
		v.Reason = CheckVerdictReasonUnreadable
	case v.Failed > 0:
		v.State = CheckVerdictFailing
		v.Reason = CheckVerdictReasonFailed
	case v.Unclassified > 0:
		v.State = CheckVerdictUnknown
		v.Reason = CheckVerdictReasonUnclassified
	case v.Pending > 0:
		v.State = CheckVerdictPending
		v.Reason = CheckVerdictReasonPending
	case v.Total == 0:
		v.State = CheckVerdictUnknown
		v.Reason = CheckVerdictReasonNoChecks
	case v.Passed == 0:
		v.State = CheckVerdictGreen
		v.Reason = CheckVerdictReasonNoGateRan
	default:
		v.State = CheckVerdictGreen
		v.Reason = CheckVerdictReasonOK
	}

	return v
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
// (skills-toolbox/verify-gate.mjs VERIFY_CONTEXT) writes on a PR head. GitHub
// contexts are case-sensitive, so it is matched exactly.
const VerifyStatusContext = "boss/verify"

// verifyNeedsHumanPrefix is the description prefix verify-gate.mjs
// (VERIFY_DESCRIPTIONS.needsHuman, without its colon) uses to park a head for
// a human.
const verifyNeedsHumanPrefix = "needs human"

// VerifyPhaseKind is the verify stage's hold on a PR head, if any.
type VerifyPhaseKind int

const (
	// VerifyPhaseNone means no pending boss/verify status is on the head.
	VerifyPhaseNone VerifyPhaseKind = iota
	// VerifyPhaseVerifying means a pending boss/verify status that is not a
	// park: a live claim ("verifying…"), an unclaimed head ("waiting: ..."),
	// or an empty/unrecognised description.
	VerifyPhaseVerifying
	// VerifyPhaseNeedsHuman means a pending boss/verify status whose
	// description starts "needs human".
	VerifyPhaseNeedsHuman
)

// VerifyPhase is ClassifyVerify's result. Reason carries the park reason for
// VerifyPhaseNeedsHuman (empty when the description names none) and is empty
// otherwise.
type VerifyPhase struct {
	Kind   VerifyPhaseKind
	Reason string
}

// IsPendingVerify reports whether c is a not-yet-completed boss/verify commit
// status. Only those participate in ClassifyVerify and are excluded from
// EvaluateChecks.
func IsPendingVerify(c CheckResult) bool {
	return c.Name == VerifyStatusContext && c.Status != CheckStatusCompleted
}

// ClassifyVerify derives the verify phase from a check set. A pending
// boss/verify whose description starts "needs human" (case-insensitive,
// leading whitespace ignored, colon optional) is NeedsHuman with the trimmed
// remainder as its reason; any other pending boss/verify is Verifying. A
// completed boss/verify is an ordinary check and yields VerifyPhaseNone. If
// several pending entries are present, NeedsHuman wins.
func ClassifyVerify(checks []CheckResult) VerifyPhase {
	phase := VerifyPhase{Kind: VerifyPhaseNone}
	for _, c := range checks {
		if !IsPendingVerify(c) {
			continue
		}
		if reason, ok := parseNeedsHuman(c.Description); ok {
			return VerifyPhase{Kind: VerifyPhaseNeedsHuman, Reason: reason}
		}
		phase.Kind = VerifyPhaseVerifying
	}
	return phase
}

// parseNeedsHuman reports whether description is a "needs human" park and
// returns its reason. The prefix must end the description or be followed by a
// colon or whitespace, so "needs humans" is not a park.
func parseNeedsHuman(description string) (string, bool) {
	d := strings.TrimLeftFunc(description, unicode.IsSpace)
	n := len(verifyNeedsHumanPrefix)
	if len(d) < n || !strings.EqualFold(d[:n], verifyNeedsHumanPrefix) {
		return "", false
	}
	rest := d[n:]
	if rest != "" && rest[0] != ':' && !unicode.IsSpace(rune(rest[0])) {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, ":")
	return strings.TrimSpace(rest), true
}
