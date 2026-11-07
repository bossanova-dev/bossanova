package apiversion

import (
	"errors"
	"strings"

	"connectrpc.com/connect"
)

type cloudControlDisabled struct{ err error }

// MarkCloudControlDisabled identifies a refusal produced by the daemon's cloud
// control policy, so compatibility handling leaves other preconditions unchanged.
func MarkCloudControlDisabled(err error) error {
	if err == nil {
		return nil
	}
	return &cloudControlDisabled{err: err}
}

func (e *cloudControlDisabled) Error() string { return e.err.Error() }
func (e *cloudControlDisabled) Unwrap() error { return e.err }

// CloudControlDisabledCodeChange preserves the old fallback code for older
// clients. Although these procedures already use FailedPrecondition for other
// failures, changing the code for this refusal is observable API behavior.
type CloudControlDisabledCodeChange struct{}

// Version implements VersionChange.
func (CloudControlDisabledCodeChange) Version() Version { return V20261103 }

// TransformResponse implements VersionChange; only errors change.
func (CloudControlDisabledCodeChange) TransformResponse(string, any) {}

// TransformError restores the legacy code only for marked orchestrator errors.
func (CloudControlDisabledCodeChange) TransformError(method string, err error) error {
	if !strings.HasPrefix(method, "/bossanova.v1.OrchestratorService/") {
		return err
	}
	var marker *cloudControlDisabled
	var ce *connect.Error
	if !errors.As(err, &marker) || !errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeAborted, errors.New(ce.Message()))
}
