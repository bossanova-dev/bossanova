// Package upstream — adapters.go holds the concrete implementations
// that bridge the StreamClient's collaborator interfaces
// (SessionCommandHandler, WebhookDispatcher, SessionAttacher, snapshot
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

// CommandHandlerAdapter implements SessionCommandHandler by delegating
// to the daemon's existing lifecycle + session store + pause-is-a-flag
// update path. Kept as a struct with explicit dependency fields so
// cmd/main.go can wire narrow interfaces rather than pull the whole
// server package in.
type CommandHandlerAdapter struct {
	Lifecycle    LifecycleStopper
	Sessions     SessionReader
	Automation   AutomationToggler
	OnCompletion func(ctx context.Context, sessionID string) // optional, mirrors task orchestrator hook
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

// --- Webhook dispatcher (no-op stub) ---

// NoopWebhookDispatcher satisfies WebhookDispatcher but does nothing.
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

// AttachOutputLine mirrors claude.OutputLine without depending on the
// claude package — adapters.go would otherwise need an import cycle
// (claude has no reason to know about upstream, and tests prefer a
// small concrete type).
type AttachOutputLine struct {
	Text      string
	Timestamp time.Time
}

// ClaudeAttachReader is the slice of claude.Runner the attacher needs.
// Matching the subscribe + history surface lets the stream attach reuse
// the same in-process broadcaster the local socket AttachSession uses.
type ClaudeAttachReader interface {
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
	Claude   ClaudeAttachReader
	Logger   zerolog.Logger
}

// Attach implements SessionAttacher.Attach. It returns a channel of
// SessionAttachChunk events correlated to commandID. The caller owns
// ctx lifetime; the adapter closes the returned channel when the claude
// subscriber closes or ctx is cancelled.
func (a *SessionAttacherAdapter) Attach(ctx context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error) {
	if a.Sessions == nil || a.Claude == nil {
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
	if claudeSessionID == "" || !a.Claude.IsRunning(claudeSessionID) {
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
	sub, err := a.Claude.Subscribe(subCtx, claudeSessionID)
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
		for _, line := range a.Claude.History(claudeSessionID) {
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
