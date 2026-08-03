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
	"strings"
	"time"

	"connectrpc.com/connect"
	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
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

// CallbackInterestsFn returns the daemon's current GitHub callback-interest set
// as the proto projection used by the snapshot. The bossd wiring adapts
// callback.DeriveInterests over the callback store.
type CallbackInterestsFn func(ctx context.Context) ([]*pb.CallbackInterest, error)

type callbackInterestAdapter struct {
	fn CallbackInterestsFn
}

// NewCallbackInterestReader wraps a CallbackInterestsFn.
func NewCallbackInterestReader(fn CallbackInterestsFn) CallbackInterestReader {
	return &callbackInterestAdapter{fn: fn}
}

func (a *callbackInterestAdapter) SnapshotCallbackInterests(ctx context.Context) ([]*pb.CallbackInterest, error) {
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
	SetIsAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error
}

// SessionCommandServer is the slice of *server.Server the merge/archive/
// record-chat/delete-chat command paths need. Narrow interface avoids an
// import cycle with the server package (same pattern as ChatWaker).
type SessionCommandServer interface {
	MergeSession(context.Context, *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error)
	SwitchSessionAccount(context.Context, *connect.Request[pb.SwitchSessionAccountRequest]) (*connect.Response[pb.SwitchSessionAccountResponse], error)
	ArchiveSession(context.Context, *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error)
	RetrySession(context.Context, *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error)
	UpdateSession(context.Context, *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error)
	LinkSessionPR(context.Context, *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error)
	RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error)
	DeleteChat(context.Context, *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error)
	UpdateChatTitle(context.Context, *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error)
	ReportChatStatus(context.Context, *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error)
	ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error)
	ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error)
	ListAccounts(context.Context, *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error)
	GetRepoSettings(context.Context, *connect.Request[pb.GetRepoSettingsRequest]) (*connect.Response[pb.GetRepoSettingsResponse], error)
	UpdateRepo(context.Context, *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error)
	RemoveRepo(context.Context, *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error)
	ListRepoPRs(context.Context, *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error)
	ListTrackerIssues(context.Context, *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error)
	GetChatTranscript(context.Context, *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error)
	SendChatMessage(context.Context, *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error)
	CreateCronJob(context.Context, *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error)
	ListCronJobs(context.Context, *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error)
	UpdateCronJob(context.Context, *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error)
	DeleteCronJob(context.Context, *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error)
	RunCronJobNow(context.Context, *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error)
	CreateGithubCallback(context.Context, *connect.Request[pb.CreateGithubCallbackRequest]) (*connect.Response[pb.CreateGithubCallbackResponse], error)
	ListGithubCallbacks(context.Context, *connect.Request[pb.ListGithubCallbacksRequest]) (*connect.Response[pb.ListGithubCallbacksResponse], error)
	DeleteGithubCallback(context.Context, *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error)
	CreateNote(context.Context, *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error)
	GetNote(context.Context, *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error)
	ListNotes(context.Context, *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error)
	UpdateNote(context.Context, *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error)
	DeleteNote(context.Context, *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error)
	AddAccount(context.Context, *connect.Request[pb.AddAccountRequest]) (*connect.Response[pb.AddAccountResponse], error)
	RefreshAccount(context.Context, *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error)
	UpdateAccount(context.Context, *connect.Request[pb.UpdateAccountRequest]) (*connect.Response[pb.UpdateAccountResponse], error)
	RemoveAccount(context.Context, *connect.Request[pb.RemoveAccountRequest]) (*connect.Response[pb.RemoveAccountResponse], error)
	TestAccount(context.Context, *connect.Request[pb.TestAccountRequest]) (*connect.Response[pb.TestAccountResponse], error)
	ListChats(context.Context, *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error)
	GetSessionStatuses(context.Context, *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error)
	ListCheckSnapshots(context.Context, *connect.Request[pb.ListCheckSnapshotsRequest]) (*connect.Response[pb.ListCheckSnapshotsResponse], error)
	ListPlugins(context.Context, *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error)
	GetCronJob(context.Context, *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error)
	RepairDoctor(context.Context, *connect.Request[pb.RepairDoctorRequest]) (*connect.Response[pb.RepairDoctorResponse], error)
	CloseSession(context.Context, *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error)
	ResurrectSession(context.Context, *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error)
	RemoveSession(context.Context, *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error)
	EmptyTrash(context.Context, *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error)
}

// CommandHandlerAdapter implements SessionCommandHandler by delegating
// to the daemon's existing lifecycle + session store + pause-is-a-flag
// update path. Kept as a struct with explicit dependency fields so
// cmd/main.go can wire narrow interfaces rather than pull the whole
// server package in.
type CommandHandlerAdapter struct {
	Lifecycle  LifecycleStopper
	Sessions   SessionReader
	Automation AutomationToggler
	Waker      ChatWaker
	Commands   SessionCommandServer
	// Broadcasts materialises an inbound cross-daemon broadcast locally
	// (BOS-558). Nil when the daemon has no upstream, in which case no
	// BroadcastCommand can arrive to need it.
	Broadcasts   BroadcastReceiver
	OnCompletion func(ctx context.Context, sessionID string) // optional, mirrors task orchestrator hook
}

// BroadcastReceiver is the one method the inbound cross-daemon broadcast path
// needs from *broadcast.Ingress, declared here as a narrow interface for the
// same reason ChatWaker is: cmd/main.go plugs in the real ingress and a test
// scripts it, without either side depending on the whole type.
//
// It takes the DOMAIN value (broadcast.InboundBroadcast) rather than the raw
// *pb.BroadcastCommand. Importing services/bossd/internal/broadcast from here
// creates NO cycle — that package depends only on internal/db, internal/rotation
// and bossalib, all of which upstream already depends on, and nothing in it
// imports upstream (the ingress takes the daemon id as a plain string precisely
// so it does not have to). Given the choice is free, translating the wire form
// into the domain form HERE is the right seam: adapters.go is where every other
// command turns proto into a daemon call, it keeps cmd/main.go pure wiring, and
// it means the ingress — where the loop guard and idempotency live — never sees
// a proto type and so can never be tempted to reach back for one.
type BroadcastReceiver interface {
	Receive(ctx context.Context, in bcastsvc.InboundBroadcast) error
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
	if err := a.Automation.SetIsAutomationEnabled(ctx, sessionID, false); err != nil {
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

// SwitchAccount implements SessionCommandHandler.SwitchAccount by delegating
// to the daemon's SwitchSessionAccount RPC. An empty agentSessionID maps to
// nil (proto3 optional) so the daemon resolves the session's primary live chat;
// on error it classifies the connect code so the dispatcher can attach a typed
// CommandResult.error_code (mirrors the WakeChat / merge typed-code paths).
func (a *CommandHandlerAdapter) SwitchAccount(ctx context.Context, sessionID, agentSessionID, accountID string, force bool) (bool, string, string, pb.CommandResult_ErrorCode, error) {
	if sessionID == "" {
		return false, "", "", pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("switch_account: session_id required")
	}
	if a.Commands == nil {
		return false, "", "", pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("switch_account: command server not wired")
	}
	var agentSessionIDPtr *string
	if agentSessionID != "" {
		agentSessionIDPtr = proto.String(agentSessionID)
	}
	resp, err := a.Commands.SwitchSessionAccount(ctx, connect.NewRequest(&pb.SwitchSessionAccountRequest{
		SessionId:      sessionID,
		AccountId:      accountID,
		AgentSessionId: agentSessionIDPtr,
		Force:          force,
	}))
	if err != nil {
		return false, "", "", classifyCommandError(err), fmt.Errorf("switch session account: %w", err)
	}
	return resp.Msg.GetResumed(), resp.Msg.GetTargetLabel(), resp.Msg.GetNoticeText(), pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}

// Resume implements SessionCommandHandler.Resume by re-enabling automation.
func (a *CommandHandlerAdapter) Resume(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("resume: session_id required")
	}
	if a.Automation == nil {
		return nil, errors.New("resume: automation toggler not wired")
	}
	if err := a.Automation.SetIsAutomationEnabled(ctx, sessionID, true); err != nil {
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

// CloseSession implements SessionCommandHandler.CloseSession.
func (a *CommandHandlerAdapter) CloseSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("close: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("close: command server not wired")
	}
	resp, err := a.Commands.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("close session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// ResurrectSession implements SessionCommandHandler.ResurrectSession.
func (a *CommandHandlerAdapter) ResurrectSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("resurrect: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("resurrect: command server not wired")
	}
	resp, err := a.Commands.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("resurrect session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// RemoveSession implements SessionCommandHandler.RemoveSession.
func (a *CommandHandlerAdapter) RemoveSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("remove: session_id required")
	}
	if a.Commands == nil {
		return errors.New("remove: command server not wired")
	}
	if _, err := a.Commands.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: sessionID})); err != nil {
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}

// EmptyTrash implements SessionCommandHandler.EmptyTrash. olderThan is threaded
// straight through (nil = delete all archived sessions).
func (a *CommandHandlerAdapter) EmptyTrash(ctx context.Context, olderThan *timestamppb.Timestamp) (int32, error) {
	if a.Commands == nil {
		return 0, errors.New("empty_trash: command server not wired")
	}
	resp, err := a.Commands.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{OlderThan: olderThan}))
	if err != nil {
		return 0, fmt.Errorf("empty trash: %w", err)
	}
	return resp.Msg.GetDeletedCount(), nil
}

// RetrySession implements SessionCommandHandler.RetrySession by delegating to
// the daemon's RetrySession connect handler. The daemon's *connect.Error is
// wrapped with %w so classifyCommandError recovers its code.
func (a *CommandHandlerAdapter) RetrySession(ctx context.Context, sessionID string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("retry: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("retry: command server not wired")
	}
	resp, err := a.Commands.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("retry session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// UpdateSession implements SessionCommandHandler.UpdateSession by delegating to
// the daemon's UpdateSession connect handler. The optional title/tracker pointers
// ride through unchanged so an unset field stays unchanged server-side.
func (a *CommandHandlerAdapter) UpdateSession(ctx context.Context, req *pb.UpdateSessionCommand) (*pb.Session, error) {
	if req.GetSessionId() == "" {
		return nil, errors.New("update_session: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("update_session: command server not wired")
	}
	resp, err := a.Commands.UpdateSession(ctx, connect.NewRequest(&pb.UpdateSessionRequest{
		Id:         req.GetSessionId(),
		Title:      req.Title,
		TrackerUrl: req.TrackerUrl,
		TrackerId:  req.TrackerId,
	}))
	if err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}
	return resp.Msg.GetSession(), nil
}

// LinkSessionPR implements SessionCommandHandler.LinkSessionPR by delegating to
// the daemon's LinkSessionPR connect handler.
func (a *CommandHandlerAdapter) LinkSessionPR(ctx context.Context, sessionID, pr string) (*pb.Session, error) {
	if sessionID == "" {
		return nil, errors.New("link_session_pr: session_id required")
	}
	if a.Commands == nil {
		return nil, errors.New("link_session_pr: command server not wired")
	}
	resp, err := a.Commands.LinkSessionPR(ctx, connect.NewRequest(&pb.LinkSessionPRRequest{Id: sessionID, Pr: pr}))
	if err != nil {
		return nil, fmt.Errorf("link session pr: %w", err)
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

// UpdateChatTitle implements SessionCommandHandler.UpdateChatTitle.
func (a *CommandHandlerAdapter) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	if agentSessionID == "" {
		return errors.New("update_chat_title: agent_session_id required")
	}
	if a.Commands == nil {
		return errors.New("update_chat_title: command server not wired")
	}
	_, err := a.Commands.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          title,
	}))
	if err != nil {
		return fmt.Errorf("update chat title: %w", err)
	}
	return nil
}

// ReportChatStatus implements SessionCommandHandler.ReportChatStatus.
func (a *CommandHandlerAdapter) ReportChatStatus(ctx context.Context, reports []*pb.ChatStatusReport) error {
	if a.Commands == nil {
		return errors.New("report_chat_status: command server not wired")
	}
	_, err := a.Commands.ReportChatStatus(ctx, connect.NewRequest(&pb.ReportChatStatusRequest{Reports: reports}))
	if err != nil {
		return fmt.Errorf("report chat status: %w", err)
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

// ListAccounts implements SessionCommandHandler.ListAccounts by delegating to
// the daemon's ListAccounts connect handler and unwrapping the response message.
// An empty provider maps to nil (proto3 optional) so the daemon returns every
// account; a non-empty provider is passed through as the filter. refresh is
// forwarded the same way: false leaves the request's Refresh pointer nil (the
// exact pre-BOS-655 wire shape) and only true sets it, so an omitted/false
// request behaves identically to before the field existed.
func (a *CommandHandlerAdapter) ListAccounts(ctx context.Context, provider string, refresh bool) (*pb.ListAccountsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_accounts: command server not wired")
	}
	req := &pb.ListAccountsRequest{}
	if provider != "" {
		req.Provider = proto.String(provider)
	}
	if refresh {
		req.Refresh = proto.Bool(true)
	}
	resp, err := a.Commands.ListAccounts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return resp.Msg, nil
}

// GetRepo implements SessionCommandHandler.GetRepo through the daemon's
// existing web-safe settings handler.
func (a *CommandHandlerAdapter) GetRepo(ctx context.Context, repoID string) (*pb.GetRepoSettingsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("get_repo: command server not wired")
	}
	resp, err := a.Commands.GetRepoSettings(ctx, connect.NewRequest(&pb.GetRepoSettingsRequest{Id: repoID}))
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return resp.Msg, nil
}

// UpdateRepo implements SessionCommandHandler.UpdateRepo through the daemon's
// direct handler, translating only the enum merge strategy to its legacy
// storage string while preserving SecretUpdate fields exactly.
func (a *CommandHandlerAdapter) UpdateRepo(ctx context.Context, msg *pb.UpdateRepoCommand) (*pb.UpdateRepoResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("update_repo: command server not wired")
	}
	req := &pb.UpdateRepoRequest{
		Id:                              msg.GetRepoId(),
		DisplayName:                     msg.DisplayName,
		SetupScript:                     msg.SetupScript,
		CanAutoMerge:                    msg.CanAutoMerge,
		CanAutoMergeDependabot:          msg.CanAutoMergeDependabot,
		CanAutoRepair:                   msg.CanAutoRepair,
		ShouldArchiveSessionsAfterMerge: msg.ShouldArchiveSessionsAfterMerge,
		SentryOrg:                       msg.SentryOrg,
		LinearKey:                       msg.LinearKey,
		SentryKey:                       msg.SentryKey,
		ExpectedUpdatedAt:               msg.ExpectedUpdatedAt,
	}
	if msg.MergeStrategy != nil {
		strategy := "merge"
		switch msg.GetMergeStrategy() {
		case pb.MergeStrategy_MERGE_STRATEGY_UNSPECIFIED, pb.MergeStrategy_MERGE_STRATEGY_MERGE:
			// Both retain the direct handler's legacy merge default.
		case pb.MergeStrategy_MERGE_STRATEGY_REBASE:
			strategy = "rebase"
		case pb.MergeStrategy_MERGE_STRATEGY_SQUASH:
			strategy = "squash"
		}
		req.MergeStrategy = &strategy
	}
	resp, err := a.Commands.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("update repo: %w", err)
	}
	return resp.Msg, nil
}

// RemoveRepo implements SessionCommandHandler.RemoveRepo through the daemon's
// existing direct handler.
func (a *CommandHandlerAdapter) RemoveRepo(ctx context.Context, repoID string) error {
	if a.Commands == nil {
		return errors.New("remove_repo: command server not wired")
	}
	if _, err := a.Commands.RemoveRepo(ctx, connect.NewRequest(&pb.RemoveRepoRequest{Id: repoID})); err != nil {
		return fmt.Errorf("remove repo: %w", err)
	}
	return nil
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
func (a *CommandHandlerAdapter) SendChatMessage(ctx context.Context, agentSessionID, message string, wakeIfAsleep, submit bool) (*pb.SendChatMessageResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("send_chat_message: command server not wired")
	}
	resp, err := a.Commands.SendChatMessage(ctx, connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: agentSessionID,
		Message:        message,
		WakeIfAsleep:   wakeIfAsleep,
		Submit:         submit,
	}))
	if err != nil {
		return nil, fmt.Errorf("send chat message: %w", err)
	}
	return resp.Msg, nil
}

// ListCronJobs implements SessionCommandHandler.ListCronJobs by delegating to the
// daemon's ListCronJobs connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) ListCronJobs(ctx context.Context) (*pb.ListCronJobsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_cron_jobs: command server not wired")
	}
	resp, err := a.Commands.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list cron jobs: %w", err)
	}
	return resp.Msg, nil
}

// CreateCronJob implements SessionCommandHandler.CreateCronJob, translating the
// stream command into the daemon's CreateCronJobRequest field-for-field. The
// run_setup_command and is_zero_output optional bools are copied as pointers so
// their unset/true/false tri-state reaches the daemon unchanged.
func (a *CommandHandlerAdapter) CreateCronJob(ctx context.Context, cmd *pb.CreateCronJobCommand) (*pb.CreateCronJobResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("create_cron_job: command server not wired")
	}
	resp, err := a.Commands.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:                cmd.GetRepoId(),
		Name:                  cmd.GetName(),
		Prompt:                cmd.GetPrompt(),
		Schedule:              cmd.GetSchedule(),
		Timezone:              cmd.GetTimezone(),
		IsEnabled:             cmd.GetIsEnabled(),
		AgentName:             cmd.GetAgentName(),
		Model:                 cmd.GetModel(),
		GateCommand:           cmd.GetGateCommand(),
		ShouldRunSetupCommand: cmd.ShouldRunSetupCommand,
		IsZeroOutput:          cmd.IsZeroOutput,
	}))
	if err != nil {
		return nil, fmt.Errorf("create cron job: %w", err)
	}
	return resp.Msg, nil
}

// UpdateCronJob implements SessionCommandHandler.UpdateCronJob, translating the
// stream command into the daemon's UpdateCronJobRequest. Every mutable field is
// an optional pointer copied straight through so the daemon applies only the
// fields the caller set.
func (a *CommandHandlerAdapter) UpdateCronJob(ctx context.Context, cmd *pb.UpdateCronJobCommand) (*pb.UpdateCronJobResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("update_cron_job: command server not wired")
	}
	resp, err := a.Commands.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:                    cmd.GetId(),
		Name:                  cmd.Name,
		Prompt:                cmd.Prompt,
		Schedule:              cmd.Schedule,
		Timezone:              cmd.Timezone,
		IsEnabled:             cmd.IsEnabled,
		AgentName:             cmd.AgentName,
		Model:                 cmd.Model,
		GateCommand:           cmd.GateCommand,
		ShouldRunSetupCommand: cmd.ShouldRunSetupCommand,
		IsZeroOutput:          cmd.IsZeroOutput,
	}))
	if err != nil {
		return nil, fmt.Errorf("update cron job: %w", err)
	}
	return resp.Msg, nil
}

// DeleteCronJob implements SessionCommandHandler.DeleteCronJob. The daemon's
// DeleteCronJobResponse carries no payload, so the response is discarded.
func (a *CommandHandlerAdapter) DeleteCronJob(ctx context.Context, id string) error {
	if a.Commands == nil {
		return errors.New("delete_cron_job: command server not wired")
	}
	if _, err := a.Commands.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id})); err != nil {
		return fmt.Errorf("delete cron job: %w", err)
	}
	return nil
}

// CreateGithubCallback implements SessionCommandHandler.CreateGithubCallback,
// translating the stream command into the daemon's CreateGithubCallbackRequest
// field-for-field. GroupId and ExpiresAt are copied as pointers so their
// unset state reaches the daemon unchanged. The Message field is a secret and
// is never logged.
func (a *CommandHandlerAdapter) CreateGithubCallback(ctx context.Context, cmd *pb.CreateGithubCallbackCommand) (*pb.CreateGithubCallbackResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("create_github_callback: command server not wired")
	}
	resp, err := a.Commands.CreateGithubCallback(ctx, connect.NewRequest(&pb.CreateGithubCallbackRequest{
		GroupId:      cmd.GroupId,
		TargetChatId: cmd.GetTargetChatId(),
		RepoOwner:    cmd.GetRepoOwner(),
		RepoName:     cmd.GetRepoName(),
		PrNumber:     cmd.GetPrNumber(),
		Trigger:      cmd.GetTrigger(),
		Message:      cmd.GetMessage(),
		ExpiresAt:    cmd.ExpiresAt,
	}))
	if err != nil {
		return nil, fmt.Errorf("create github callback: %w", err)
	}
	return resp.Msg, nil
}

// ListGithubCallbacks implements SessionCommandHandler.ListGithubCallbacks,
// translating the stream command into the daemon's ListGithubCallbacksRequest.
// Every filter is an optional pointer copied straight through so the daemon
// applies only the filters the caller set.
func (a *CommandHandlerAdapter) ListGithubCallbacks(ctx context.Context, cmd *pb.ListGithubCallbacksCommand) (*pb.ListGithubCallbacksResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_github_callbacks: command server not wired")
	}
	resp, err := a.Commands.ListGithubCallbacks(ctx, connect.NewRequest(&pb.ListGithubCallbacksRequest{
		TargetChatId: cmd.TargetChatId,
		RepoOwner:    cmd.RepoOwner,
		RepoName:     cmd.RepoName,
		PrNumber:     cmd.PrNumber,
		Trigger:      cmd.Trigger,
		State:        cmd.State,
	}))
	if err != nil {
		return nil, fmt.Errorf("list github callbacks: %w", err)
	}
	return resp.Msg, nil
}

// DeleteGithubCallback implements SessionCommandHandler.DeleteGithubCallback.
// The daemon's DeleteGithubCallbackResponse carries no payload, so the response
// is discarded.
func (a *CommandHandlerAdapter) DeleteGithubCallback(ctx context.Context, id string) error {
	if a.Commands == nil {
		return errors.New("delete_github_callback: command server not wired")
	}
	if _, err := a.Commands.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: id})); err != nil {
		return fmt.Errorf("delete github callback: %w", err)
	}
	return nil
}

// CreateNote implements SessionCommandHandler.CreateNote (BOS-552), translating
// the stream command into the daemon's CreateNoteRequest field-for-field.
// SessionId and ChatId are copied as pointers so their unset state reaches the
// daemon unchanged — the store distinguishes "no provenance" from a blank one.
func (a *CommandHandlerAdapter) CreateNote(ctx context.Context, cmd *pb.CreateNoteCommand) (*pb.CreateNoteResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("create_note: command server not wired")
	}
	resp, err := a.Commands.CreateNote(ctx, connect.NewRequest(&pb.CreateNoteRequest{
		RepoId:    cmd.GetRepoId(),
		SessionId: cmd.SessionId,
		ChatId:    cmd.ChatId,
		Body:      cmd.GetBody(),
		Tags:      cmd.GetTags(),
	}))
	if err != nil {
		return nil, fmt.Errorf("create note: %w", err)
	}
	return resp.Msg, nil
}

// GetNote implements SessionCommandHandler.GetNote. An absent id surfaces as the
// daemon's NotFound, which classifyCommandError maps to ERROR_CODE_NOT_FOUND.
func (a *CommandHandlerAdapter) GetNote(ctx context.Context, cmd *pb.GetNoteCommand) (*pb.GetNoteResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("get_note: command server not wired")
	}
	resp, err := a.Commands.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: cmd.GetId()}))
	if err != nil {
		return nil, fmt.Errorf("get note: %w", err)
	}
	return resp.Msg, nil
}

// ListNotes implements SessionCommandHandler.ListNotes, translating the stream
// command into the daemon's ListNotesRequest. Every filter is an optional
// pointer copied straight through so the daemon applies only the filters the
// caller set; Limit is a plain int32 the daemon treats as unbounded when zero.
func (a *CommandHandlerAdapter) ListNotes(ctx context.Context, cmd *pb.ListNotesCommand) (*pb.ListNotesResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_notes: command server not wired")
	}
	resp, err := a.Commands.ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{
		RepoId:    cmd.RepoId,
		SessionId: cmd.SessionId,
		ChatId:    cmd.ChatId,
		Tags:      cmd.GetTags(),
		Search:    cmd.Search,
		Limit:     cmd.GetLimit(),
	}))
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	return resp.Msg, nil
}

// UpdateNote implements SessionCommandHandler.UpdateNote. Body and Tags are
// copied as pointers: an unset field leaves that part of the note alone, and a
// set-but-empty Tags wrapper means "clear the tags", so flattening either one
// would turn a clear into a silent no-op.
func (a *CommandHandlerAdapter) UpdateNote(ctx context.Context, cmd *pb.UpdateNoteCommand) (*pb.UpdateNoteResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("update_note: command server not wired")
	}
	resp, err := a.Commands.UpdateNote(ctx, connect.NewRequest(&pb.UpdateNoteRequest{
		Id:   cmd.GetId(),
		Body: cmd.Body,
		Tags: cmd.GetTags(),
	}))
	if err != nil {
		return nil, fmt.Errorf("update note: %w", err)
	}
	return resp.Msg, nil
}

// DeleteNote implements SessionCommandHandler.DeleteNote. The daemon's
// DeleteNoteResponse carries no payload, so the response is discarded (mirrors
// DeleteGithubCallback).
func (a *CommandHandlerAdapter) DeleteNote(ctx context.Context, id string) error {
	if a.Commands == nil {
		return errors.New("delete_note: command server not wired")
	}
	if _, err := a.Commands.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: id})); err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	return nil
}

// DeliverBroadcast implements SessionCommandHandler.DeliverBroadcast by turning
// the wire command into the ingress's domain value (BOS-558). It is a
// TRANSLATOR and nothing more — the loop guard, the idempotency probe and
// local-only resolution all live in broadcast.Ingress.Receive.
//
// Ids are passed through trimmed because the ingress compares origin_daemon_id
// against this daemon's persisted id: a stray space would defeat the loop guard.
//
// ABSENT EXPIRY IS A REJECTION, NOT A DEFAULT. An origin always stamps an
// absolute expires_at (SendBroadcast derives it from the callback duration
// parser, which never yields zero), so a missing one means a malformed or
// truncated command. The two tempting alternatives are both worse:
// timestamppb's nil AsTime() is the 1970 epoch, which would make every delivery
// instantly overdue with no error anywhere, and inventing a local default would
// give a routed broadcast a different lifetime than the one its sender asked
// for — exactly what carrying an absolute expiry exists to prevent. So we fail
// loudly, naming the field.
//
// This guard runs BEFORE the ingress's loop guard, which the contract says
// comes first. That ordering is safe and deliberate: a self-echo is by
// construction well-formed (this daemon stamped its expiry), so the check can
// only ever fire on a genuinely malformed command from elsewhere — one there is
// no domain value to build for at all.
//
// SECRET BODY: no error below interpolates cmd.message; error text from here
// travels back to bosso on CommandResult.error.
func (a *CommandHandlerAdapter) DeliverBroadcast(ctx context.Context, cmd *pb.BroadcastCommand) error {
	if cmd == nil {
		return errors.New("broadcast: command is required")
	}
	if a.Broadcasts == nil {
		return errors.New("broadcast: ingress not wired")
	}
	id := strings.TrimSpace(cmd.GetBroadcastId())
	// A zero-valued (as opposed to absent) timestamp decodes to the epoch, which
	// the ingress's own IsZero check cannot distinguish from "the caller sent
	// nothing", so the wire form is normalised to a zero time.Time HERE and the
	// ingress rejects it as the missing field it is. The ingress owns the rule
	// (it is exported and has other potential callers); this only stops the
	// decode from inventing 1970.
	var expiresAt time.Time
	if ts := cmd.GetExpiresAt(); ts != nil && ts.IsValid() && (ts.GetSeconds() != 0 || ts.GetNanos() != 0) {
		expiresAt = ts.AsTime().UTC()
	}
	err := a.Broadcasts.Receive(ctx, bcastsvc.InboundBroadcast{
		ID:             id,
		Selector:       bcast.SelectorFromProto(cmd.GetSelector()),
		OriginDaemonID: strings.TrimSpace(cmd.GetOriginDaemonId()),
		OriginChatID:   strings.TrimSpace(cmd.GetOriginChatId()),
		Message:        cmd.GetMessage(),
		ExpiresAt:      expiresAt,
	})
	// PERMANENT failures are typed so the router can drop rather than redeliver.
	// The stream is at-least-once, so an error that reads as "try again" WILL come
	// back: a malformed command or an over-cap selector reported the same way as
	// "database is locked" is a poison pill that retries forever. Both sentinels
	// here are deterministic in the command itself — the same bytes fail the same
	// way on every attempt — so they become InvalidArgument, which
	// classifyCommandError turns into a typed CommandResult.error_code. Everything
	// else stays untyped and therefore retryable, which is the safe default.
	//
	// SECRET BODY: connect.NewError wraps the ingress's own error text, which is
	// id-and-field only by construction; nothing here reaches for cmd.GetMessage().
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcastsvc.ErrInvalidInbound), errors.Is(err, bcastsvc.ErrTooManyTargets):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return err
	}
}

// RunCronJobNow implements SessionCommandHandler.RunCronJobNow by delegating to
// the daemon's RunCronJobNow connect handler.
func (a *CommandHandlerAdapter) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("run_cron_job_now: command server not wired")
	}
	resp, err := a.Commands.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{Id: id}))
	if err != nil {
		return nil, fmt.Errorf("run cron job now: %w", err)
	}
	return resp.Msg, nil
}

// AddAccount implements SessionCommandHandler.AddAccount, translating the stream
// command into the daemon's AddAccountRequest. The credential blob is inbound
// only — forwarded verbatim into the keyring by the daemon handler and never
// logged or echoed here.
func (a *CommandHandlerAdapter) AddAccount(ctx context.Context, cmd *pb.AddAccountCommand) (*pb.AddAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("add_account: command server not wired")
	}
	resp, err := a.Commands.AddAccount(ctx, connect.NewRequest(&pb.AddAccountRequest{
		Provider:   cmd.GetProvider(),
		Label:      cmd.GetLabel(),
		Priority:   cmd.GetPriority(),
		Credential: cmd.GetCredential(),
	}))
	if err != nil {
		return nil, fmt.Errorf("add account: %w", err)
	}
	return resp.Msg, nil
}

// RefreshAccount implements SessionCommandHandler.RefreshAccount, translating the
// stream command into the daemon's RefreshAccountRequest. The credential is
// inbound only; test_after_save is copied through so the daemon runs the smoke
// check when requested.
func (a *CommandHandlerAdapter) RefreshAccount(ctx context.Context, cmd *pb.RefreshAccountCommand) (*pb.RefreshAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("refresh_account: command server not wired")
	}
	resp, err := a.Commands.RefreshAccount(ctx, connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            cmd.GetId(),
		Credential:    cmd.GetCredential(),
		TestAfterSave: cmd.GetTestAfterSave(),
	}))
	if err != nil {
		return nil, fmt.Errorf("refresh account: %w", err)
	}
	return resp.Msg, nil
}

// UpdateAccount implements SessionCommandHandler.UpdateAccount, translating the
// stream command into the daemon's UpdateAccountRequest. The optional label /
// priority / status pointers are forwarded 1:1 so the daemon applies present-only
// semantics; allowed_models (repeated, no presence) is passed straight through.
func (a *CommandHandlerAdapter) UpdateAccount(ctx context.Context, cmd *pb.UpdateAccountCommand) (*pb.UpdateAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("update_account: command server not wired")
	}
	resp, err := a.Commands.UpdateAccount(ctx, connect.NewRequest(&pb.UpdateAccountRequest{
		Id:            cmd.GetId(),
		Label:         cmd.Label,
		Priority:      cmd.Priority,
		Status:        cmd.Status,
		AllowedModels: cmd.GetAllowedModels(),
	}))
	if err != nil {
		return nil, fmt.Errorf("update account: %w", err)
	}
	return resp.Msg, nil
}

// RemoveAccount implements SessionCommandHandler.RemoveAccount by delegating to
// the daemon's RemoveAccount connect handler. The RemoveAccountResponse carries
// no payload, so the response is discarded.
func (a *CommandHandlerAdapter) RemoveAccount(ctx context.Context, id string) error {
	if a.Commands == nil {
		return errors.New("remove_account: command server not wired")
	}
	if _, err := a.Commands.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: id})); err != nil {
		return fmt.Errorf("remove account: %w", err)
	}
	return nil
}

// TestAccount implements SessionCommandHandler.TestAccount by delegating to the
// daemon's TestAccount connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) TestAccount(ctx context.Context, cmd *pb.TestAccountCommand) (*pb.TestAccountResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("test_account: command server not wired")
	}
	resp, err := a.Commands.TestAccount(ctx, connect.NewRequest(&pb.TestAccountRequest{Id: cmd.GetId()}))
	if err != nil {
		return nil, fmt.Errorf("test account: %w", err)
	}
	return resp.Msg, nil
}

// ListChats implements SessionCommandHandler.ListChats by delegating to the
// daemon's ListChats connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) ListChats(ctx context.Context, sessionID string) (*pb.ListChatsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_chats: command server not wired")
	}
	resp, err := a.Commands.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{SessionId: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	return resp.Msg, nil
}

// GetSessionStatuses implements SessionCommandHandler.GetSessionStatuses by
// delegating to the daemon's GetSessionStatuses connect handler.
func (a *CommandHandlerAdapter) GetSessionStatuses(ctx context.Context, sessionIDs []string) (*pb.GetSessionStatusesResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("get_session_statuses: command server not wired")
	}
	resp, err := a.Commands.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{SessionIds: sessionIDs}))
	if err != nil {
		return nil, fmt.Errorf("get session statuses: %w", err)
	}
	return resp.Msg, nil
}

// ListCheckSnapshots implements SessionCommandHandler.ListCheckSnapshots by
// delegating to the daemon's ListCheckSnapshots connect handler.
func (a *CommandHandlerAdapter) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_check_snapshots: command server not wired")
	}
	resp, err := a.Commands.ListCheckSnapshots(ctx, connect.NewRequest(&pb.ListCheckSnapshotsRequest{SessionId: sessionID, Limit: limit}))
	if err != nil {
		return nil, fmt.Errorf("list check snapshots: %w", err)
	}
	return resp.Msg, nil
}

// ListPlugins implements SessionCommandHandler.ListPlugins by delegating to the
// daemon's ListPlugins connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) ListPlugins(ctx context.Context) (*pb.ListPluginsResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("list_plugins: command server not wired")
	}
	resp, err := a.Commands.ListPlugins(ctx, connect.NewRequest(&pb.ListPluginsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}
	return resp.Msg, nil
}

// GetCronJob implements SessionCommandHandler.GetCronJob by delegating to the
// daemon's GetCronJob connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) GetCronJob(ctx context.Context, id string) (*pb.GetCronJobResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("get_cron_job: command server not wired")
	}
	resp, err := a.Commands.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: id}))
	if err != nil {
		return nil, fmt.Errorf("get cron job: %w", err)
	}
	return resp.Msg, nil
}

// RepairDoctor implements SessionCommandHandler.RepairDoctor by delegating to
// the daemon's RepairDoctor connect handler and unwrapping the response message.
func (a *CommandHandlerAdapter) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	if a.Commands == nil {
		return nil, errors.New("repair_doctor: command server not wired")
	}
	resp, err := a.Commands.RepairDoctor(ctx, connect.NewRequest(&pb.RepairDoctorRequest{}))
	if err != nil {
		return nil, fmt.Errorf("repair doctor: %w", err)
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
		IsQuickChat:   cmd.GetIsQuickChat(),
		PrNumber:      cmd.PrNumber,
		BranchName:    cmd.BranchName,
		TrackerId:     cmd.TrackerId,
		TrackerUrl:    cmd.TrackerUrl,
		TrackerIssue:  cmd.TrackerIssue,
		TrackerSource: cmd.TrackerSource,
		AccountId:     cmd.AccountId,
		Force:         cmd.GetForce(),
		ForceBranch:   cmd.GetForceBranch(),
		// Unattended-session fields carried over the reverse stream so a hosted
		// create runs the same headless/unattended flow as a direct socket
		// create rather than starting interactive on the default model.
		Detach:           cmd.GetDetach(),
		Model:            cmd.Model,
		IsTmuxUnattended: cmd.GetIsTmuxUnattended(),
		// Carry defer_pr so the rebuilt request preserves the hosted create's
		// skip-up-front-draft-PR behavior (PR opened at finalize only if commits
		// land); dropping it here would silently re-enable the eager draft PR.
		DeferPr: cmd.GetDeferPr(),
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
				// Carry the dedup/attach signal upstream so the hosted create
				// path (bosso → mcp-gateway) can surface attached_existing
				// instead of hard-coding false.
				chunk.AttachedExisting = resp.GetSessionCreated().GetAttachedExisting()
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
