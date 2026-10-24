package session

import (
	"context"
	"os"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// Client is the daemon surface consumed by the session commands.
type Client interface {
	ListRepos(context.Context) ([]*pb.Repo, error)
	CreateSession(context.Context, *pb.CreateSessionRequest) (rpc.CreateSessionStream, error)
	GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error)
	ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error)
	ListSessionsWithReadFailures(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error)
	MergeSession(context.Context, string, string) (*pb.Session, string, error)
	ApproveSessionPR(context.Context, string, bool) (*pb.VerifyApproveResult, error)
	UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error)
	LinkSessionPR(context.Context, string, string) (*pb.LinkSessionPRResponse, error)
	SetSessionTracker(context.Context, string, string) (*pb.Session, error)
	RefreshSessionPR(context.Context, *pb.RefreshSessionPRRequest) (*pb.Session, error)
	ArchiveSession(context.Context, *pb.ArchiveSessionRequest) (*pb.ArchiveSessionResponse, error)
	ListChats(context.Context, string) ([]*pb.AgentChat, error)
	DescribeChatMCP(context.Context, string) (*pb.DescribeChatMCPResponse, error)
	SetChatPhase(context.Context, string, string, string) error
	FinishUnattendedRun(context.Context, *pb.FinishUnattendedRunRequest) (*pb.FinishUnattendedRunResponse, error)
	GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error)
	ListCheckSnapshots(context.Context, string, int32) (*pb.ListCheckSnapshotsResponse, error)
}

// LaunchOptions describes the initial TUI destination without coupling CLI commands to its models.
type LaunchOptions struct {
	NewSession      bool
	AttachSessionID string
	Agent           string
	Account         *string
}

// Options holds the session group's runtime seams.
type Options struct {
	LaunchTUI       func(*cobra.Command, LaunchOptions) error
	NewRemoteClient func(*cobra.Command, string) (Client, error)
	LoadSettings    func() (config.Settings, error)
	Getenv          func(string) string
}

// Group constructs the session command family with independent dependencies.
type Group struct {
	newClient cliutil.ClientFactory[Client]
	Options
}

// New constructs a session group whose client target is resolved at command execution.
func New(factory cliutil.ClientFactory[Client], opts Options) *Group {
	if opts.LoadSettings == nil {
		opts.LoadSettings = config.Load
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	return &Group{newClient: factory, Options: opts}
}
