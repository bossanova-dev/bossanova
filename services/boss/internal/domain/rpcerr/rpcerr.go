// Package rpcerr classifies RPC failures without depending on a transport.
package rpcerr

import "errors"

type Kind string

const (
	Unknown            Kind = "unknown"
	Canceled           Kind = "canceled"
	InvalidArgument    Kind = "invalid_argument"
	DeadlineExceeded   Kind = "deadline_exceeded"
	NotFound           Kind = "not_found"
	AlreadyExists      Kind = "already_exists"
	PermissionDenied   Kind = "permission_denied"
	ResourceExhausted  Kind = "resource_exhausted"
	FailedPrecondition Kind = "failed_precondition"
	Aborted            Kind = "aborted"
	OutOfRange         Kind = "out_of_range"
	Unimplemented      Kind = "unimplemented"
	Internal           Kind = "internal"
	Unavailable        Kind = "unavailable"
	DataLoss           Kind = "data_loss"
	Unauthenticated    Kind = "unauthenticated"
)

func (k Kind) String() string { return string(k) }
func KindOf(err error) Kind {
	var classified interface{ RPCKind() Kind }
	if errors.As(err, &classified) {
		return classified.RPCKind()
	}
	return Unknown
}

type failure struct {
	kind Kind
	err  error
}

func (e *failure) Error() string {
	if e.err == nil {
		return e.kind.String()
	}
	return e.kind.String() + ": " + e.err.Error()
}
func (e *failure) Unwrap() error { return e.err }
func (e *failure) RPCKind() Kind { return e.kind }
func (e *failure) RPCMessage() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}
func New(kind Kind, err error) error { return &failure{kind: kind, err: err} }

// Message returns the peer's original message when the transport provided one.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var source interface{ RPCMessage() string }
	if errors.As(err, &source) {
		return source.RPCMessage()
	}
	return err.Error()
}
func IsWireError(err error) bool {
	var source interface{ RPCWireError() bool }
	return errors.As(err, &source) && source.RPCWireError()
}
