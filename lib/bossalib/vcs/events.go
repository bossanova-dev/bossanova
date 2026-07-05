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
	// HeadSHA is the PR head commit SHA observed when the failure was emitted.
	// The dispatcher compares it against the session's last-counted attempt SHA
	// to decide whether this fix-loop lap consumes an attempt (a real fix pushes
	// a new commit) or is a free CI-settle lap on an unchanged commit (BOS-235).
	HeadSHA string
}

// ConflictDetected indicates a merge conflict was detected on a PR.
type ConflictDetected struct {
	PRID int
	// HeadSHA carries the PR head SHA for head-SHA-gated attempt counting; see
	// ChecksFailed.HeadSHA.
	HeadSHA string
}

// ReviewSubmitted indicates a code review was submitted on a PR.
type ReviewSubmitted struct {
	PRID     int
	ReviewID int64
	Author   string
	State    ReviewState
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
