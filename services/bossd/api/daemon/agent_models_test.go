package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

type modelListingAgent struct {
	agent.RunnerClient
	models []*pb.AgentModel
	err    error
}

func (a *modelListingAgent) ListModels(context.Context, *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &pb.ListModelsResponse{Models: a.models}, nil
}

type recentModelUsage struct {
	byAgent map[string][]string
	err     error
	limit   int
}

func (u *recentModelUsage) ListRecent(_ context.Context, agentName string, limit int) ([]string, error) {
	u.limit = limit
	return u.byAgent[agentName], u.err
}

func modelIDs(models []*pb.AgentModel) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.GetId())
	}
	return ids
}

func TestListAgentModelsReturnsAgentListAndRecentModels(t *testing.T) {
	usage := &recentModelUsage{byAgent: map[string][]string{"codex": {"gpt-6-luna", "gpt-6.2-sol"}, "claude": {"opus"}}}
	srv := New(Config{Logger: zerolog.Nop(), ModelUsage: usage, AgentClients: map[string]agent.RunnerClient{
		"codex": &modelListingAgent{models: []*pb.AgentModel{{Id: "gpt-6.2-sol", IsDefault: true}, {Id: "gpt-6-luna"}}},
	}})

	got, err := srv.ListAgentModels(context.Background(), "codex")
	if err != nil {
		t.Fatalf("ListAgentModels: %v", err)
	}
	if ids := modelIDs(got.GetModels()); !slices.Equal(ids, []string{"gpt-6.2-sol", "gpt-6-luna"}) {
		t.Fatalf("models = %v, want the agent's order", ids)
	}
	if !slices.Equal(got.GetRecentModelIds(), []string{"gpt-6-luna", "gpt-6.2-sol"}) {
		t.Fatalf("recent = %v, want codex's own history", got.GetRecentModelIds())
	}
	if usage.limit != recentAgentModelLimit {
		t.Fatalf("recent limit = %d, want %d", usage.limit, recentAgentModelLimit)
	}
}

func TestListAgentModelsTreatsAnAgentWithoutAListAsEmpty(t *testing.T) {
	usage := &recentModelUsage{byAgent: map[string][]string{"opencode": {"anthropic/opus"}}}
	for name, client := range map[string]agent.RunnerClient{
		"plugin answers Unimplemented": &modelListingAgent{err: status.Error(codes.Unimplemented, "unknown method ListModels")},
		"client lacks the capability":  &struct{ agent.RunnerClient }{},
	} {
		t.Run(name, func(t *testing.T) {
			srv := New(Config{Logger: zerolog.Nop(), ModelUsage: usage, AgentClients: map[string]agent.RunnerClient{"opencode": client}})
			got, err := srv.ListAgentModels(context.Background(), "opencode")
			if err != nil {
				t.Fatalf("ListAgentModels: %v", err)
			}
			if len(got.GetModels()) != 0 || !slices.Equal(got.GetRecentModelIds(), []string{"anthropic/opus"}) {
				t.Fatalf("result = %v, want no models and the recent history", got)
			}
		})
	}
}

func TestListAgentModelsErrors(t *testing.T) {
	srv := New(Config{Logger: zerolog.Nop(), AgentClients: map[string]agent.RunnerClient{
		"codex": &modelListingAgent{err: status.Error(codes.DeadlineExceeded, "app-server did not answer")},
	}})

	if _, err := srv.ListAgentModels(context.Background(), "nope"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown agent error = %v, want NotFound", err)
	}
	if _, err := srv.ListAgentModels(context.Background(), "codex"); err == nil {
		t.Fatal("a failing model list returned no error")
	}
}

func TestListAgentModelsKeepsModelsWhenHistoryFails(t *testing.T) {
	srv := New(Config{Logger: zerolog.Nop(), ModelUsage: &recentModelUsage{err: errors.New("database is locked")}, AgentClients: map[string]agent.RunnerClient{
		"claude": &modelListingAgent{models: []*pb.AgentModel{{IsDefault: true}, {Id: "opus"}}},
	}})
	got, err := srv.ListAgentModels(context.Background(), "claude")
	if err != nil {
		t.Fatalf("ListAgentModels: %v", err)
	}
	if len(got.GetModels()) != 2 || len(got.GetRecentModelIds()) != 0 {
		t.Fatalf("result = %v, want both models and no history", got)
	}
}
