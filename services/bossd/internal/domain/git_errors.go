package domain

import "errors"

// ErrRebaseConflict identifies a rebase stopped on a content conflict.
var ErrRebaseConflict = errors.New("rebase conflict")

// ErrBranchNotPushed identifies a local branch tip that differs from origin.
var ErrBranchNotPushed = errors.New("local branch tip differs from origin")

// ErrUnsafeCloneURL identifies a clone URL rejected by transport validation.
var ErrUnsafeCloneURL = errors.New("unsafe clone URL")
