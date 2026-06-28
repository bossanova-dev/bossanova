// Package upstream — adapters.go holds the concrete implementations
// that bridge the StreamClient's collaborator interfaces
// (SessionCommandHandler, WebhookCommandDispatcher, SessionAttacher, snapshot
// readers) to the daemon's existing stores and lifecycle. Kept in the
// upstream package (rather than cmd/main.go) so the type signatures sit
// next to the interfaces they implement and unit tests can cover them.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProtoSessionLister lists sessions as proto. Matches the existing
// sessionListerAdapter signature in cmd/main.go — defined here so the
// snapshot reader adapter can reuse it without duplicating wiring.
type ProtoSessionLister interface {
	ListSessions(ctx context.Context) ([]*pb.Session, error)
}

// sessionSnapshotAdapter adapts a ProtoSessionLister to SessionSnapshotReader.
type sessionSnapshotAdapter struct {
	lister ProtoSessionLister
}

// NewSessionSnapshotReader wraps a ProtoSessionLister for the StreamClient
// snapshot path. Kept as a tiny adapter (rather than defining a new list
// method on the store) so the existing session lister used by the legacy
// Manager keeps working unchanged.
func NewSessionSnapshotReader(lister ProtoSessionLister) SessionSnapshotReader {
	return &sessionSnapshotAdapter{lister: lister}
}

func (a *sessionSnapshotAdapter) SnapshotSessions(ctx context.Context) ([]*pb.Session, error) {
	return a.lister.ListSessions(ctx)
}

// ChatListFn returns slim ClaudeChatMetadata protos — the per-chat
// projection used by the snapshot path. Wired via a function type
// (rather than an interface) so cmd/main.go can inline the join query
// without defining a new type for a one-call-site contract.
type ChatListFn func(ctx context.Context) ([]*pb.ClaudeChatMetadata, error)

// chatSnapshotAdapter adapts a ChatListFn to ChatSnapshotReader.
type chatSnapshotAdapter struct {
	fn ChatListFn
}

// NewChatSnapshotReader wraps a ChatListFn.
func NewChatSnapshotReader(fn ChatListFn) ChatSnapshotReader {
	return &chatSnapshotAdapter{fn: fn}
}

func (a *chatSnapshotAdapter) SnapshotChats(ctx context.Context) ([]*pb.ClaudeChatMetadata, error) {
	if a.fn == nil {
		return nil, nil
	}
	return a.fn(ctx)
}

// RepoIDsFn returns all repo IDs the daemon is managing. Used by the
// snapshot path; the full repo proto isn't sent.
type RepoIDsFn func(ctx context.Context) ([]string, error)

type repoSnapshotAdapter struct {
	fn RepoIDsFn
}

// NewRepoSnapshotReader wraps a RepoIDsFn.
func NewRepoSnapshotReader(fn RepoIDsFn) RepoSnapshotReader {
	return &repoSnapshotAdapter{fn: fn}
}

func (a *repoSnapshotAdapter) SnapshotRepoIDs(ctx context.Context) ([]string, error) {
	if a.fn == nil {
		return nil, nil
	}
	return a.fn(ctx)
}

// StatusEntriesFn returns the current chat-status set as the proto
// projection used by the snapshot.
type StatusEntriesFn func(ctx context.Context) ([]*pb.ChatStatusEntry, error)

type statusSnapshotAdapter struct {
	fn StatusEntriesFn
}

// NewStatusSnapshotReader wraps a StatusEntriesFn.
func NewStatusSnapshotReader(fn StatusEntriesFn) StatusSnapshotReader {
	return &statusSnapshotAdapter{fn: fn}
}

func (a *statusSnapshotAdapter) SnapshotStatuses(ctx context.Context) ([]*pb.ChatStatusEntry, error) {
	if a.fn == nil {
		return nil, nil
	}
	return a.fn(ctx)
}

// --- Command handler adapter ---

// LifecycleStopper is the slice of *session.Lifecycle the stop path
// needs. Keeping it as a narrow interface (rather than importing the
// whole session package) avoids an import cycle via db → upstream.
type LifecycleStopper interface {
	StopSession(ctx context.Context, sessionID string) error
}

// SessionReader fetches the post-action session row so the command
// result can echo the current state back.
type SessionReader interface {
	GetSession(ctx context.Context, id string) (*pb.Session, error)
}

// AutomationToggler flips the automation_enabled flag — pause/resume.
type AutomationToggler interface {
	SetAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error
}

// SessionCommandServer is the slice of *server.Server the merge/archive/
// record-chat/delete-chat command paths need. Narrow interface avoids an
// import cycle with the server package (same pattern as ChatWaker).
type SessionCommandServer interface {
	MergeSession(context.Context, *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error)
	ArchiveSession(context.Context, *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error)
	RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error)
	DeleteChat(context.Context, *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error)
	ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error)
	ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error)
	ListRepoPRs(context.Context, *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error)
	ListTrackerIssues(context.Context, *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error)
	GetChatTranscript(context.Context, *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error)
	SendChatMessage(context.Context, *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error)
}

// CommandHandlerAdapter implements SessionCommandHandler by delegating
// to the daemon's existing lifecycle + session store + pause-is-a-flag
// update path. Kept as a struct with explicit dependency fields so
// cmd/main.go can wire narrow interfaces rather than pull the whole
// server package in.
type CommandHandlerAdapter struct {
	Lifecycle    LifecycleStopper
	Sessions     SessionReader
	Automation   AutomationToggler
	Waker        ChatWaker
	Commands     SessionCommandServer
	OnCompletion func(ctx context.Context, sessionID string) // optional, mirrors task orchestrator hook
}

// ChatWaker is the slice of *server.Server the WakeChat path needs. The
// adapter takes it as a narrow interface so cmd/main.go can plug in the
// real server without dragging the whole server package into upstream
// (preserving the same import-cycle-avoidance pattern used by the other
// command handlers in this file).
type ChatWaker interface {
	// WakeChatStream returns the proto-level Outcome and the persisted
	// tmux session name plus a user-facing fallback reason when outcome is
	// OUTCOME_FRESH_FALLBACK. The errorCode classifies any failure so the
	// dispatcher can attach a typed CommandResult.error_code.
	WakeChatStream(ctx context.Context, agentSessionID string, forceFresh bool) (pb.WakeChatResult_Outcome, string, string, pb.CommandResult_ErrorCode, error)
}

// Stop implements SessionCommandHandler.Stop.
func (a *CommandHandlerAdapter) Stop(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("stop: session_id required")
	}
	if a.Lifecycle == nil {
		return nil, errors.New("stop: lifecycle not wired")
	}
	if err := a.Lifecycle.StopSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("stop session: %w", err)
	}
	if a.OnCompletion != nil {
		a.OnCompletion(ctx, sessionID)
	}
	if a.Sessions == nil {
		return nil, nil
	}
	return a.Sessions.GetSession(ctx, sessionID)
}

// Pause implements SessionCommandHandler.Pause by disabling automation.
func (a *CommandHandlerAdapter) Pause(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("pause: session_id required")
	}
	if a.Automation == nil {
		return nil, errors.New("pause: automation toggler not wired")
	}
	if err := a.Automation.SetAutomationEnabled(ctx, sessionID, false); err != nil {
		return nil, fmt.Errorf("pause session: %w", err)
	}
	if a.Sessions == nil {
		return nil, nil
	}
	return a.Sessions.GetSession(ctx, sessionID)
}

// WakeChat implements SessionCommandHandler.WakeChat by delegating to the
// configured ChatWaker (typically *server.Server).
func (a *CommandHandlerAdapter) WakeChat(ctx context.Context, agentSessionID string, forceFresh bool) (pb.WakeChatResult_Outcome, string, string, pb.CommandResult_ErrorCode, error) {
	if agentSessionID == "" {
		return pb.WakeChatResult_OUTCOME_UNSPECIFIED, "", "", pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("wake_chat: agent_session_id required")
	}
	if a.Waker == nil {
		return pb.WakeChatResult_OUTCOME_UNSPECIFIED, "", "", pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("wake_chat: waker not wired")
	}
	return a.Waker.WakeChatStream(ctx, agentSessionID, forceFresh)
}

// Resume implements SessionCommandHandler.Resume by re-enabling automation.
func (a *CommandHandlerAdapter) Resume(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("resume: session_id required")
	}
	if a.Automation == nil {
		return nil, errors.New("resume: automation toggler not wired")
	}
	if err := a.Automation.SetAutomationEnabled(ctx, sessionID, true); err != nil {
		return nil, fmt.Errorf("resume session: %w", err)
	}
	if a.Sessions == nil {
		return nil, nil
	}
	return a.Sessions.GetSession(ctx, sessionID)
}

// MergeSession implements SessionCommandHandler.MergeSession.
func (a *CommandHandlerAdapter) MergeSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("merge: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("merge: command server not wired")
	}
	resp, err := a.Commands.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("merge session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// ArchiveSession implements SessionCommandHandler.ArchiveSession.
func (a *CommandHandlerAdapter) ArchiveSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("archive: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("archive: command server not wired")
	}
	resp, err := a.Commands.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("archive session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// RecordChat implements SessionCommandHandler.RecordChat.
func (a *CommandHandlerAdapter) RecordChat(ctx context.Context, sessionID, agentSessionID, title string, resume bool, agentName string) (*pb.ClaudeChat, error) {
	if a.Commands == nil {
		return nil, errors.New("record_chat: command server not wired")
	}
	req := &pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		Title:          title,
		Resume:         resume,
	}
	if agentName != "" {
		req.AgentName = &agentName
	}
	resp, err := a.Commands.RecordChat(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("record chat: %w", err)
	}
	return resp.Msg.GetChat(), nil
}

// DeleteChat implements SessionCommandHandler.DeleteChat.
func (a *CommandHandlerAdapter) DeleteChat(ctx context.Context, sessionID, agentSessionID string) error {
	if a.Commands == nil {
		return errors.New("delete_chat: command server not wired")
	}
	_, err := a.Commands.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
		SessionId:      sessionID,
	}))
	if err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	return nil
}

// ListRepos implements SessionCommandHandler.ListRepos by delegating to the
// daemon's ListRepos connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) ListRepos(ctx context.Context) (*pb.ListReposResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_repos: command server not wired")
	}
	resp, err := a.Commands.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	return resp.Msg, nil
}

// ListAgents implements SessionCommandHandler.ListAgents by delegating to the
// daemon's ListAgents connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) ListAgents(ctx context.Context) (*pb.ListAgentsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_agents: command server not wired")
	}
	resp, err := a.Commands.ListAgents(ctx, connect.NewRequest(&pb.ListAgentsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	return resp.Msg, nil
}

// ListRepoPRs implements SessionCommandHandler.ListRepoPRs.
func (a *CommandHandlerAdapter) ListRepoPRs(ctx context.Context, repoID string) (*pb.ListRepoPRsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_repo_prs: command server not wired")
	}
	resp, err := a.Commands.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, fmt.Errorf("list repo prs: %w", err)
	}
	return resp.Msg, nil
}

// ListTrackerIssues implements SessionCommandHandler.ListTrackerIssues.
func (a *CommandHandlerAdapter) ListTrackerIssues(ctx context.Context, repoID, query string, source *string) (*pb.ListTrackerIssuesResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_tracker_issues: command server not wired")
	}
	resp, err := a.Commands.ListTrackerIssues(ctx, connect.NewRequest(&pb.ListTrackerIssuesRequest{
		RepoId: repoID, Query: query, Source: source,
	}))
	if err != nil {
		return nil, fmt.Errorf("list tracker issues: %w", err)
	}
	return resp.Msg, nil
}

// GetChatTranscript implements SessionCommandHandler.GetChatTranscript by
// delegating to the daemon's GetChatTranscript connect handler.
func (a *CommandHandlerAdapter) GetChatTranscript(ctx context.Context, sessionID, agentSessionID string, maxMessages int32) (*pb.GetChatTranscriptResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("get_chat_transcript: command server not wired")
	}
	resp, err := a.Commands.GetChatTranscript(ctx, connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: agentSessionID,
		SessionId:      sessionID,
		MaxMessages:    maxMessages,
	}))
	if err != nil {
		return nil, fmt.Errorf("get chat transcript: %w", err)
	}
	return resp.Msg, nil
}

// SendChatMessage implements SessionCommandHandler.SendChatMessage by delegating
// to the daemon's SendChatMessage connect handler.
func (a *CommandHandlerAdapter) SendChatMessage(ctx context.Context, agentSessionID, message string, wakeIfAsleep bool) (*pb.SendChatMessageResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("send_chat_message: command server not wired")
	}
	resp, err := a.Commands.SendChatMessage(ctx, connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: agentSessionID,
		Message:        message,
		WakeIfAsleep:   wakeIfAsleep,
	}))
	if err != nil {
		return nil, fmt.Errorf("send chat message: %w", err)
	}
	return resp.Msg, nil
}

// --- Webhook dispatcher (no-op stub) ---

// NoopWebhookDispatcher satisfies WebhookCommandDispatcher but does nothing.
// bossd has no in-daemon webhook subscriber today (webhooks flow
// directly into bosso); keeping this as a visible no-op keeps the
// interface satisfied and the WARN log makes it obvious when bosso
// starts dispatching webhooks the daemon hasn't been wired for.
type NoopWebhookDispatcher struct {
	Logger zerolog.Logger
}

// Dispatch logs and returns nil so bosso's command waiter resolves
// promptly (with ok=true via the ack path).
func (d *NoopWebhookDispatcher) Dispatch(_ context.Context, ev *pb.WebhookEvent) error {
	d.Logger.Warn().
		Str("event_type", ev.GetEventType()).
		Str("provider", ev.GetProvider()).
		Msg("webhook dispatcher not wired in bossd; dropping webhook")
	return nil
}

// --- Session attacher (tmux reader) ---

// AttachOutputLine mirrors agent.OutputLine without depending on the
// claude package — adapters.go would otherwise need an import cycle
// (claude has no reason to know about upstream, and tests prefer a
// small concrete type).
type AttachOutputLine struct {
	Text      string
	Timestamp time.Time
}

// AgentAttachReader is the slice of claude.Runner the attacher needs.
// Matching the subscribe + history surface lets the stream attach reuse
// the same in-process broadcaster the local socket AttachSession uses.
type AgentAttachReader interface {
	IsRunning(claudeSessionID string) bool
	History(claudeSessionID string) []AttachOutputLine
	Subscribe(ctx context.Context, claudeSessionID string) (<-chan AttachOutputLine, error)
}

// SessionAttachSessionLookup returns the claude session ID and current
// state for a bossd session ID. The adapter uses this to bounce straight
// to SessionEnded when no claude process is running.
type SessionAttachSessionLookup interface {
	LookupAttachTarget(ctx context.Context, sessionID string) (claudeSessionID string, state int32, err error)
}

// SessionAttacherAdapter implements SessionAttacher by running the same
// tmux-reader protocol the local AttachSession RPC already uses. The
// per-chunk event shapes (OutputLine / StateChange / SessionEnded) are
// borrowed directly from pb.SessionAttachChunk so the orchestrator can
// forward them verbatim to its own AttachSession proxy.
type SessionAttacherAdapter struct {
	Sessions SessionAttachSessionLookup
	Agent    AgentAttachReader
	Logger   zerolog.Logger
}

// Attach implements SessionAttacher.Attach. It returns a channel of
// SessionAttachChunk events correlated to commandID. The caller owns
// ctx lifetime; the adapter closes the returned channel when the claude
// subscriber closes or ctx is cancelled.
func (a *SessionAttacherAdapter) Attach(ctx context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error) {
	if a.Sessions == nil || a.Agent == nil {
		return nil, errors.New("attacher: dependencies not wired")
	}

	claudeSessionID, state, err := a.Sessions.LookupAttachTarget(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("lookup session: %w", err)
	}

	out := make(chan *pb.SessionAttachChunk, 64)

	// Initial StateChange so the consumer knows the session exists.
	initialChunk := &pb.SessionAttachChunk{
		SessionId: sessionID,
		CommandId: commandID,
		Event: &pb.SessionAttachChunk_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: pb.SessionState(state),
				NewState:      pb.SessionState(state),
			},
		},
	}

	// No live process — emit StateChange + SessionEnded and close.
	if claudeSessionID == "" || !a.Agent.IsRunning(claudeSessionID) {
		safego.Go(a.Logger, func() {
			defer close(out)
			select {
			case out <- initialChunk:
			case <-ctx.Done():
				return
			}
			endChunk := &pb.SessionAttachChunk{
				SessionId: sessionID,
				CommandId: commandID,
				Event: &pb.SessionAttachChunk_SessionEnded{
					SessionEnded: &pb.SessionEnded{FinalState: pb.SessionState(state)},
				},
			}
			select {
			case out <- endChunk:
			case <-ctx.Done():
			}
		})
		return out, nil
	}

	// Live process — subscribe and pump.
	subCtx, cancelSub := context.WithCancel(ctx)
	sub, err := a.Agent.Subscribe(subCtx, claudeSessionID)
	if err != nil {
		cancelSub()
		close(out)
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	safego.Go(a.Logger, func() {
		defer close(out)
		defer cancelSub()

		// 1. StateChange first.
		select {
		case out <- initialChunk:
		case <-ctx.Done():
			return
		}

		// 2. Replay history.
		for _, line := range a.Agent.History(claudeSessionID) {
			chunk := &pb.SessionAttachChunk{
				SessionId: sessionID,
				CommandId: commandID,
				Event: &pb.SessionAttachChunk_OutputLine{
					OutputLine: &pb.OutputLine{
						Text:      line.Text,
						Timestamp: timestamppb.New(line.Timestamp),
					},
				},
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}

		// 3. Live tail.
		for line := range sub {
			chunk := &pb.SessionAttachChunk{
				SessionId: sessionID,
				CommandId: commandID,
				Event: &pb.SessionAttachChunk_OutputLine{
					OutputLine: &pb.OutputLine{
						Text:      line.Text,
						Timestamp: timestamppb.New(line.Timestamp),
					},
				},
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}

		// 4. Subscriber closed → process exited. Look up final state.
		_, finalState, _ := a.Sessions.LookupAttachTarget(ctx, sessionID)
		endChunk := &pb.SessionAttachChunk{
			SessionId: sessionID,
			CommandId: commandID,
			Event: &pb.SessionAttachChunk_SessionEnded{
				SessionEnded: &pb.SessionEnded{FinalState: pb.SessionState(finalState)},
			},
		}
		select {
		case out <- endChunk:
		case <-ctx.Done():
		}
	})

	return out, nil
}

// --- Session creator (streaming CreateSession) ---

// StreamCreateSessioner is the slice of *server.Server the creator adapter
// needs. Narrow interface keeps upstream free of an import cycle with the
// server package (same pattern as ChatWaker / SessionCommandServer). It is
// satisfied by *server.Server.StreamCreateSession.
type StreamCreateSessioner interface {
	StreamCreateSession(ctx context.Context, msg *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error
}

// SessionCreatorAdapter implements SessionCreator by driving the daemon's
// extracted StreamCreateSession core and translating each
// *pb.CreateSessionResponse into a SessionCreateChunk correlated by
// commandID. On a non-nil StreamCreateSession error it emits a terminal
// CreateError chunk. The returned channel is always closed when done.
type SessionCreatorAdapter struct {
	Server StreamCreateSessioner
	Logger zerolog.Logger
}

// Create implements SessionCreator.Create. It builds a CreateSessionRequest
// from the command, runs StreamCreateSession in a goroutine, and pumps
// translated chunks onto the returned channel.
func (a *SessionCreatorAdapter) Create(ctx context.Context, cmd *pb.CreateSessionCommand, commandID string) (<-chan *pb.SessionCreateChunk, error) {
	if a.Server == nil {
		return nil, errors.New("creator: server not wired")
	}

	req := &pb.CreateSessionRequest{
		RepoId:        cmd.GetRepoId(),
		Title:         cmd.GetTitle(),
		Plan:          cmd.GetPlan(),
		BaseBranch:    cmd.GetBaseBranch(),
		QuickChat:     cmd.GetQuickChat(),
		PrNumber:      cmd.PrNumber,
		BranchName:    cmd.BranchName,
		TrackerId:     cmd.TrackerId,
		TrackerUrl:    cmd.TrackerUrl,
		TrackerIssue:  cmd.TrackerIssue,
		TrackerSource: cmd.TrackerSource,
	}
	if name := cmd.GetAgentName(); name != "" {
		req.AgentName = &name
	}

	out := make(chan *pb.SessionCreateChunk, 64)

	// safego.Go returns a done channel; intentionally not awaited here — the
	// goroutine owns the channel lifecycle and closes `out` when it finishes
	// (mirrors SessionAttacherAdapter.Attach).
	_ = safego.Go(a.Logger, func() {
		defer close(out)

		emit := func(resp *pb.CreateSessionResponse) error {
			chunk := &pb.SessionCreateChunk{CommandId: commandID}
			switch {
			case resp.GetSetupOutput() != nil:
				chunk.Body = &pb.SessionCreateChunk_SetupOutput{SetupOutput: resp.GetSetupOutput().GetText()}
			case resp.GetSessionCreated() != nil:
				chunk.Body = &pb.SessionCreateChunk_Created{Created: resp.GetSessionCreated().GetSession()}
			default:
				// Unknown response variant — skip rather than push an empty
				// chunk.
				return nil
			}
			select {
			case out <- chunk:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := a.Server.StreamCreateSession(ctx, req, emit); err != nil {
			terminal := &pb.SessionCreateChunk{
				CommandId: commandID,
				Body:      &pb.SessionCreateChunk_Error{Error: &pb.CreateError{Message: err.Error()}},
			}
			select {
			case out <- terminal:
			case <-ctx.Done():
			}
		}
	})

	return out, nil
}

// --- Daemon registration helper ---

// Register calls RegisterDaemon on the given client and returns the
// session token for the daemon. Intended to be invoked once at bossd
// startup before the StreamClient.Run loop kicks in. Callers pass the
// WorkOS JWT as bearer; on success the returned session_token is what
// bosso will verify on subsequent DaemonStream opens.
func Register(
	ctx context.Context,
	client bossanovav1connect.OrchestratorServiceClient,
	daemonID, hostname, userJWT string,
	repoIDs []string,
) (sessionToken string, err error) {
	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: daemonID,
		Hostname: hostname,
		RepoIds:  repoIDs,
	})
	if userJWT != "" {
		req.Header().Set("Authorization", "Bearer "+userJWT)
	}
	resp, err := client.RegisterDaemon(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Msg.SessionToken, nil
}
