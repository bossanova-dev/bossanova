package domain

import (
	"context"
	"errors"
)

type cloudTargetContextKey struct{}

// ErrCloudTarget refuses a command whose locally authorized ownership changed.
var ErrCloudTarget = errors.New("cloud command target could not be authorized")

// CloudTarget maps object identifiers to their authorized repository.
type CloudTarget map[string]string

// WithCloudTarget carries an immutable copy of the command's authorization to
// handlers that reload its objects before acting on them.
func WithCloudTarget(ctx context.Context, targets CloudTarget) context.Context {
	copyTargets := make(CloudTarget, len(targets))
	for key, repo := range targets {
		copyTargets[key] = repo
	}
	return context.WithValue(ctx, cloudTargetContextKey{}, copyTargets)
}

// AssertCloudTarget rejects ownership changes between authorization and use.
func AssertCloudTarget(ctx context.Context, kind, id, repoID string) error {
	targets, cloud := ctx.Value(cloudTargetContextKey{}).(CloudTarget)
	if !cloud {
		return nil
	}
	if authorized, ok := targets[kind+":"+id]; ok {
		if authorized != repoID {
			return ErrCloudTarget
		}
	} else if targets["repo:"+repoID] != repoID {
		return ErrCloudTarget
	}
	return nil
}

// IsCloudCommand distinguishes a locally authorized stream call from local IPC.
func IsCloudCommand(ctx context.Context) bool {
	_, ok := ctx.Value(cloudTargetContextKey{}).(CloudTarget)
	return ok
}

type cloudRepoCheckKey struct{}

// WithCloudRepoCheck carries the local policy, which is read again at the point
// of effect. It grants nothing outside the command's frozen ownership bindings.
func WithCloudRepoCheck(ctx context.Context, check func(context.Context, string) bool) context.Context {
	return context.WithValue(ctx, cloudRepoCheckKey{}, check)
}

// CloudRepoAllowed leaves local IPC alone and fails closed for cloud calls
// without a current policy decision or a locally authorized target.
func CloudRepoAllowed(ctx context.Context, repoID string) bool {
	if !IsCloudCommand(ctx) {
		return true
	}
	if repoID == "" || AssertCloudTarget(ctx, "repo", repoID, repoID) != nil {
		return false
	}
	check, ok := ctx.Value(cloudRepoCheckKey{}).(func(context.Context, string) bool)
	return ok && check(ctx, repoID)
}
