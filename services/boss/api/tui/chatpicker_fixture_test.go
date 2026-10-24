package tui

import (
	"context"
	"sync"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type routerClientFixture struct {
	Client
	mu            sync.Mutex
	wakeChatCalls []wakeChatCall
	wakeResp      *pb.WakeChatResponse
	wakeErr       error
	session       *pb.Session
	repos         []*pb.Repo

	// deleteChatFixtured opts this stub into answering DeleteChat; see the
	// method. deleteChatCalls and lastDeleteReason then record what the picker
	// asked for, so a test can assert the explicit-delete key path states its
	// own reason instead of defaulting to UNSPECIFIED (BOS-1299).
	deleteChatFixtured bool
	deleteChatCalls    int
	lastDeleteReason   pb.DeleteChatRequest_DeletionReason

	// archiveResp opts this stub into answering ArchiveSession (BOS-1380);
	// lastArchiveReq records the request the picker sent.
	archiveResp    *pb.ArchiveSessionResponse
	lastArchiveReq *pb.ArchiveSessionRequest

	// Switch-account canned data (BOS-171). accounts is returned by
	// ListAgentAccounts; switchResp / switchErr drive SwitchSessionAgentAccount; and
	// switchCalls records the requests the TUI dispatched.
	accounts []*pb.AgentAccount

	// Inline rename (BOS-836). titleUpdates records every UpdateChatTitle the
	// picker dispatched; titleErr is the canned answer.
	titleUpdates []titleUpdateCall
	titleErr     error

	// Armed-callback context (BOS-1517). callbacks is what ListGitHubCallbacks
	// answers, filtered by TargetChatId; callbacksErr is the canned failure; and
	// callbackCalls records every request the picker dispatched, so a test can
	// assert the key-diffed fetch issued exactly the calls it should.
	callbacks     []*pb.GitHubCallback
	callbacksErr  error
	callbackCalls []*pb.ListGitHubCallbacksRequest
}

// titleUpdateCall is one recorded UpdateChatTitle RPC.
type titleUpdateCall struct {
	agentSessionID string
	title          string
}

type wakeChatCall struct {
	sessionID      string
	agentSessionID string
	forceFresh     bool
}

func (s *routerClientFixture) WakeChat(_ context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakeChatCalls = append(s.wakeChatCalls, wakeChatCall{
		sessionID:      sessionID,
		agentSessionID: agentSessionID,
		forceFresh:     forceFresh,
	})
	if s.wakeErr != nil {
		return nil, s.wakeErr
	}
	if s.wakeResp != nil {
		return s.wakeResp, nil
	}
	return &pb.WakeChatResponse{Outcome: pb.WakeChatResponse_OUTCOME_RESUMED}, nil
}

// GetChatStatuses must not panic — the model's refreshStatuses tick may
// call it; we just return nothing so the picker keeps the explicitly
// seeded statuses.
func (s *routerClientFixture) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return nil, nil
}

// GetSession must not panic — refreshStatuses calls it on every tick.
func (s *routerClientFixture) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	if s.session != nil {
		return s.session, nil
	}
	return &pb.Session{Id: "session-1"}, nil
}

// Unused interface methods — panic if called unexpectedly.

func (s *routerClientFixture) ListRepos(context.Context) ([]*pb.Repo, error) { return s.repos, nil }

func (s *routerClientFixture) ResumeSessionRun(context.Context, string) (*pb.Session, error) {
	panic("unused")
}

func (s *routerClientFixture) MergeSession(context.Context, string, string) (*pb.Session, string, error) {
	panic("unused")
}

func (s *routerClientFixture) OperatorMergeSession(context.Context, string, string, bool, []string) (*pb.OperatorMergeSessionResponse, error) {
	panic("unused")
}

func (s *routerClientFixture) ArchiveSession(_ context.Context, req *pb.ArchiveSessionRequest) (*pb.ArchiveSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archiveResp == nil {
		panic("unused")
	}
	s.lastArchiveReq = req
	return s.archiveResp, nil
}

func (s *routerClientFixture) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	panic("unused")
}

// UpdateChatTitle records the rename RPC the inline [r]ename prompt dispatches
// (BOS-836) and answers with titleErr, so a test can assert both what the TUI
// sent and how it recovers from a daemon refusal. It records rather than panics
// because the picker now has a key that reaches it.
func (s *routerClientFixture) UpdateChatTitle(_ context.Context, agentSessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.titleUpdates = append(s.titleUpdates, titleUpdateCall{agentSessionID: agentSessionID, title: title})
	return s.titleErr
}

// titleUpdateCalls returns a copy of the recorded rename RPCs.

// DeleteChat answers only when the stub was fixtured for it. Unfixtured it
// still panics rather than returning an empty success: a delete this test did
// not arrange for must fail loudly, not look like it worked.
func (s *routerClientFixture) DeleteChat(_ context.Context, _ string, reason pb.DeleteChatRequest_DeletionReason) error {
	if !s.deleteChatFixtured {
		panic("unused")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteChatCalls++
	s.lastDeleteReason = reason
	return nil
}

func (s *routerClientFixture) GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	panic("unused")
}

func (s *routerClientFixture) ListGitHubCallbacks(_ context.Context, req *pb.ListGitHubCallbacksRequest) ([]*pb.GitHubCallback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callbackCalls = append(s.callbackCalls, req)
	if s.callbacksErr != nil {
		return nil, s.callbacksErr
	}
	var out []*pb.GitHubCallback
	for _, cb := range s.callbacks {
		if req.TargetChatId != nil && cb.GetTargetChatId() != req.GetTargetChatId() {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

// listCallbackCalls returns a copy of the recorded ListGitHubCallbacks requests.

func (s *routerClientFixture) ListAgentAccounts(context.Context, string, bool) ([]*pb.AgentAccount, error) {
	return s.accounts, nil
}

func (s *routerClientFixture) GetSessionTimeline(context.Context, string) ([]*pb.TimelineStep, error) {
	return nil, nil
}
func (s *routerClientFixture) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	return nil, nil
}
