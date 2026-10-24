package domain

import "errors"

var ErrBranchExists = errors.New("branch already exists")
var ErrBaseBranchNotReady = errors.New("base branch not ready for sync")
var ErrLocalSyncDeferred = errors.New("local base sync deferred")
var ErrRefLockContended = errors.New("concurrent git updated the same remote-tracking ref")
var ErrMergeConflict = errors.New("merge conflict")
var ErrInvalidBranchName = errors.New("invalid branch name")

type RebaseResult struct {
	PriorHead string
	NewHead   string
}
