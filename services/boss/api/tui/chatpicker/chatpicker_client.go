package chatpicker

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Client is the operations consumed by session details and its attention actions.
type Client interface {
	GetSession(ctx context.Context, id string, opts rpc.SessionReadOptions) (*pb.Session, error)
	ListChats(ctx context.Context, sessionID string) ([]*pb.AgentChat, error)
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	ListAgents(ctx context.Context) ([]rpc.AgentInfo, error)
	GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
	UpdateChatTitle(ctx context.Context, agentSessionID, title string) error
	ArchiveSession(ctx context.Context, req *pb.ArchiveSessionRequest) (*pb.ArchiveSessionResponse, error)
	MergeSession(ctx context.Context, id, matchHead string) (*pb.Session, string, error)
	ListAgentAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.AgentAccount, error)
	SwitchSessionAgentAccount(ctx context.Context, req *pb.SwitchSessionAgentAccountRequest) (*pb.SwitchSessionAgentAccountResponse, error)
	WakeChat(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error)
	DeleteChat(ctx context.Context, agentSessionID string, reason pb.DeleteChatRequest_DeletionReason) error
	GetSessionTimeline(ctx context.Context, sessionID string) ([]*pb.TimelineStep, error)
	GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
	ListGitHubCallbacks(ctx context.Context, req *pb.ListGitHubCallbacksRequest) ([]*pb.GitHubCallback, error)
	DismissSessionAlert(ctx context.Context, sessionID, alertKey string) (*pb.Session, error)
	ResumeSessionRun(ctx context.Context, sessionID string) (*pb.Session, error)
	OperatorMergeSession(ctx context.Context, id, head string, approve bool, waives []string) (*pb.OperatorMergeSessionResponse, error)
}
