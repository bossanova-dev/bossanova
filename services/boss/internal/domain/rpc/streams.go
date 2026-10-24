// Package rpc defines transport-independent client values and stream contracts.
package rpc

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AttachStream abstracts a server-streaming attach response, allowing both
// local (DaemonService) and remote (OrchestratorService) implementations.
type AttachStream interface {
	// Receive advances the stream. Returns false when done or on error.
	Receive() bool
	// Msg returns the most recent message from the stream.
	Msg() *AttachEvent
	// Err returns the stream error, if any.
	Err() error
	// Close closes the stream.
	Close() error
}

// AttachEvent is a unified attach event for both local and remote streams.
type AttachEvent struct {
	OutputLine   *pb.OutputLine
	StateChange  *pb.StateChange
	SessionEnded *pb.SessionEnded
}

// ResurrectSessionStream abstracts a server-streaming resurrect session
// response. Same four-method shape as CreateSessionStream.
type ResurrectSessionStream interface {
	// Receive advances the stream. Returns false when done or on error.
	Receive() bool
	// Msg returns the most recent message from the stream.
	Msg() *pb.ResurrectSessionResponse
	// Err returns the stream error, if any.
	Err() error
	// Close closes the stream.
	Close() error
}

// CreateSessionStream abstracts a server-streaming create session response.
type CreateSessionStream interface {
	// Receive advances the stream. Returns false when done or on error.
	Receive() bool
	// Msg returns the most recent message from the stream.
	Msg() *pb.CreateSessionResponse
	// Err returns the stream error, if any.
	Err() error
	// Close closes the stream.
	Close() error
}

// WatchSessionSetupStream streams a bootstrap's replay tail, live output and outcome.
type WatchSessionSetupStream interface {
	Receive() bool
	Msg() *pb.WatchSessionSetupResponse
	Err() error
	Close() error
}
