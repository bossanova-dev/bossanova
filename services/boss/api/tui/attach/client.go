package attach

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/internal/attachprocess"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Client is the chat/session service consumed by the attach screen.
type Client interface {
	GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error)
	ListRepos(context.Context) ([]*pb.Repo, error)
	ListChats(context.Context, string) ([]*pb.AgentChat, error)
	RecordChat(context.Context, string, string, string, string, pb.ChatRole, bool) (*pb.AgentChat, error)
	UpdateChatTitle(context.Context, string, string) error
	DeleteChat(context.Context, string, pb.DeleteChatRequest_DeletionReason) error
	ReportChatStatus(context.Context, []*pb.ChatStatusReport) error
	DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error)
}

// Executor constructs the interactive process handed to Bubble Tea.
type Executor interface {
	Command(string, ...string) *attachprocess.Command
}
