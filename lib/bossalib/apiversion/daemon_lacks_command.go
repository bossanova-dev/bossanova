package apiversion

import (
	"errors"

	"connectrpc.com/connect"
)

// daemonLacksCommand marks a connect error as a command the session's daemon is
// too old to run: its snapshot does not list the command, or it answered
// ERROR_CODE_UNIMPLEMENTED. bosso's validateCommandResult wraps with it through
// MarkDaemonLacksCommand; DaemonLacksCommandCodeChange matches it.
//
// The type is unexported and only MarkDaemonLacksCommand builds it, so the zero
// value is unreachable.
type daemonLacksCommand struct{ err error }

// MarkDaemonLacksCommand wraps err in the daemon-lacks-command marker. Returns
// nil for a nil err.
func MarkDaemonLacksCommand(err error) error {
	if err == nil {
		return nil
	}
	return &daemonLacksCommand{err: err}
}

// IsDaemonLacksCommand reports whether err carries the daemon-lacks-command
// marker anywhere in its chain.
func IsDaemonLacksCommand(err error) bool {
	var marker *daemonLacksCommand
	return errors.As(err, &marker)
}

func (e *daemonLacksCommand) Error() string { return e.err.Error() }

func (e *daemonLacksCommand) Unwrap() error { return e.err }

// TransformResponse implements VersionChange. This change lives on the error
// path only.
func (DaemonLacksCommandCodeChange) TransformResponse(string, any) {}

// TransformError implements ErrorTransform. It rewrites only a marked error,
// to CodeDeadlineExceeded with the original message.
func (DaemonLacksCommandCodeChange) TransformError(_ string, err error) error {
	if err == nil || !IsDaemonLacksCommand(err) {
		return err
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return err
	}
	return connect.NewError(connect.CodeDeadlineExceeded, errors.New(connectErr.Message()))
}
