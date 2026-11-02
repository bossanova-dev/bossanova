package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/credsync"
)

// Gate failures the refresh broker cannot see on its own; their texts are the
// ones operators and log readers already know.
var (
	errCredentialIdentityNotPinned = credsync.ErrBootstrapUnpinned // the engine's FindBootstrap refusal
	errCredentialSyncUnavailable   = errors.New("credential synchronization unavailable")
)

// classifyRefreshGateError attaches the broker failure class only the gate can
// see. Every other error, including whatever ResolveRefreshVersion returns, is
// passed through unchanged and the broker reports it as unclassified.
func classifyRefreshGateError(err error) error {
	if class, ok := refreshGateClass(err); ok {
		return credmaterialize.NewRefreshFailure(class, err)
	}
	return err
}

func refreshGateClass(err error) (credmaterialize.RefreshFailureClass, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, errCredentialIdentityNotPinned), errors.Is(err, credsync.ErrBootstrapNotPinned):
		return credmaterialize.RefreshFailureIdentityNotPinned, true
	case errors.Is(err, credsync.ErrOwnDeviceUnverified):
		return credmaterialize.RefreshFailureDeviceNotEnrolled, true
	case errors.Is(err, errCredentialSyncUnavailable):
		return credmaterialize.RefreshFailureSyncUnavailable, true
	case errors.Is(err, credsync.ErrConflict), errors.Is(err, credsync.ErrCredentialReplaced):
		return credmaterialize.RefreshFailureCredentialChanged, true
	case errors.Is(err, context.DeadlineExceeded), connect.CodeOf(err) == connect.CodeUnavailable, connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		return credmaterialize.RefreshFailureSyncUnreachable, true
	default:
		return "", false
	}
}
