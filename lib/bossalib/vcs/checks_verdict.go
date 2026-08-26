package vcs

import "fmt"

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
func EvaluateChecks(headSHA string, checks []CheckResult, readErr error) CheckVerdict {
	v := CheckVerdict{
		State:   CheckVerdictUnknown,
		Reason:  CheckVerdictReasonNoChecks,
		HeadSHA: headSHA,
		Total:   len(checks),
	}

	for _, c := range checks {
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
	case len(checks) == 0:
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
