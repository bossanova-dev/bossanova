package vcs

// Event is a marker interface for all VCS events.
// Implementations use a unexported method to restrict the set of types.
type Event interface {
	vcsEvent()
}

// ChecksPassed indicates all CI checks passed on a PR.
type ChecksPassed struct {
	PRID int
}

// ChecksFailed indicates one or more CI checks failed on a PR.
type ChecksFailed struct {
	PRID         int
	FailedChecks []CheckResult
}

// ConflictDetected indicates a merge conflict was detected on a PR.
type ConflictDetected struct {
	PRID int
}

// ReviewSubmitted indicates a code review was submitted on a PR.
type ReviewSubmitted struct {
	PRID     int
	Comments []ReviewComment
}

// PRMerged indicates a PR was merged.
type PRMerged struct {
	PRID int
}

// PRClosed indicates a PR was closed without merging.
type PRClosed struct {
	PRID int
}

func (ChecksPassed) vcsEvent()     {}
func (ChecksFailed) vcsEvent()     {}
func (ConflictDetected) vcsEvent() {}
func (ReviewSubmitted) vcsEvent()  {}
func (PRMerged) vcsEvent()         {}
func (PRClosed) vcsEvent()         {}
