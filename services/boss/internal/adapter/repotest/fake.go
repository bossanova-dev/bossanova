// Package repotest provides repository RPC fakes shared by repo and skills tests.
package repotest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/crontest"
	"github.com/recurser/boss/internal/adapter/daemontest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RepoFake adds the repository and agent-list RPCs used by project and init commands.
type RepoFake struct {
	*crontest.CronFake
	mu                   sync.RWMutex
	socket               string
	repos                []*pb.Repo
	agents               []*pb.AgentInfo
	validateRepoPathResp *pb.ValidateRepoPathResponse
	registerRepoCalls    []*pb.RegisterRepoRequest
	updateRepoCalls      []*pb.UpdateRepoRequest
}

// New starts a fake for repo and skills command tests.
func New(t *testing.T) *RepoFake {
	t.Helper()
	f := &RepoFake{CronFake: crontest.NewHandler()}
	f.socket = daemontest.Serve(t, f)
	return f
}

// SocketPath returns the serving socket.
func (m *RepoFake) SocketPath() string { return m.socket }

// AddRepo supports the CLI consumer fixture.
func (m *RepoFake) AddRepo(repo *pb.Repo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos = append(m.repos, proto.CloneOf(repo))
}

// ListRepos supports the CLI consumer fixture.
func (m *RepoFake) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	repos := make([]*pb.Repo, 0, len(m.repos))
	for _, repo := range m.repos {
		repos = append(repos, proto.CloneOf(repo))
	}
	return connect.NewResponse(&pb.ListReposResponse{Repos: repos}), nil
}

// UpdateRepo supports the CLI consumer fixture.
func (m *RepoFake) UpdateRepo(_ context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateRepoCalls = append(m.updateRepoCalls, proto.CloneOf(req.Msg))
	for _, r := range m.repos {
		if r.Id != req.Msg.Id {
			continue
		}
		if req.Msg.DisplayName != nil {
			r.DisplayName = *req.Msg.DisplayName
		}
		if req.Msg.CanAutoMerge != nil {
			r.CanAutoMerge = *req.Msg.CanAutoMerge
		}
		if req.Msg.CanAutoMergeDependabot != nil {
			r.CanAutoMergeDependabot = *req.Msg.CanAutoMergeDependabot
		}
		if req.Msg.ShouldKeepBranchesCurrent != nil {
			r.ShouldKeepBranchesCurrent = *req.Msg.ShouldKeepBranchesCurrent
		}
		if req.Msg.IsTrusted != nil {
			r.IsTrusted = *req.Msg.IsTrusted
		}
		if req.Msg.CanAutoRepair != nil {
			r.CanAutoRepair = *req.Msg.CanAutoRepair
		}
		if req.Msg.MergeStrategy != nil {
			r.MergeStrategy = *req.Msg.MergeStrategy
		}
		if req.Msg.SetupScript != nil {
			r.SetupScript = req.Msg.SetupScript
		}
		applySecret := func(update *pb.SecretUpdate, dest *string) {
			if update == nil {
				return
			}
			switch update.Action {
			case pb.SecretAction_SECRET_ACTION_SET:
				value := update.GetValue()
				*dest = value
			case pb.SecretAction_SECRET_ACTION_CLEAR:
				*dest = ""
			case pb.SecretAction_SECRET_ACTION_UNSPECIFIED, pb.SecretAction_SECRET_ACTION_UNCHANGED:
				// Leave the existing secret unchanged.
			}
		}
		applySecret(req.Msg.LinearKey, &r.LinearApiKey)
		applySecret(req.Msg.SentryKey, &r.SentryApiKey)
		if req.Msg.SentryOrg != nil {
			value := *req.Msg.SentryOrg
			r.SentryOrg = value
		}
		r.UpdatedAt = timestamppb.Now()
		return connect.NewResponse(&pb.UpdateRepoResponse{Repo: proto.CloneOf(r)}), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

// ValidateRepoPath supports the CLI consumer fixture.
func (m *RepoFake) ValidateRepoPath(_ context.Context, _ *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	m.mu.RLock()
	resp := m.validateRepoPathResp
	m.mu.RUnlock()
	if resp != nil {
		return connect.NewResponse(resp), nil
	}
	return connect.NewResponse(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		DefaultBranch: "main",
	}), nil
}

// SetValidateRepoPathResult supports the CLI consumer fixture.
func (m *RepoFake) SetValidateRepoPathResult(resp *pb.ValidateRepoPathResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateRepoPathResp = resp
}

// RegisterRepo supports the CLI consumer fixture.
func (m *RepoFake) RegisterRepo(_ context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerRepoCalls = append(m.registerRepoCalls, proto.CloneOf(req.Msg))
	repo := &pb.Repo{
		Id:                fmt.Sprintf("repo-%d", len(m.repos)+1),
		DisplayName:       req.Msg.DisplayName,
		LocalPath:         req.Msg.LocalPath,
		DefaultBaseBranch: req.Msg.DefaultBaseBranch,
		WorktreeBaseDir:   req.Msg.WorktreeBaseDir,
		SetupScript:       req.Msg.SetupScript,
		IsTrusted:         req.Msg.GetIsTrusted(),
		CreatedAt:         timestamppb.Now(),
		UpdatedAt:         timestamppb.Now(),
	}
	m.repos = append(m.repos, proto.CloneOf(repo))
	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: proto.CloneOf(repo)}), nil
}

// RegisterRepoCalls supports the CLI consumer fixture.
func (m *RepoFake) RegisterRepoCalls() []*pb.RegisterRepoRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.RegisterRepoRequest, len(m.registerRepoCalls))
	for i, req := range m.registerRepoCalls {
		out[i] = proto.CloneOf(req)
	}
	return out
}

// UpdateRepoCalls supports the CLI consumer fixture.
func (m *RepoFake) UpdateRepoCalls() []*pb.UpdateRepoRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateRepoRequest, len(m.updateRepoCalls))
	for i, req := range m.updateRepoCalls {
		out[i] = proto.CloneOf(req)
	}
	return out
}

// ListAgents supports the CLI consumer fixture.
func (m *RepoFake) ListAgents(_ context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agents := make([]*pb.AgentInfo, len(m.agents))
	copy(agents, m.agents)
	return connect.NewResponse(&pb.ListAgentsResponse{Agents: agents}), nil
}

// SetAgents supports the CLI consumer fixture.
func (m *RepoFake) SetAgents(agents []*pb.AgentInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents = append([]*pb.AgentInfo(nil), agents...)
}
