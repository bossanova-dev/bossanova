package home

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Client is the daemon surface consumed by the home screen and its heartbeat.
type Client interface {
	ui.AttentionClient
	ListSessionsWithReadFailures(ctx context.Context, req *pb.ListSessionsRequest, opts rpc.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error)
	GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error)
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error)
	NotifyAuthChange(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error)
	ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error)
	UpdateNote(ctx context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error)
	ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error
	MoveSession(context.Context, *pb.MoveSessionRequest) (*pb.Session, bool, error)
}
