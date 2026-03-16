// Package client provides interfaces and implementations for communicating
// with the bossanova daemon, both locally (Unix socket) and remotely (orchestrator).
package client

import (
	"context"

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
	OutputLine  *pb.OutputLine
	StateChange *pb.StateChange
	SessionEnded *pb.SessionEnded
}

// BossClient defines the interface for all daemon operations.
// Both LocalClient (Unix socket) and RemoteClient (orchestrator proxy) implement this.
type BossClient interface {
	// Ping verifies the daemon is reachable.
	Ping(ctx context.Context) error

	// Context resolution
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)

	// Repo management
	RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error)
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	RemoveRepo(ctx context.Context, id string) error
	ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error)

	// Session lifecycle
	CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error)
	GetSession(ctx context.Context, id string) (*pb.Session, error)
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error)
	AttachSession(ctx context.Context, id string) (AttachStream, error)
	StopSession(ctx context.Context, id string) (*pb.Session, error)
	PauseSession(ctx context.Context, id string) (*pb.Session, error)
	ResumeSession(ctx context.Context, id string) (*pb.Session, error)
	RetrySession(ctx context.Context, id string) (*pb.Session, error)
	CloseSession(ctx context.Context, id string) (*pb.Session, error)
	RemoveSession(ctx context.Context, id string) error

	// Archive / Resurrect
	ArchiveSession(ctx context.Context, id string) (*pb.Session, error)
	ResurrectSession(ctx context.Context, id string) (*pb.Session, error)
	EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error)
}
