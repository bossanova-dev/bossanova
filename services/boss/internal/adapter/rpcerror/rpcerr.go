package rpcerror

import (
	"errors"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
)

type rpcFailure struct{ error }

func (e rpcFailure) Unwrap() error        { return e.error }
func (e rpcFailure) RPCKind() rpcerr.Kind { return rpcerr.Kind(connect.CodeOf(e.error).String()) }
func (e rpcFailure) RPCMessage() string {
	var source *connect.Error
	if errors.As(e.error, &source) {
		return source.Message()
	}
	return e.Error()
}
func (e rpcFailure) RPCWireError() bool { return connect.IsWireError(e.error) }

// RPCDetails decodes the Connect error details attached to the failure,
// skipping any whose message type this binary cannot decode.
func (e rpcFailure) RPCDetails() []any {
	var source *connect.Error
	if !errors.As(e.error, &source) {
		return nil
	}
	var details []any
	for _, detail := range source.Details() {
		value, err := detail.Value()
		if err != nil {
			continue
		}
		details = append(details, value)
	}
	return details
}

// WrapError exposes transport classification while preserving the original error.
func WrapError(err error) error {
	if err == nil {
		return nil
	}
	var classified interface{ RPCKind() rpcerr.Kind }
	if errors.As(err, &classified) {
		return err
	}
	return rpcFailure{err}
}
